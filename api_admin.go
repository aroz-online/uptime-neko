package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"
)

/*
	api_admin.go

	REST handlers for the configuration UI (served through Zoraxy's authenticated
	admin panel). All endpoints are mounted under the plugin UI path.
*/

// MonitorSummary is the per-monitor payload for the dashboard
type MonitorSummary struct {
	*Monitor
	CurrentUp    bool        `json:"current_up"`
	Known        bool        `json:"known"`
	LastMessage  string      `json:"last_message"`
	LastLatency  float64     `json:"last_latency"`
	AvgPing24h   float64     `json:"avg_ping_24h"`
	Uptime24h    float64     `json:"uptime_24h"`  // -1 if unknown
	Uptime30d    float64     `json:"uptime_30d"`  // -1 if unknown
	CertExpiry   int64       `json:"cert_expiry"` // unix ms, 0 if n/a
	Heartbeats   []Heartbeat `json:"heartbeats"`  // recent ring for sparkline
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func writeOK(w http.ResponseWriter) {
	writeJSON(w, map[string]bool{"ok": true})
}

func since(d time.Duration) int64 {
	return time.Now().Add(-d).UnixMilli()
}

// --- Dashboard summary ---

func handleSummary(w http.ResponseWriter, r *http.Request) {
	var monitors []*Monitor
	app.cfg.Read(func(c *Config) {
		monitors = append(monitors, c.Monitors...)
	})

	out := make([]MonitorSummary, 0, len(monitors))
	for _, m := range monitors {
		ring := app.store.Recent(m.ID)
		up, known := app.engine.CurrentUp(m.ID)
		s := MonitorSummary{
			Monitor:    m,
			CurrentUp:  up,
			Known:      known,
			Uptime24h:  app.store.Uptime(m.ID, since(24*time.Hour)),
			Uptime30d:  app.store.Uptime(m.ID, since(30*24*time.Hour)),
			AvgPing24h: app.store.AvgLatency(m.ID, since(24*time.Hour)),
			Heartbeats: ring,
		}
		if len(ring) > 0 {
			last := ring[len(ring)-1]
			s.LastMessage = last.Msg
			s.LastLatency = last.Latency
			s.CertExpiry = last.CertExpiry
		}
		out = append(out, s)
	}
	writeJSON(w, out)
}

// --- Monitor CRUD ---

func handleMonitorList(w http.ResponseWriter, r *http.Request) {
	var monitors []*Monitor
	app.cfg.Read(func(c *Config) {
		monitors = append(monitors, c.Monitors...)
	})
	writeJSON(w, monitors)
}

func decodeMonitor(r *http.Request) (*Monitor, error) {
	var m Monitor
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		return nil, err
	}
	if m.Interval < MinInterval {
		m.Interval = MinInterval
	}
	if m.Timeout <= 0 {
		m.Timeout = 10
	}
	return &m, nil
}

