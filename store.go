package main

import (
	"encoding/binary"
	"encoding/json"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

/*
	store.go

	Heartbeat persistence.

	- bbolt: one bucket per monitor (named by monitor ID). Key = 8-byte
	  big-endian unix-milliseconds, value = JSON Heartbeat. Range scans are
	  ordered by time, which makes uptime / latency windows cheap.
	- In-memory ring buffer: last RingSize heartbeats per monitor for fast
	  dashboard rendering and sparklines without touching disk.
*/

const (
	DB_PATH  = "./neko.db"
	RingSize = 120 // heartbeats kept in memory per monitor
)

// Heartbeat is a single check result
type Heartbeat struct {
	Time       int64   `json:"time"`        // unix milliseconds
	Up         bool    `json:"up"`          // true = up, false = down
	Latency    float64 `json:"latency"`     // milliseconds
	Msg        string  `json:"msg"`         // status message / error
	CertExpiry int64   `json:"cert_expiry"` // unix ms of cert NotAfter, 0 if n/a
}

// Store wraps the bbolt DB and the per-monitor ring buffer
type Store struct {
	db   *bolt.DB
	mu   sync.RWMutex
	ring map[string][]Heartbeat
}

func itob(v int64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(v))
	return b
}

// OpenStore opens (or creates) the heartbeat database
func OpenStore(path string) (*Store, error) {
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, err
	}
	return &Store{db: db, ring: make(map[string][]Heartbeat)}, nil
}

// Close closes the database
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// WarmRing loads the last RingSize heartbeats for each known monitor into memory
func (s *Store) WarmRing(monitorIDs []string) {
	for _, id := range monitorIDs {
		beats := s.tailFromDB(id, RingSize)
		s.mu.Lock()
		s.ring[id] = beats
		s.mu.Unlock()
	}
}

// tailFromDB returns up to n most-recent heartbeats (ascending order)
func (s *Store) tailFromDB(monitorID string, n int) []Heartbeat {
	var out []Heartbeat
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(monitorID))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		// iterate backwards collecting up to n, then reverse
		for k, v := c.Last(); k != nil && len(out) < n; k, v = c.Prev() {
			var hb Heartbeat
			if json.Unmarshal(v, &hb) == nil {
				out = append(out, hb)
			}
		}
		return nil
	})
	// reverse to ascending
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Add records a heartbeat to both the DB and the ring buffer
func (s *Store) Add(monitorID string, hb Heartbeat) {
	_ = s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(monitorID))
		if err != nil {
			return err
		}
		data, err := json.Marshal(hb)
		if err != nil {
			return err
		}
		return b.Put(itob(hb.Time), data)
	})

	s.mu.Lock()
	r := append(s.ring[monitorID], hb)
	if len(r) > RingSize {
		r = r[len(r)-RingSize:]
	}
	s.ring[monitorID] = r
	s.mu.Unlock()
}

// Recent returns the in-memory ring (ascending). Caller must not mutate.
func (s *Store) Recent(monitorID string) []Heartbeat {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.ring[monitorID]
	out := make([]Heartbeat, len(src))
	copy(out, src)
	return out
}

// Range returns heartbeats with Time >= since (unix ms), ascending
func (s *Store) Range(monitorID string, since int64) []Heartbeat {
	var out []Heartbeat
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(monitorID))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.Seek(itob(since)); k != nil; k, v = c.Next() {
			var hb Heartbeat
			if json.Unmarshal(v, &hb) == nil {
				out = append(out, hb)
			}
		}
		return nil
	})
	return out
}

// Uptime returns the fraction (0..1) of "up" heartbeats since the given time.
// Returns -1 when there is no data.
func (s *Store) Uptime(monitorID string, since int64) float64 {
	beats := s.Range(monitorID, since)
	if len(beats) == 0 {
		return -1
	}
	up := 0
	for _, hb := range beats {
		if hb.Up {
			up++
		}
	}
	return float64(up) / float64(len(beats))
}

// AvgLatency returns mean latency (ms) of up heartbeats since the given time.
func (s *Store) AvgLatency(monitorID string, since int64) float64 {
	beats := s.Range(monitorID, since)
	var sum float64
	var n int
	for _, hb := range beats {
		if hb.Up {
			sum += hb.Latency
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// Prune deletes heartbeats older than retentionDays across all buckets
func (s *Store) Prune(retentionDays int) {
	cutoff := time.Now().Add(-time.Duration(retentionDays) * 24 * time.Hour).UnixMilli()
	_ = s.db.Update(func(tx *bolt.Tx) error {
		return tx.ForEach(func(name []byte, b *bolt.Bucket) error {
			c := b.Cursor()
			var keys [][]byte
			for k, _ := c.First(); k != nil; k, _ = c.Next() {
				if binary.BigEndian.Uint64(k) >= uint64(cutoff) {
					break
				}
				kk := make([]byte, len(k))
				copy(kk, k)
				keys = append(keys, kk)
			}
			for _, k := range keys {
				_ = b.Delete(k)
			}
			return nil
		})
	})
}

// DropMonitor removes all data for a monitor (DB bucket + ring)
func (s *Store) DropMonitor(monitorID string) {
	_ = s.db.Update(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte(monitorID)) != nil {
			return tx.DeleteBucket([]byte(monitorID))
		}
		return nil
	})
	s.mu.Lock()
	delete(s.ring, monitorID)
	s.mu.Unlock()
}
