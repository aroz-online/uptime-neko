package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

/*
	api_public.go

	Everything served on the public port (default 127.0.0.1:8999). This is
	read-only: published status pages routed by Host header, plus a
	token-authenticated read-only JSON API under /api/v1.
*/

// publicMonitorView is the trimmed, safe-to-expose monitor payload.
// Note: the monitor target is deliberately NOT exposed publicly.
type publicMonitorView struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	Type       string      `json:"type"`
	CurrentUp  bool        `json:"current_up"`
	Known      bool        `json:"known"`
	Uptime24h  float64     `json:"uptime_24h"`
	Uptime30d  float64     `json:"uptime_30d"`
	AvgPing    float64     `json:"avg_ping"`
	Heartbeats []Heartbeat `json:"heartbeats"`
}

// resolvePage finds the status page for a request (by Host, then ?slug=, then default)
func resolvePage(r *http.Request) *StatusPage {
	host := r.Host
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	host = strings.ToLower(strings.TrimSpace(host))
	slug := r.URL.Query().Get("slug")

	var match, def *StatusPage
	app.cfg.Read(func(c *Config) {
		for _, sp := range c.StatusPages {
			if sp.IsDefault {
				def = sp
			}
			if slug != "" && sp.Slug == slug {
				match = sp
			}
			if slug == "" {
				for _, h := range sp.Hostnames {
					if strings.ToLower(strings.TrimSpace(h)) == host {
						match = sp
					}
				}
			}
		}
	})
	if match != nil {
		return match
	}
	return def
}

func cookieName(pageID string) string { return "neko_sp_" + pageID }

// pageAuthorized reports whether the request carries a valid password cookie
func pageAuthorized(r *http.Request, sp *StatusPage) bool {
	if sp.PasswordHash == "" {
		return true
	}
	ck, err := r.Cookie(cookieName(sp.ID))
	if err != nil {
		return false
	}
	return ck.Value == sp.PasswordHash
}

// handlePublicPage returns the page metadata + monitor data for the current host.
// If the page is password protected and the request is unauthorized, it returns
// 401 with {"locked": true}.
func handlePublicPage(w http.ResponseWriter, r *http.Request) {
	sp := resolvePage(r)
	if sp == nil {
		writeJSON(w, map[string]interface{}{"exists": false})
		return
	}
	if !pageAuthorized(r, sp) {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]interface{}{
			"exists": true,
			"locked": true,
			"title":  sp.Title,
		})
		return
	}

	views := buildPublicViews(sp.MonitorIDs)
	allUp := true
	for _, v := range views {
		if v.Known && !v.CurrentUp {
			allUp = false
		}
	}
	writeJSON(w, map[string]interface{}{
		"exists":      true,
		"locked":      false,
		"title":       sp.Title,
		"description": sp.Description,
		"theme":       sp.Theme,
		"all_up":      allUp,
		"monitors":    views,
		"updated":     time.Now().UnixMilli(),
	})
}

// handlePublicPageAuth validates a password and sets the access cookie
func handlePublicPageAuth(w http.ResponseWriter, r *http.Request) {
	sp := resolvePage(r)
	if sp == nil || sp.PasswordHash == "" {
		writeErr(w, http.StatusBadRequest, "no password required")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if hashToken(body.Password) != sp.PasswordHash {
		writeErr(w, http.StatusUnauthorized, "incorrect password")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName(sp.ID),
		Value:    sp.PasswordHash,
		Path:     "/",
		HttpOnly: true,
		MaxAge:   86400 * 7,
		SameSite: http.SameSiteLaxMode,
	})
	writeOK(w)
}

