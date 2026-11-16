package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNtfyProductionClientBlocksLiteralAndResolvedLoopback(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{server.URL, "http://localhost:" + port} {
		cfg := &Config{Notify: &Notify{Server: address, Topic: "audit"}}
		notifier := newNtfyNotifier(&ConfigStore{cfg: cfg}, "synthetic-private-token")
		err := notifier.Send(context.Background(), Notification{Title: "Owned fixture only"})
		if err == nil || hits.Load() != 0 {
			t.Fatalf("prohibited destination reached: hits=%d err=%v", hits.Load(), err)
		}
		if strings.Contains(err.Error(), address) || strings.Contains(err.Error(), "synthetic-private-token") {
			t.Fatal("ntfy failure disclosed its destination or credential")
		}
	}
}

func webhookSecurityConfig(endpoint string) *Config {
	return &Config{Webhook: &WebhookConfig{Format: "json"}, credentials: map[string]string{"webhook": endpoint}}
}

func TestWebhookRetryAfterBounds(t *testing.T) {
	for _, test := range []struct {
		name, header, body string
		discord            bool
		minimum, maximum   time.Duration
	}{
		{"numeric", "30", "", false, 30 * time.Second, 30 * time.Second},
		{"nearby date", time.Now().Add(2 * time.Minute).UTC().Format(http.TimeFormat), "", false, 110 * time.Second, 2 * time.Minute},
		{"distant date", time.Now().Add(365 * 24 * time.Hour).UTC().Format(http.TimeFormat), "", false, 10 * time.Second, 10 * time.Second},
		{"infinite numeric", "+Inf", "", false, 10 * time.Second, 10 * time.Second},
		{"oversized numeric", "86400000", "", false, 10 * time.Second, 10 * time.Second},
		{"discord override", "30", `{"retry_after":90}`, true, 90 * time.Second, 90 * time.Second},
		{"oversized discord", "", `{"retry_after":1e300}`, true, 10 * time.Second, 10 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := webhookSecurityConfig("https://receiver.example.test/hook")
			if test.discord {
				cfg.Webhook.Format = "discord"
				cfg.credentials["webhook"] = "https://discord.com/api/webhooks/123/synthetic-token"
			}
			router := newNotificationRouter(&ConfigStore{cfg: cfg})
			router.client = &http.Client{Transport: notificationTransport(func(*http.Request) (*http.Response, error) {
				response := notificationResponse(http.StatusTooManyRequests, test.body)
				response.Header.Set("Retry-After", test.header)
				return response, nil
			})}
			err := router.sendWebhook(context.Background(), cfg, Notification{})
			var limited *webhookRateLimit
			if !errors.As(err, &limited) || limited.delay < test.minimum || limited.delay > test.maximum {
				t.Fatalf("retry delay out of bounds: %v (%#v)", err, limited)
			}
		})
	}
}

func TestWebhookReplacementDoesNotInheritCooldown(t *testing.T) {
	old := webhookSecurityConfig("https://old.example.test/hook")
	replacement := webhookSecurityConfig("https://replacement.example.test/hook")
	router := newNotificationRouter(&ConfigStore{cfg: old})
	calls := 0
	router.client = &http.Client{Transport: notificationTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Hostname() == "old.example.test" {
			response := notificationResponse(http.StatusTooManyRequests, "")
			response.Header.Set("Retry-After", "30")
			return response, nil
		}
		return notificationResponse(http.StatusNoContent, ""), nil
	})}
	if err := router.sendWebhook(context.Background(), old, Notification{}); err == nil {
		t.Fatal("expected original receiver's rate limit")
	}
	if err := router.sendWebhook(context.Background(), old, Notification{}); err == nil || calls != 1 {
		t.Fatalf("unchanged receiver's cooldown not enforced: calls=%d err=%v", calls, err)
	}
	if err := router.sendWebhook(context.Background(), replacement, Notification{}); err != nil || calls != 2 {
		t.Fatalf("replacement inherited old cooldown: calls=%d err=%v", calls, err)
	}
}

func TestWebhookLateRateLimitDoesNotThrottleReplacement(t *testing.T) {
	old := webhookSecurityConfig("https://old.example.test/hook")
	replacement := webhookSecurityConfig("https://replacement.example.test/hook")
	router := newNotificationRouter(&ConfigStore{cfg: old})
	started, release := make(chan struct{}), make(chan struct{})
	var replacementCalls atomic.Int32
	router.client = &http.Client{Transport: notificationTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Hostname() == "old.example.test" {
			close(started)
			<-release
			response := notificationResponse(http.StatusTooManyRequests, "")
			response.Header.Set("Retry-After", "3600")
			return response, nil
		}
		replacementCalls.Add(1)
		return notificationResponse(http.StatusNoContent, ""), nil
	})}
	done := make(chan error, 1)
	go func() { done <- router.sendWebhook(context.Background(), old, Notification{}) }()
	<-started
	firstErr := router.sendWebhook(context.Background(), replacement, Notification{})
	close(release)
	oldErr := <-done
	if firstErr != nil || oldErr == nil {
		t.Fatalf("unexpected delivery results: replacement=%v original=%v", firstErr, oldErr)
	}
	if err := router.sendWebhook(context.Background(), replacement, Notification{}); err != nil || replacementCalls.Load() != 2 {
		t.Fatalf("late old response throttled replacement: calls=%d err=%v", replacementCalls.Load(), err)
	}
}
