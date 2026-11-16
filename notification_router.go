package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func validWebhookURL(raw, format string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || strings.ContainsAny(raw, "\r\n\t ") {
		return "", errors.New("webhook URL must be an HTTPS address without a username or fragment")
	}
	if format == "discord" {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if u.Host != "discord.com" || len(parts) != 4 || parts[0] != "api" || parts[1] != "webhooks" || parts[2] == "" || parts[3] == "" || u.RawQuery != "" {
			return "", errors.New("paste a Discord webhook URL from the channel's Integrations settings")
		}
	}
	return raw, nil
}

type notificationReceipt struct {
	mu   sync.Mutex
	sent atomic.Bool
}
type notificationReceipts struct {
	mu        sync.Mutex
	targets   map[string]*notificationReceipt
	cancelled atomic.Bool
}

func (r *notificationReceipts) anySent() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, receipt := range r.targets {
		if receipt.sent.Load() {
			return true
		}
	}
	return false
}

func (r *notificationReceipts) target(key string) *notificationReceipt {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.targets == nil {
		r.targets = map[string]*notificationReceipt{}
	}
	if r.targets[key] == nil {
		r.targets[key] = &notificationReceipt{}
	}
	return r.targets[key]
}

type notificationDeliveryStatus struct {
	fingerprint string
	State       string    `json:"state"`
	Message     string    `json:"message"`
	At          time.Time `json:"at"`
}
type notificationRouter struct {
	store              *ConfigStore
	ntfy               *ntfyNotifier
	client             *http.Client
	mu                 sync.Mutex
	status             map[string]notificationDeliveryStatus
	nextWebhook        time.Time
	webhookFingerprint string
}

func newNotificationRouter(store *ConfigStore) *notificationRouter {
	return &notificationRouter{store: store, ntfy: newNtfyNotifier(store, ""), client: newUpstreamClient(false), status: map[string]notificationDeliveryStatus{}}
}
func (n *notificationRouter) statuses() map[string]notificationDeliveryStatus {
	cfg, _ := n.store.Get()
	n.mu.Lock()
	defer n.mu.Unlock()
	out := map[string]notificationDeliveryStatus{}
	for k, v := range n.status {
		if v.fingerprint == notificationFingerprint(cfg, k) {
			out[k] = v
		}
	}
	return out
}

func notificationFingerprint(cfg *Config, destination string) string {
	if destination == "ntfy" && cfg.Notify != nil {
		return tokenHash(cfg.Notify.Server + "\n" + cfg.Notify.Topic + "\n" + cfg.credentials["ntfy"])
	}
	if destination == "webhook" && cfg.Webhook != nil {
		return tokenHash(cfg.Webhook.Format + "\n" + cfg.credentials["webhook"])
	}
	return ""
}

type partialDeliveryError struct {
	delivered  int
	retryAfter time.Duration
}

func (e *partialDeliveryError) Error() string {
	return "one or more notification destinations could not deliver; check Notifications for details"
}

type webhookRateLimit struct{ delay time.Duration }

func (e *webhookRateLimit) Error() string {
	return "webhook is rate limited; Anchor will wait before retrying"
}