func buildPublicViews(monitorIDs []string) []publicMonitorView {
	idSet := make(map[string]bool, len(monitorIDs))
	for _, id := range monitorIDs {
		idSet[id] = true
	}
	var monitors []*Monitor
	app.cfg.Read(func(c *Config) {
		for _, m := range c.Monitors {
			if idSet[m.ID] {
				monitors = append(monitors, m)
			}
		}
	})

	// preserve the order chosen in the status page config
	order := make(map[string]int, len(monitorIDs))
	for i, id := range monitorIDs {
		order[id] = i
	}
	views := make([]publicMonitorView, 0, len(monitors))
	for _, m := range monitors {
		up, known := app.engine.CurrentUp(m.ID)
		views = append(views, publicMonitorView{
			ID:         m.ID,
			Name:       m.Name,
			Type:       string(m.Type),
			CurrentUp:  up,
			Known:      known,
			Uptime24h:  app.store.Uptime(m.ID, since(24*time.Hour)),
			Uptime30d:  app.store.Uptime(m.ID, since(30*24*time.Hour)),
			AvgPing:    app.store.AvgLatency(m.ID, since(24*time.Hour)),
			Heartbeats: app.store.Recent(m.ID),
		})
	}
	sortViews(views, order)
	return views
}

func sortViews(views []publicMonitorView, order map[string]int) {
	for i := 1; i < len(views); i++ {
		for j := i; j > 0 && order[views[j].ID] < order[views[j-1].ID]; j-- {
			views[j], views[j-1] = views[j-1], views[j]
		}
	}
}

// --- Token-authenticated read-only API (/api/v1) ---

func authToken(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	tok := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	if tok == "" {
		tok = r.URL.Query().Get("token")
	}
	if tok == "" {
		return false
	}
	hash := hashToken(tok)
	valid := false
	app.cfg.Read(func(c *Config) {
		for _, t := range c.APITokens {
			if t.TokenHash == hash {
				valid = true
				return
			}
		}
	})
	return valid
}

func requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authToken(r) {
			writeErr(w, http.StatusUnauthorized, "invalid or missing API token")
			return
		}
		next(w, r)
	}
}

// GET /api/v1/status — overall + per-monitor current state
func handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	var monitors []*Monitor
	app.cfg.Read(func(c *Config) {
		monitors = append(monitors, c.Monitors...)
	})
	type item struct {
		ID        string  `json:"id"`
		Name      string  `json:"name"`
		Type      string  `json:"type"`
		Up        bool    `json:"up"`
		Known     bool    `json:"known"`
		Uptime24h float64 `json:"uptime_24h"`
		AvgPing   float64 `json:"avg_ping_24h"`
	}
	out := make([]item, 0, len(monitors))
	upCount := 0
	for _, m := range monitors {
		up, known := app.engine.CurrentUp(m.ID)
		if up {
			upCount++
		}
		out = append(out, item{
			ID: m.ID, Name: m.Name, Type: string(m.Type), Up: up, Known: known,
			Uptime24h: app.store.Uptime(m.ID, since(24*time.Hour)),
			AvgPing:   app.store.AvgLatency(m.ID, since(24*time.Hour)),
		})
	}
	writeJSON(w, map[string]interface{}{
		"total":    len(monitors),
		"up":       upCount,
		"down":     len(monitors) - upCount,
		"monitors": out,
	})
}

// GET /api/v1/monitors — monitor definitions (target included for token holders)
func handleAPIMonitors(w http.ResponseWriter, r *http.Request) {
	var monitors []*Monitor
	app.cfg.Read(func(c *Config) {
		monitors = append(monitors, c.Monitors...)
	})
	writeJSON(w, monitors)
}

// GET /api/v1/heartbeats?id=&range= — heartbeat history for one monitor
func handleAPIHeartbeats(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	dur := parseRange(r.URL.Query().Get("range"))
	writeJSON(w, map[string]interface{}{
		"id":         id,
		"uptime_24h": app.store.Uptime(id, since(24*time.Hour)),
		"avg_ping":   app.store.AvgLatency(id, since(dur)),
		"heartbeats": app.store.Range(id, since(dur)),
	})
}
