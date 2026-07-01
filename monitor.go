package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

/*
	monitor.go

	The monitoring engine. Each enabled monitor runs in its own goroutine with
	a ticker at its interval. State transitions (up<->down) and cert-expiry
	threshold crossings fan out to the notifier.
*/

// monitorState tracks runtime info for state-change detection
type monitorState struct {
	hasState     bool
	lastUp       bool
	certNotified bool // already warned about cert expiry in current cycle
}

// Engine owns the running monitors
type Engine struct {
	cfg      *ConfigManager
	store    *Store
	notifier *Notifier

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	states  map[string]*monitorState
}

// NewEngine creates an engine
func NewEngine(cfg *ConfigManager, store *Store, notifier *Notifier) *Engine {
	return &Engine{
		cfg:      cfg,
		store:    store,
		notifier: notifier,
		cancels:  make(map[string]context.CancelFunc),
		states:   make(map[string]*monitorState),
	}
}

// Start launches goroutines for all enabled monitors
func (e *Engine) Start() {
	var mons []*Monitor
	e.cfg.Read(func(c *Config) {
		mons = append(mons, c.Monitors...)
	})
	for _, m := range mons {
		if m.Enabled {
			e.startMonitor(m)
		}
	}
}

// StopAll cancels every running monitor
func (e *Engine) StopAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, cancel := range e.cancels {
		cancel()
		delete(e.cancels, id)
	}
}

// Reload restarts a single monitor by id (used after add/edit/toggle).
// If the monitor no longer exists or is disabled, it is just stopped.
func (e *Engine) Reload(id string) {
	e.stopMonitor(id)
	var found *Monitor
	e.cfg.Read(func(c *Config) {
		for _, m := range c.Monitors {
			if m.ID == id {
				found = m
				return
			}
		}
	})
	if found != nil && found.Enabled {
		e.startMonitor(found)
	}
}

func (e *Engine) stopMonitor(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if cancel, ok := e.cancels[id]; ok {
		cancel()
		delete(e.cancels, id)
	}
}

func (e *Engine) startMonitor(m *Monitor) {
	e.mu.Lock()
	if _, running := e.cancels[m.ID]; running {
		e.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.cancels[m.ID] = cancel
	if e.states[m.ID] == nil {
		e.states[m.ID] = &monitorState{}
	}
	e.mu.Unlock()

	interval := m.Interval
	if interval < MinInterval {
		interval = MinInterval
	}

	go func(id string, every time.Duration) {
		// immediate first check, then on the ticker
		e.tick(id)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				e.tick(id)
			}
		}
	}(m.ID, time.Duration(interval)*time.Second)
}

// tick performs one check for a monitor id, persists the heartbeat, and
// dispatches notifications on state changes.
func (e *Engine) tick(id string) {
	// fetch a fresh copy of the monitor each tick (so edits take effect)
	var m *Monitor
	e.cfg.Read(func(c *Config) {
		for _, x := range c.Monitors {
			if x.ID == id {
				cp := *x
				m = &cp
				return
			}
		}
	})
	if m == nil {
		return
	}

	res := runCheck(m)
	hb := Heartbeat{
		Time:       time.Now().UnixMilli(),
		Up:         res.Up,
		Latency:    res.Latency,
		Msg:        res.Msg,
		CertExpiry: res.CertExpiry,
	}
	e.store.Add(id, hb)

	e.mu.Lock()
	st := e.states[id]
	if st == nil {
		st = &monitorState{}
		e.states[id] = st
	}
	transitioned := false
	wasUp := st.lastUp
	if !st.hasState || st.lastUp != res.Up {
		transitioned = st.hasState // don't notify on the very first check
		st.hasState = true
		st.lastUp = res.Up
	}
	// cert-expiry warning evaluation
	certWarn := false
	if res.CertExpiry > 0 && m.CertWarnDays > 0 {
		daysLeft := time.Until(time.UnixMilli(res.CertExpiry)).Hours() / 24
		if daysLeft <= float64(m.CertWarnDays) {
			if !st.certNotified {
				certWarn = true
				st.certNotified = true
			}
		} else {
			st.certNotified = false
		}
	}
	e.mu.Unlock()

	if transitioned && e.notifier != nil {
		e.notifier.Dispatch(m, res, wasUp)
	}
	if certWarn && e.notifier != nil {
		daysLeft := int(time.Until(time.UnixMilli(res.CertExpiry)).Hours() / 24)
		e.notifier.DispatchCert(m, daysLeft)
	}
}

// CurrentUp reports the last known up/down state of a monitor (and whether known)
func (e *Engine) CurrentUp(id string) (up bool, known bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.states[id]
	if st == nil || !st.hasState {
		return false, false
	}
	return st.lastUp, true
}

// debug helper
func (e *Engine) logf(format string, a ...interface{}) {
	fmt.Printf("[uptime-neko] "+format+"\n", a...)
}
