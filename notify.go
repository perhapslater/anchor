package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Notification is one event delivered to the administrator's selected destinations.
type Notification struct {
	Kind     string
	delivery *notificationReceipts
	Title    string
	Message  string
	Priority int // ntfy: 1 (min) to 5 (max); 3 is default
	Tags     []string
	Click    string // URL opened when the notification is tapped
}

// Notifier sends notifications. Tests swap in a fake.
type Notifier interface {
	Send(ctx context.Context, n Notification) error
}

// ntfyNotifier publishes using the saved SQLite credential projection.
type ntfyNotifier struct {
	store  *ConfigStore
	token  string
	client *http.Client
}

func newNtfyNotifier(store *ConfigStore, token string) *ntfyNotifier {
	client := newUpstreamClient(false)
	// Same reasoning as the status checks: fresh connections survive
	// Tailscale path changes.
	client.Timeout = 10 * time.Second
	return &ntfyNotifier{
		store:  store,
		token:  strings.TrimSpace(token),
		client: client,
	}
}

func (n *ntfyNotifier) Send(ctx context.Context, msg Notification) error {
	cfg, _ := n.store.Get()
	if cfg.Notify == nil {
		return fmt.Errorf("notify is not set up in anchor.yaml")
	}
	body, _ := json.Marshal(map[string]any{
		"topic":    cfg.Notify.Topic,
		"title":    msg.Title,
		"message":  msg.Message,
		"priority": msg.Priority,
		"tags":     msg.Tags,
		"click":    msg.Click,
	})
	// JSON publishing goes to the server root; the topic is in the body.
	server, err := validUpstream(cfg.Notify.Server)
	if err != nil {
		return fmt.Errorf("ntfy address is invalid")
	}
	endpoint := server + "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("could not create ntfy request")
	}
	req.Header.Set("Content-Type", "application/json")
	token := n.token
	if cfg.credentials != nil {
		token = cfg.credentials["ntfy"]
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy could not be reached")
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("ntfy answered HTTP %d", resp.StatusCode)
	}
	return nil
}

// sendAsync sends in the background with one retry. Used for alerts, which
// must never slow down the status checks.
func sendAsync(nt Notifier, msg Notification, done func(bool)) {
	sendAsyncContext(context.Background(), nt, msg, done)
}

func sendAsyncContext(parent context.Context, nt Notifier, msg Notification, done func(bool)) {
	if nt == nil {
		if done != nil {
			done(false)
		}
		return
	}
	if msg.delivery == nil {
		msg.delivery = &notificationReceipts{}
	}
	go func() {
		sent := false
		defer func() {
			if done != nil {
				done(sent)
			}
		}()
		for attempt := 0; attempt < 2; attempt++ {
			if parent.Err() != nil {
				return
			}
			ctx, cancel := context.WithTimeout(parent, 15*time.Second)
			err := nt.Send(ctx, msg)
			cancel()
			if err == nil {
				sent = true
				return
			}
			log.Printf("notification %q failed (attempt %d): %v", msg.Title, attempt+1, err)
			if attempt == 0 {
				delay := 10 * time.Second
				var partial *partialDeliveryError
				if errors.As(err, &partial) && partial.retryAfter > delay {
					delay = partial.retryAfter
				}
				if !waitContext(parent, delay) {
					return
				}
			}
		}
	}()
}

// Guarded by Checker.mu. Publication is distinct from an attempt; a failed
// batch can be retried by a later health check without flooding the notifier.
type alertDelivery struct {
	receipts      *notificationReceipts
	sent, pending bool
	attempted     time.Time
	generation    uint64
}

func (d *alertDelivery) start(now time.Time) bool {
	if d.sent || d.pending || (!d.attempted.IsZero() && now.Sub(d.attempted) < 5*time.Minute) {
		return false
	}
	d.pending, d.attempted = true, now
	if d.receipts == nil {
		d.receipts = &notificationReceipts{}
	}
	d.generation++
	return true
}

func (d *alertDelivery) reset() {
	if d.receipts != nil {
		d.receipts.cancelled.Store(true)
	}
	d.receipts = nil
	d.sent, d.pending, d.attempted = false, false, time.Time{}
	d.generation++
}

// rateLimiter allows one event per key per window, plus a global cap per
// hour so a stuck button or a script can't flood your phone.
type rateLimiter struct {
	mu        sync.Mutex
	window    time.Duration
	perHour   int
	last      map[string]time.Time
	hourStart time.Time
	hourCount int
	now       func() time.Time
}

func newRateLimiter(window time.Duration, perHour int) *rateLimiter {
	return &rateLimiter{window: window, perHour: perHour, last: map[string]time.Time{}, now: time.Now}
}

// allow reports whether the event may go ahead, and records it if so.
func (r *rateLimiter) allow(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if now.Sub(r.hourStart) >= time.Hour {
		r.hourStart, r.hourCount = now, 0
	}
	if r.hourCount >= r.perHour {
		return false
	}
	if t, ok := r.last[key]; ok && now.Sub(t) < r.window {
		return false
	}
	for k, t := range r.last { // keep the map small
		if now.Sub(t) >= r.window {
			delete(r.last, k)
		}
	}
	r.last[key] = now
	r.hourCount++
	return true
}

// forceHTTP1 makes a transport open a fresh connection per request and speak
// only HTTP/1.1.
//
// Fresh connections: a kept-alive connection across the tailnet can die
// silently when Tailscale switches paths, and requests sent down it just
// time out.
//
// HTTP/1.1 only: newer Go releases enable HTTP/2 through the transport's
// Protocols setting, and the old trick of emptying TLSNextProto no longer
// stops the client from offering "h2". The server (Tailscale Serve) then
// answered in HTTP/2 and Go read it as HTTP/1 ("malformed HTTP response
// \x00\x00..."). Setting Protocols AND the TLS ALPN list means the server
// can't pick HTTP/2 on any Go version.
func forceHTTP1(tr *http.Transport) {
	tr.DisableKeepAlives = true
	tr.ForceAttemptHTTP2 = false
	p := new(http.Protocols)
	p.SetHTTP1(true)
	tr.Protocols = p
	tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{}
	} else {
		tr.TLSClientConfig = tr.TLSClientConfig.Clone()
	}
	tr.TLSClientConfig.NextProtos = []string{"http/1.1"}
}
