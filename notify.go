package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

/*
	notify.go

	Notification delivery. Channels: Webhook.
	The engine calls Dispatch on up<->down transitions and DispatchCert on
	certificate-expiry threshold crossings.
*/

// Notifier resolves channel IDs from config and sends messages
type Notifier struct {
	cfg *ConfigManager
}

func NewNotifier(cfg *ConfigManager) *Notifier {
	return &Notifier{cfg: cfg}
}

// Dispatch sends an up/down transition alert for a monitor
func (n *Notifier) Dispatch(m *Monitor, res CheckResult, wasUp bool) {
	state := "🔴 DOWN"
	if res.Up {
		state = "🟢 UP"
	}
	subject := fmt.Sprintf("[Uptime Neko] %s is %s", m.Name, statusWord(res.Up))
	body := fmt.Sprintf("Monitor: %s\nTarget: %s\nStatus: %s\nMessage: %s\nTime: %s",
		m.Name, m.Target, state, res.Msg, time.Now().Format(time.RFC1123))
	n.sendToChannels(m.NotificationIDs, subject, body)
}

// DispatchCert sends a certificate-expiry warning
func (n *Notifier) DispatchCert(m *Monitor, daysLeft int) {
	subject := fmt.Sprintf("[Uptime Neko] %s certificate expiring in %d days", m.Name, daysLeft)
	body := fmt.Sprintf("Monitor: %s\nTarget: %s\nThe TLS certificate expires in %d days.\nTime: %s",
		m.Name, m.Target, daysLeft, time.Now().Format(time.RFC1123))
	n.sendToChannels(m.NotificationIDs, subject, body)
}

func (n *Notifier) sendToChannels(ids []string, subject, body string) {
	if len(ids) == 0 {
		return
	}
	var channels []*NotificationChannel
	n.cfg.Read(func(c *Config) {
		for _, id := range ids {
			for _, ch := range c.Notifications {
				if ch.ID == id && ch.Enabled {
					cp := *ch
					channels = append(channels, &cp)
				}
			}
		}
	})
	for _, ch := range channels {
		go func(ch *NotificationChannel) {
			if err := sendNotification(ch, subject, body); err != nil {
				fmt.Printf("[uptime-neko] notify via %s (%s) failed: %s\n", ch.Type, ch.Name, err.Error())
			}
		}(ch)
	}
}

// sendNotification delivers one message through a single channel.
// Exported behavior is reused by the "test" button in the admin API.
func sendNotification(ch *NotificationChannel, subject, body string) error {
	switch ch.Type {
	case "webhook":
		return sendWebhook(ch, subject, body)
	default:
		return fmt.Errorf("unknown channel type: %s", ch.Type)
	}
}

func sendWebhook(ch *NotificationChannel, subject, body string) error {
	if ch.WebhookURL == "" {
		return fmt.Errorf("webhook url is required")
	}
	method := ch.WebhookMethod
	if method == "" {
		method = http.MethodPost
	}
	payload := map[string]interface{}{
		"subject": subject,
		"body":    body,
		"time":    time.Now().Unix(),
	}
	data, _ := json.Marshal(payload)

	req, err := http.NewRequest(strings.ToUpper(method), ch.WebhookURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("webhook returned %d", resp.StatusCode)
	}
	return nil
}

func statusWord(up bool) string {
	if up {
		return "UP"
	}
	return "DOWN"
}
