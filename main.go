package main

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"time"

	plugin "aroz.org/zoraxy/uptime-neko/mod/zoraxy_plugin"
)

/*
	Uptime Neko — a mini Uptime Kuma for Zoraxy.

	Two web surfaces:
	  - Admin UI  : served on 127.0.0.1:<introspect port>, proxied through the
	                Zoraxy admin panel (UIPath). Full configuration.
	  - Public UI : served on a separate, user-configurable port (default
	                127.0.0.1:8999). Read-only status pages, routed by Host.
*/

const (
	PLUGIN_ID   = "org.aroz.zoraxy.uptime-neko"
	UI_PATH     = "/"
	WEB_ROOT    = "/www"
	PUBLIC_ROOT = "www_public"
)

//go:embed www/*
var adminFS embed.FS

//go:embed www_public/*
var publicFS embed.FS

// App holds shared singletons used across handlers
type App struct {
	cfg      *ConfigManager
	store    *Store
	engine   *Engine
	notifier *Notifier
}

// global app (single plugin instance)
var app *App

func main() {
	runtimeCfg, err := plugin.ServeAndRecvSpec(&plugin.IntroSpect{
		ID:            PLUGIN_ID,
		Name:          "Uptime Neko",
		Author:        "aroz.org",
		AuthorContact: "noreply@aroz.org",
		Description:   "A mini Uptime Monitor: monitor hosts, websites, ports, DNS and TLS certs with status pages and notifications.",
		URL:           "https://github.com/aroz-online/uptime-neko",
		Type:          plugin.PluginType_Utilities,
		VersionMajor:  1,
		VersionMinor:  0,
		VersionPatch:  1,
		UIPath:        UI_PATH,
	})
	if err != nil {
		panic(err)
	}

	// --- Core wiring ---
	cfg := LoadConfig()
	store, err := OpenStore(DB_PATH)
	if err != nil {
		panic(err)
	}
	notifier := NewNotifier(cfg)
	engine := NewEngine(cfg, store, notifier)
	app = &App{cfg: cfg, store: store, engine: engine, notifier: notifier}

	// Warm the in-memory ring from disk so charts have history immediately
	var ids []string
	cfg.Read(func(c *Config) {
		for _, m := range c.Monitors {
			ids = append(ids, m.ID)
		}
	})
	store.WarmRing(ids)

	// Start monitoring + maintenance loops
	engine.Start()
	go pruneLoop()

	// --- Public server (separate port) ---
	go startPublicServer()

	// --- Admin server (proxied via Zoraxy) ---
	adminMux := http.NewServeMux()
	embedWebRouter := plugin.NewPluginEmbedUIRouter(PLUGIN_ID, &adminFS, WEB_ROOT, UI_PATH)
	// Only point at the on-disk ./www folder when it actually exists (local dev
	// runs from the source tree); otherwise leave dev mode off so the router
	// falls back to the embedded FS, which is what a standalone-deployed binary
	// (no ./www next to it) needs to serve its UI at all.
	if info, err := os.Stat("./www"); err == nil && info.IsDir() {
		embedWebRouter.SetDevWebRoot("./www")
	}
	embedWebRouter.RegisterTerminateHandler(func() {
		fmt.Println("[uptime-neko] terminating...")
		engine.StopAll()
		_ = store.Close()
	}, adminMux)

	registerAdminAPI(embedWebRouter, adminMux)

	// Catch-all: serve the admin UI
	adminMux.Handle(UI_PATH, embedWebRouter.Handler())

	addr := "127.0.0.1:" + strconv.Itoa(runtimeCfg.Port)
	fmt.Println("[uptime-neko] admin UI on http://" + addr)
	if err := http.ListenAndServe(addr, adminMux); err != nil {
		panic(err)
	}
}

// registerAdminAPI mounts all configuration endpoints on the admin mux
func registerAdminAPI(router *plugin.PluginUiRouter, mux *http.ServeMux) {
	h := func(path string, fn http.HandlerFunc) {
		router.HandleFunc(path, fn, mux)
	}
	// dashboard
	h("/api/summary", handleSummary)
	// monitors
	h("/api/monitors", handleMonitorList)
	h("/api/monitor/add", handleMonitorAdd)
	h("/api/monitor/update", handleMonitorUpdate)
	h("/api/monitor/delete", handleMonitorDelete)
	h("/api/monitor/toggle", handleMonitorToggle)
	h("/api/monitor/detail", handleMonitorDetail)
	// notifications
	h("/api/notify/list", handleNotifyList)
	h("/api/notify/save", handleNotifySave)
	h("/api/notify/delete", handleNotifyDelete)
	h("/api/notify/test", handleNotifyTest)
	// status pages
	h("/api/statuspage/list", handleStatusPageList)
	h("/api/statuspage/save", handleStatusPageSave)
	h("/api/statuspage/delete", handleStatusPageDelete)
	// tokens
	h("/api/token/list", handleTokenList)
	h("/api/token/create", handleTokenCreate)
	h("/api/token/delete", handleTokenDelete)
	// settings
	h("/api/settings/get", handleSettingsGet)
	h("/api/settings/save", handleSettingsSave)
}

// startPublicServer serves status pages and the read-only token API
func startPublicServer() {
	var bindAddr string
	var port int
	app.cfg.Read(func(c *Config) {
		bindAddr = c.PublicBindAddr
		port = c.PublicPort
	})
	if bindAddr == "" {
		bindAddr = "127.0.0.1"
	}
	if port == 0 {
		port = 8999
	}

	mux := http.NewServeMux()

	// Public status-page data
	mux.HandleFunc("/api/page", handlePublicPage)
	mux.HandleFunc("/api/page/auth", handlePublicPageAuth)

	// Token-authenticated read-only API
	mux.HandleFunc("/api/v1/status", requireToken(handleAPIStatus))
	mux.HandleFunc("/api/v1/monitors", requireToken(handleAPIMonitors))
	mux.HandleFunc("/api/v1/heartbeats", requireToken(handleAPIHeartbeats))

	// Static public UI (self-contained, no Zoraxy assets)
	sub, err := fs.Sub(publicFS, PUBLIC_ROOT)
	if err != nil {
		fmt.Println("[uptime-neko] public FS error:", err.Error())
		return
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

	addr := bindAddr + ":" + strconv.Itoa(port)
	fmt.Println("[uptime-neko] public status server on http://" + addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Println("[uptime-neko] public server error:", err.Error())
	}
}

// pruneLoop trims old heartbeats once per hour
func pruneLoop() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		var days int
		app.cfg.Read(func(c *Config) { days = c.RetentionDays })
		if days <= 0 {
			days = 30
		}
		app.store.Prune(days)
	}
}
