package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

/*
	config.go

	Persistent configuration for Uptime Neko.
	Stored as a single config.json next to the plugin binary, following the
	same load/save-with-mutex pattern as the dns-server plugin.
*/

const CONFIG_PATH = "./config.json"

// MonitorType is the kind of check a monitor performs
type MonitorType string

const (
	MonitorHTTP MonitorType = "http" // HTTP/HTTPS GET, status code + optional keyword
	MonitorTCP  MonitorType = "tcp"  // TCP port connect
	MonitorPing MonitorType = "ping" // ICMP echo
	MonitorDNS  MonitorType = "dns"  // DNS resolve + optional expected value
	MonitorSSL  MonitorType = "ssl"  // TLS certificate expiry
)

// Monitor is a single thing being watched
type Monitor struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	Type     MonitorType `json:"type"`
	Target   string      `json:"target"`   // url, host:port, hostname or ip depending on type
	Interval int         `json:"interval"` // seconds, floored to MinInterval
	Timeout  int         `json:"timeout"`  // seconds
	Enabled  bool        `json:"enabled"`

	// HTTP options
	Method         string `json:"method,omitempty"`          // default GET
	Keyword        string `json:"keyword,omitempty"`         // body must contain this (optional)
	KeywordInvert  bool   `json:"keyword_invert,omitempty"`  // up when keyword is ABSENT
	ExpectedStatus int    `json:"expected_status,omitempty"` // 0 = any 2xx/3xx
	IgnoreTLS      bool   `json:"ignore_tls,omitempty"`      // skip cert verification

	// TCP / SSL options
	Port int `json:"port,omitempty"` // optional explicit port (else parsed from target)

	// DNS options
	DNSRecordType string `json:"dns_record_type,omitempty"` // A, AAAA, CNAME, TXT, MX, NS
	DNSResolver   string `json:"dns_resolver,omitempty"`    // optional custom resolver host:port
	DNSExpected   string `json:"dns_expected,omitempty"`    // optional expected value/keyword

	// SSL / HTTPS cert expiry warning threshold (days). 0 disables.
	CertWarnDays int `json:"cert_warn_days,omitempty"`

	// Linked notification channel IDs
	NotificationIDs []string `json:"notification_ids,omitempty"`
}

// NotificationChannel describes where alerts are delivered
type NotificationChannel struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"` // webhook
	Enabled bool   `json:"enabled"`

	// Webhook
	WebhookURL    string `json:"webhook_url,omitempty"`
	WebhookMethod string `json:"webhook_method,omitempty"` // default POST
}

// StatusPage is a public, read-only page bound to one or more hostnames
type StatusPage struct {
	ID           string   `json:"id"`
	Slug         string   `json:"slug"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	Hostnames    []string `json:"hostnames"`     // Host header values that route here
	MonitorIDs   []string `json:"monitor_ids"`   // monitors shown on the page
	PasswordHash string   `json:"password_hash"` // optional; empty = no password
	Theme        string   `json:"theme"`         // light, dark, auto
	IsDefault    bool     `json:"is_default"`    // fallback when no hostname matches
}

// APIToken grants read-only access to the public JSON API
type APIToken struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	TokenHash string `json:"token_hash"`
	Created   int64  `json:"created"`
}

// Config is the whole persisted state
type Config struct {
	PublicBindAddr  string                 `json:"public_bind_addr"` // default 127.0.0.1
	PublicPort      int                    `json:"public_port"`      // default 8999
	DefaultInterval int                    `json:"default_interval"` // default 20
	RetentionDays   int                    `json:"retention_days"`   // default 30
	Monitors        []*Monitor             `json:"monitors"`
	Notifications   []*NotificationChannel `json:"notifications"`
	StatusPages     []*StatusPage          `json:"status_pages"`
	APITokens       []*APIToken            `json:"api_tokens"`
}

// MinInterval is the hard floor on monitor check intervals (seconds)
const MinInterval = 20

// ConfigManager wraps Config with safe concurrent access + persistence
type ConfigManager struct {
	mu  sync.RWMutex
	cfg *Config
}

func defaultConfig() *Config {
	return &Config{
		PublicBindAddr:  "127.0.0.1",
		PublicPort:      8999,
		DefaultInterval: 20,
		RetentionDays:   30,
		Monitors:        []*Monitor{},
		Notifications:   []*NotificationChannel{},
		StatusPages:     []*StatusPage{},
		APITokens:       []*APIToken{},
	}
}

// LoadConfig reads config.json, falling back to defaults
func LoadConfig() *ConfigManager {
	cm := &ConfigManager{cfg: defaultConfig()}
	data, err := os.ReadFile(CONFIG_PATH)
	if err != nil {
		fmt.Printf("[uptime-neko] no config found, using defaults: %s\n", err.Error())
		return cm
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		fmt.Printf("[uptime-neko] failed to parse config, using defaults: %s\n", err.Error())
		return cm
	}
	// Normalize / backfill defaults
	if c.PublicBindAddr == "" {
		c.PublicBindAddr = "127.0.0.1"
	}
	if c.PublicPort == 0 {
		c.PublicPort = 8999
	}
	if c.DefaultInterval < MinInterval {
		c.DefaultInterval = MinInterval
	}
	if c.RetentionDays <= 0 {
		c.RetentionDays = 30
	}
	if c.Monitors == nil {
		c.Monitors = []*Monitor{}
	}
	if c.Notifications == nil {
		c.Notifications = []*NotificationChannel{}
	}
	if c.StatusPages == nil {
		c.StatusPages = []*StatusPage{}
	}
	if c.APITokens == nil {
		c.APITokens = []*APIToken{}
	}
	for _, m := range c.Monitors {
		if m.Interval < MinInterval {
			m.Interval = MinInterval
		}
		if m.Timeout <= 0 {
			m.Timeout = 10
		}
	}
	cm.cfg = &c
	return cm
}

// Save persists the current config to disk
func (cm *ConfigManager) Save() error {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.saveLocked()
}

func (cm *ConfigManager) saveLocked() error {
	f, err := os.Create(CONFIG_PATH)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(cm.cfg)
}

// Read gives read access to the config under a read lock
func (cm *ConfigManager) Read(fn func(c *Config)) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	fn(cm.cfg)
}

// Update mutates the config under a write lock and persists it
func (cm *ConfigManager) Update(fn func(c *Config)) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	fn(cm.cfg)
	return cm.saveLocked()
}

// --- helpers ---

// newID returns a short random hex id
func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// newToken returns a random API token string
func newToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// hashToken hashes an API token / password for storage
func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}