func handleMonitorAdd(w http.ResponseWriter, r *http.Request) {
	m, err := decodeMonitor(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if m.Name == "" || m.Target == "" {
		writeErr(w, http.StatusBadRequest, "name and target are required")
		return
	}
	m.ID = newID()
	if err := app.cfg.Update(func(c *Config) {
		c.Monitors = append(c.Monitors, m)
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if m.Enabled {
		app.engine.Reload(m.ID)
	}
	writeJSON(w, m)
}

func handleMonitorUpdate(w http.ResponseWriter, r *http.Request) {
	m, err := decodeMonitor(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if m.ID == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	found := false
	err = app.cfg.Update(func(c *Config) {
		for i, x := range c.Monitors {
			if x.ID == m.ID {
				c.Monitors[i] = m
				found = true
				return
			}
		}
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "monitor not found")
		return
	}
	app.engine.Reload(m.ID)
	writeJSON(w, m)
}

func handleMonitorDelete(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	app.engine.stopMonitor(id)
	_ = app.cfg.Update(func(c *Config) {
		out := c.Monitors[:0]
		for _, m := range c.Monitors {
			if m.ID != id {
				out = append(out, m)
			}
		}
		c.Monitors = out
		// also drop from any status pages
		for _, sp := range c.StatusPages {
			sp.MonitorIDs = removeString(sp.MonitorIDs, id)
		}
	})
	app.store.DropMonitor(id)
	writeOK(w)
}

func handleMonitorToggle(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	_ = app.cfg.Update(func(c *Config) {
		for _, m := range c.Monitors {
			if m.ID == id {
				m.Enabled = !m.Enabled
			}
		}
	})
	app.engine.Reload(id)
	writeOK(w)
}

// handleMonitorDetail returns the monitor plus heartbeats over a time range
// (for the response-time chart). ?id=&range=6h|24h|7d|30d
func handleMonitorDetail(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	var m *Monitor
	app.cfg.Read(func(c *Config) {
		for _, x := range c.Monitors {
			if x.ID == id {
				m = x
				return
			}
		}
	})
	if m == nil {
		writeErr(w, http.StatusNotFound, "monitor not found")
		return
	}

	dur := parseRange(r.URL.Query().Get("range"))
	beats := app.store.Range(id, since(dur))
	up, known := app.engine.CurrentUp(id)

	writeJSON(w, map[string]interface{}{
		"monitor":    m,
		"current_up": up,
		"known":      known,
		"uptime_24h": app.store.Uptime(id, since(24*time.Hour)),
		"uptime_30d": app.store.Uptime(id, since(30*24*time.Hour)),
		"avg_ping":   app.store.AvgLatency(id, since(dur)),
		"heartbeats": beats,
	})
}

func parseRange(s string) time.Duration {
	switch s {
	case "1h":
		return time.Hour
	case "6h":
		return 6 * time.Hour
	case "7d":
		return 7 * 24 * time.Hour
	case "30d":
		return 30 * 24 * time.Hour
	default:
		return 24 * time.Hour
	}
}

// --- Notification channels ---

func handleNotifyList(w http.ResponseWriter, r *http.Request) {
	var chs []*NotificationChannel
	app.cfg.Read(func(c *Config) {
		chs = append(chs, c.Notifications...)
	})
	writeJSON(w, chs)
}

func handleNotifySave(w http.ResponseWriter, r *http.Request) {
	var ch NotificationChannel
	if err := json.NewDecoder(r.Body).Decode(&ch); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if ch.Name == "" || ch.Type == "" {
		writeErr(w, http.StatusBadRequest, "name and type are required")
		return
	}
	err := app.cfg.Update(func(c *Config) {
		if ch.ID == "" {
			ch.ID = newID()
			c.Notifications = append(c.Notifications, &ch)
			return
		}
		for i, x := range c.Notifications {
			if x.ID == ch.ID {
				c.Notifications[i] = &ch
				return
			}
		}
		// id provided but not found -> append
		c.Notifications = append(c.Notifications, &ch)
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, ch)
}

func handleNotifyDelete(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	_ = app.cfg.Update(func(c *Config) {
		out := c.Notifications[:0]
		for _, ch := range c.Notifications {
			if ch.ID != id {
				out = append(out, ch)
			}
		}
		c.Notifications = out
		for _, m := range c.Monitors {
			m.NotificationIDs = removeString(m.NotificationIDs, id)
		}
	})
	writeOK(w)
}

// handleNotifyTest sends a test message through the posted channel config
func handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	var ch NotificationChannel
	if err := json.NewDecoder(r.Body).Decode(&ch); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ch.Enabled = true
	err := sendNotification(&ch, "[Uptime Neko] Test notification",
		"This is a test message from Uptime Neko. If you received this, the channel works! 🐱")
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeOK(w)
}

// --- Status pages ---

func handleStatusPageList(w http.ResponseWriter, r *http.Request) {
	var pages []*StatusPage
	app.cfg.Read(func(c *Config) {
		pages = append(pages, c.StatusPages...)
	})
	writeJSON(w, pages)
}

func handleStatusPageSave(w http.ResponseWriter, r *http.Request) {
	// Accept an optional plaintext "password" field to (re)set the hash.
	var raw struct {
		StatusPage
		Password    string `json:"password"`     // new plaintext (optional)
		ClearPasswd bool   `json:"clear_passwd"` // remove password
	}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	sp := raw.StatusPage
	if sp.Slug == "" || sp.Title == "" {
		writeErr(w, http.StatusBadRequest, "slug and title are required")
		return
	}
	if sp.Theme == "" {
		sp.Theme = "auto"
	}

	err := app.cfg.Update(func(c *Config) {
		// resolve existing for password preservation
		var existing *StatusPage
		for _, x := range c.StatusPages {
			if x.ID == sp.ID && sp.ID != "" {
				existing = x
			}
		}
		// password handling
		switch {
		case raw.ClearPasswd:
			sp.PasswordHash = ""
		case raw.Password != "":
			sp.PasswordHash = hashToken(raw.Password)
		case existing != nil:
			sp.PasswordHash = existing.PasswordHash
		}
		// if marked default, clear default on others
		if sp.IsDefault {
			for _, x := range c.StatusPages {
				if x.ID != sp.ID {
					x.IsDefault = false
				}
			}
		}
		if sp.ID == "" {
			sp.ID = newID()
			c.StatusPages = append(c.StatusPages, &sp)
			return
		}
		for i, x := range c.StatusPages {
			if x.ID == sp.ID {
				c.StatusPages[i] = &sp
				return
			}
		}
		c.StatusPages = append(c.StatusPages, &sp)
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, sp)
}

func handleStatusPageDelete(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	_ = app.cfg.Update(func(c *Config) {
		out := c.StatusPages[:0]
		for _, sp := range c.StatusPages {
			if sp.ID != id {
				out = append(out, sp)
			}
		}
		c.StatusPages = out
	})
	writeOK(w)
}

// --- API tokens (read-only public API) ---

func handleTokenList(w http.ResponseWriter, r *http.Request) {
	type tokenView struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Created int64  `json:"created"`
	}
	var views []tokenView
	app.cfg.Read(func(c *Config) {
		for _, t := range c.APITokens {
			views = append(views, tokenView{ID: t.ID, Name: t.Name, Created: t.Created})
		}
	})
	sort.Slice(views, func(i, j int) bool { return views[i].Created > views[j].Created })
	writeJSON(w, views)
}

// handleTokenCreate returns the plaintext token ONCE (only stored hashed)
func handleTokenCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Name == "" {
		body.Name = "token"
	}
	plain := newToken()
	tok := &APIToken{
		ID:        newID(),
		Name:      body.Name,
		TokenHash: hashToken(plain),
		Created:   time.Now().Unix(),
	}
	if err := app.cfg.Update(func(c *Config) {
		c.APITokens = append(c.APITokens, tok)
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{
		"id":    tok.ID,
		"name":  tok.Name,
		"token": plain, // shown once
	})
}

func handleTokenDelete(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	_ = app.cfg.Update(func(c *Config) {
		out := c.APITokens[:0]
		for _, t := range c.APITokens {
			if t.ID != id {
				out = append(out, t)
			}
		}
		c.APITokens = out
	})
	writeOK(w)
}

// --- Settings ---

func handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	app.cfg.Read(func(c *Config) {
		writeJSON(w, map[string]interface{}{
			"public_bind_addr": c.PublicBindAddr,
			"public_port":      c.PublicPort,
			"default_interval": c.DefaultInterval,
			"retention_days":   c.RetentionDays,
		})
	})
}

func handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PublicBindAddr  string `json:"public_bind_addr"`
		PublicPort      int    `json:"public_port"`
		DefaultInterval int    `json:"default_interval"`
		RetentionDays   int    `json:"retention_days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	portChanged := false
	app.cfg.Read(func(c *Config) {
		portChanged = c.PublicPort != body.PublicPort || c.PublicBindAddr != body.PublicBindAddr
	})
	err := app.cfg.Update(func(c *Config) {
		if body.PublicBindAddr != "" {
			c.PublicBindAddr = body.PublicBindAddr
		}
		if body.PublicPort > 0 {
			c.PublicPort = body.PublicPort
		}
		if body.DefaultInterval >= MinInterval {
			c.DefaultInterval = body.DefaultInterval
		}
		if body.RetentionDays > 0 {
			c.RetentionDays = body.RetentionDays
		}
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true, "restart_required": portChanged})
}

// removeString returns s with all occurrences of v removed
func removeString(s []string, v string) []string {
	out := s[:0]
	for _, x := range s {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

// itoa convenience for query building in tests/log
func itoa(i int) string { return strconv.Itoa(i) }