// Send takes one settings snapshot. Each destination runs independently and keeps
// its receipt across retries of the same outage. Successful targets are not resent.
func (n *notificationRouter) Send(ctx context.Context, msg Notification) error {
	return n.send(ctx, msg, "")
}
func (n *notificationRouter) send(ctx context.Context, msg Notification, only string) error {
	cfg, _ := n.store.Get()
	if msg.Kind == "" {
		msg.Kind = "alert"
	}
	if msg.delivery == nil {
		msg.delivery = &notificationReceipts{}
	}
	if msg.delivery.cancelled.Load() {
		return nil
	}
	type target struct {
		name, sig string
		send      func() error
	}
	var targets []target
	ntfyOn := cfg.Notify != nil && (msg.Kind == "test" || msg.Kind == "report" && cfg.Notify.reportsOn() || msg.Kind == "alert" && cfg.Notify.alertsOn())
	if ntfyOn && (only == "" || only == "ntfy") {
		// Freeze the config so a settings edit cannot redirect an in-flight send.
		frozen := &ConfigStore{cfg: cfg}
		sender := &ntfyNotifier{store: frozen, client: n.ntfy.client}
		targets = append(targets, target{"ntfy", notificationFingerprint(cfg, "ntfy"), func() error { return sender.Send(ctx, msg) }})
	}
	w := cfg.Webhook
	if w != nil && (only == "" || only == "webhook") && (msg.Kind == "test" || msg.Kind == "report" && (w.Reports == nil || *w.Reports) || msg.Kind == "alert" && (w.Alerts == nil || *w.Alerts)) {
		targets = append(targets, target{"webhook", notificationFingerprint(cfg, "webhook"), func() error { return n.sendWebhook(ctx, cfg, msg) }})
	}
	if len(targets) == 0 {
		return errors.New("no notification destination is enabled for this event")
	}
	type result struct{ err error }
	results := make(chan result, len(targets))
	for _, t := range targets {
		go func() {
			receipt := msg.delivery.target(t.name + ":" + t.sig)
			receipt.mu.Lock()
			defer receipt.mu.Unlock()
			if receipt.sent.Load() || msg.delivery.cancelled.Load() {
				results <- result{}
				return
			}
			err := t.send()
			receipt.sent.Store(err == nil)
			status := notificationDeliveryStatus{fingerprint: t.sig, State: "sent", Message: "Last delivery accepted by the service.", At: time.Now().UTC()}
			if err != nil {
				status.State, status.Message = "failed", err.Error()
			}
			n.mu.Lock()
			n.status[t.name] = status
			n.mu.Unlock()
			results <- result{err}
		}()
	}
	delivered := 0
	var delay time.Duration
	for range targets {
		r := <-results
		if r.err == nil {
			delivered++
		}
		var limited *webhookRateLimit
		if errors.As(r.err, &limited) && limited.delay > delay {
			delay = limited.delay
		}
	}
	if delivered != len(targets) {
		return &partialDeliveryError{delivered: delivered, retryAfter: delay}
	}
	return nil
}

func clipped(s string, limit int) string {
	units := 0
	for i, r := range s {
		width := 1
		if r > 0xffff {
			width = 2
		}
		units += width
		if units >= limit {
			return s[:i] + "…"
		}
	}
	return s
}
func (n *notificationRouter) sendWebhook(ctx context.Context, cfg *Config, msg Notification) error {
	endpoint, err := validWebhookURL(cfg.credentials["webhook"], cfg.Webhook.Format)
	if err != nil {
		return err
	}
	n.mu.Lock()
	fingerprint := notificationFingerprint(cfg, "webhook")
	if n.webhookFingerprint != fingerprint {
		n.webhookFingerprint = fingerprint
		n.nextWebhook = time.Time{}
	}
	remaining := time.Until(n.nextWebhook)
	n.mu.Unlock()
	if remaining > 0 {
		return &webhookRateLimit{remaining}
	}
	var payload any = map[string]any{"schema_version": 1, "event": msg.Kind, "title": msg.Title, "message": msg.Message, "priority": msg.Priority, "tags": msg.Tags, "url": msg.Click}
	if cfg.Webhook.Format == "discord" {
		u, _ := url.Parse(endpoint)
		q := u.Query()
		q.Set("wait", "true")
		u.RawQuery = q.Encode()
		endpoint = u.String()
		text := msg.Title + "\n" + msg.Message
		if msg.Click != "" {
			text += "\n" + msg.Click
		}
		payload = map[string]any{"content": clipped(text, 1900), "allowed_mentions": map[string]any{"parse": []string{}}}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return errors.New("could not prepare webhook message")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return errors.New("could not prepare webhook request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Anchor")
	resp, err := n.client.Do(req)
	if err != nil {
		return errors.New("webhook could not be reached; check its address and Anchor's network access")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		delay := 10 * time.Second
		if seconds, err := strconv.ParseFloat(resp.Header.Get("Retry-After"), 64); err == nil && seconds > 0 && seconds < 86400 {
			delay = time.Duration(seconds * float64(time.Second))
		} else if at, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil {
			if until := time.Until(at); until > 0 && until < 24*time.Hour {
				delay = until
			}
		}
		if cfg.Webhook.Format == "discord" {
			var reply struct {
				RetryAfter float64 `json:"retry_after"`
			}
			if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&reply) == nil && reply.RetryAfter > 0 && reply.RetryAfter < 86400 {
				delay = time.Duration(reply.RetryAfter * float64(time.Second))
			}
		}
		n.mu.Lock()
		if n.webhookFingerprint == fingerprint {
			n.nextWebhook = time.Now().Add(delay)
		}
		n.mu.Unlock()
		return &webhookRateLimit{delay}
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("webhook answered HTTP %d", resp.StatusCode)
	}
	return nil
}
