package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestRevokedSessionCannotSendDelayedReport(t *testing.T) {
	for _, revoke := range []string{"logout", "expire"} {
		t.Run(revoke, func(t *testing.T) {
			f := newAccountFixture(t)
			f.setup(t)
			cfg, _ := f.state.store.Get()
			copyCfg := *cfg
			copyCfg.Notify = &Notify{Server: "https://notifications.invalid", Topic: "audit"}
			copyCfg.Apps = []App{{ID: "audit", Name: "Audit", URL: "https://audit.invalid", Access: []string{"everyone"}, Check: "off"}}
			if err := f.state.saveConfig(&copyCfg, mustUserRevision(t, f), f.user.ID); err != nil {
				t.Fatal(err)
			}
			notifier := &fakeNotifier{}
			h := &handler{store: f.state.store, auth: f.auth, checker: NewChecker(f.state.store), notifier: notifier, reports: newRateLimiter(0, 100)}
			mux := http.NewServeMux()
			f.auth.routes(mux)
			mux.HandleFunc("POST /api/report", h.report)
			f.handler = externalScheme("http", f.auth.middleware(mux))
			if result := f.request("POST", "/api/report", map[string]string{"app": "audit"}, true); result.Code != 200 || notifier.count() != 1 {
				t.Fatal("current session could not send report")
			}
			notifier.sent = nil
			raw, _ := json.Marshal(map[string]string{"app": "audit"})
			body := &auditPausedBody{Reader: bytes.NewReader(raw), entered: make(chan struct{}), release: make(chan struct{})}
			release := sync.OnceFunc(func() { close(body.release) })
			t.Cleanup(release)
			r := httptest.NewRequest("POST", "http://anchor.test/api/report", nil)
			r.Body = body
			r.RemoteAddr = "192.0.2.10:1234"
			r.Header.Set("Origin", "http://anchor.test")
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-CSRF-Token", f.csrf)
			r.AddCookie(f.cookie)
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { defer close(done); f.handler.ServeHTTP(w, r) }()
			select {
			case <-body.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("report did not decode")
			}
			if revoke == "logout" {
				if result := f.request("POST", "/api/auth/logout", map[string]any{}, true); result.Code != 200 {
					t.Fatal("logout failed")
				}
			} else if _, err := f.state.db.Exec(`UPDATE sessions SET expires=? WHERE hash=?`, time.Now().Add(-time.Second).Unix(), tokenHash(f.cookie.Value)); err != nil {
				t.Fatal(err)
			}
			release()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("report did not finish")
			}
			if w.Code != 403 || notifier.count() != 0 {
				t.Fatalf("revoked session sent report: HTTP %d deliveries=%d", w.Code, notifier.count())
			}
		})
	}
}
