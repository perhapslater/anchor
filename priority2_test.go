package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNtfyRefusesRedirectsAndRedactsErrors(t *testing.T) {
	for _, target := range []string{"https://ntfy.example.com/other", "https://child.ntfy.example.com/collect", "https://elsewhere.test/collect"} {
		for _, code := range []int{301, 302, 307, 308} {
			h, _ := newFeatureHandler(t, featureConfig)
			n := newNtfyNotifier(h.store, "dummy-sensitive-token")
			var calls atomic.Int32
			n.client.Transport = ebookDownloadTransport(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				res := ebookReply(code, []byte("dummy-sensitive-token"))
				res.Header.Set("Location", target)
				return res, nil
			})
			err := n.Send(context.Background(), Notification{Message: "private payload"})
			if err == nil || calls.Load() != 1 || strings.Contains(err.Error(), "dummy-sensitive") {
				t.Fatalf("redirect: calls=%d err=%v", calls.Load(), err)
			}
		}
	}
	h, _ := newFeatureHandler(t, featureConfig)
	n := newNtfyNotifier(h.store, "dummy-sensitive-token")
	if tr := n.client.Transport.(*http.Transport); tr.Proxy != nil {
		t.Fatal("ntfy uses environment proxy")
	}
	for _, networkFailure := range []bool{false, true} {
		n.client.Transport = ebookDownloadTransport(func(*http.Request) (*http.Response, error) {
			if networkFailure {
				return nil, errors.New("URL contains dummy-sensitive-token")
			}
			res := ebookReply(401, []byte("echo dummy-sensitive-token"))
			res.Status = "401 dummy-sensitive-token"
			return res, nil
		})
		err := n.Send(context.Background(), Notification{})
		if err == nil || strings.Contains(err.Error(), "dummy-sensitive-token") {
			t.Fatalf("unsafe ntfy error: %v", err)
		}
	}
}

func TestNotifyConfigurationRejectsCredentialAndQueryURLs(t *testing.T) {
	for _, server := range []string{"https://user:password@ntfy.test", "https://ntfy.test?token=secret", "https://ntfy.test/#fragment"} {
		if _, err := parseConfig([]byte("notify: {server: '" + server + "', topic: alerts}\n")); err == nil {
			t.Fatal("unsafe notify URL accepted")
		}
	}
}

func TestFailedReportDoesNotClaimDeliveryOnRetry(t *testing.T) {
	h, fn := newFeatureHandler(t, featureConfig)
	fn.err = errors.New("mock failure")
	if code := postReport(h, "mom@example.com", "photos", nil).Code; code != 502 {
		t.Fatal(code)
	}
	retry := postReport(h, "mom@example.com", "photos", nil)
	if retry.Code != 429 || strings.Contains(retry.Body.String(), "already knows") || !strings.Contains(retry.Body.String(), "attempted") {
		t.Fatalf("false acknowledgement: %s", retry.Body)
	}
}

func TestUndeliveredAlertsRetryWithCooldownAndIgnoreLateResults(t *testing.T) {
	h, _ := newFeatureHandler(t, featureConfig)
	c := h.checker
	ck := &clock{t: time.Now()}
	c.now = ck.Now
	app := *h.store.cfg.appByID("photos")
	var attempts int
	var callbacks []func(bool)
	c.send = func(_ Notifier, _ Notification, done func(bool)) { attempts++; callbacks = append(callbacks, done) }
	c.set(app, "down", "offline", true)
	c.set(app, "down", "offline", true)
	if attempts != 1 || !c.Get(app.ID).delivery.pending || c.Get(app.ID).delivery.sent {
		t.Fatal("attempt marked delivered")
	}
	callbacks[0](false)
	for i := 0; i < 3; i++ {
		c.set(app, "down", "offline", true)
	}
	if attempts != 1 {
		t.Fatal("failure bypassed alert cooldown")
	}
	ck.advance(5 * time.Minute)
	c.set(app, "down", "offline", true)
	if attempts != 2 {
		t.Fatal("failed down alert never retried")
	}
	callbacks[1](true)
	if !c.Get(app.ID).delivery.sent {
		t.Fatal("publication not recorded")
	}
	c.set(app, "up", "ok", false)
	if attempts != 3 || c.Get(app.ID).delivery.sent {
		t.Fatal("recovery state incorrect")
	}
	// The previous outage callback must not resurrect delivery after recovery.
	callbacks[1](true)
	if c.Get(app.ID).delivery.sent {
		t.Fatal("late callback changed newer state")
	}
}

func TestAllDownUndeliveredAlertRetries(t *testing.T) {
	h, _, fp := setupAllDown(t, allDownConfig)
	c := h.checker
	ck := &clock{t: time.Now()}
	c.now = ck.Now
	for _, id := range []string{"photos", "books", "movies"} {
		fp.set(id, down)
	}
	attempts := 0
	c.send = func(_ Notifier, _ Notification, done func(bool)) {
		attempts++
		if done != nil {
			done(false)
		}
	}
	h.runRound()
	h.runRound()
	h.runRound()
	if attempts != 1 {
		t.Fatal("all-down alert not bounded")
	}
	ck.advance(5 * time.Minute)
	h.runRound()
	if attempts != 2 {
		t.Fatal("all-down failure not retried")
	}
}

func TestConfigWatchReportsMissingReadFailureAndRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "anchor.yaml")
	data := []byte(testConfig)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := NewConfigStore(path)
	if err != nil {
		t.Fatal(err)
	}
	original, _ := s.Get()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if !s.poll() {
		t.Fatal("missing config change was silent")
	}
	current, message := s.Get()
	if current != original || !strings.Contains(message, "missing") {
		t.Fatal("last good config/error lost")
	}
	if s.poll() {
		t.Fatal("same error repeatedly notified")
	}
	// A directory at the file path reliably produces a read failure on both
	// Windows and root Linux, unlike chmod-only tests.
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if !s.poll() {
		t.Fatal("read failure was silent")
	}
	if _, message = s.Get(); !strings.Contains(message, "could not be read") {
		t.Fatal(message)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	// Recovery must be observed even with the old size and mtime.
	if err := os.Chtimes(path, s.modTime, s.modTime); err != nil {
		t.Fatal(err)
	}
	if !s.poll() {
		t.Fatal("restored same-metadata file not reloaded")
	}
	if _, message = s.Get(); message != "" {
		t.Fatal("error did not clear: " + message)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { s.WatchContext(ctx, func() {}); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watcher did not stop")
	}
}
