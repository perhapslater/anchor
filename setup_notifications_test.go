package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"
)

type notificationTransport func(*http.Request) (*http.Response, error)

func (f notificationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func notificationResponse(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}
func notificationTestRouter(t *testing.T) (*ConfigStore, *notificationRouter) {
	t.Helper()
	store, err := newPersistentStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.persistent.db.Close() })
	cfg, _ := store.Get()
	cfg.Notify = &Notify{Server: "https://ntfy.example.test", Topic: "anchor"}
	cfg.Webhook = &WebhookConfig{Format: "discord"}
	cfg.credentials["webhook"] = "https://discord.com/api/webhooks/123/private-token"
	return store, newNotificationRouter(store)
}

func TestNotificationFanoutRetriesOnlyFailedDestination(t *testing.T) {
	_, router := notificationTestRouter(t)
	var mu sync.Mutex
	counts := map[string]int{}
	transport := notificationTransport(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		counts[r.URL.Host]++
		count := counts[r.URL.Host]
		mu.Unlock()
		if r.URL.Host == "discord.com" && count == 1 {
			return notificationResponse(500, "private upstream response must not leak"), nil
		}
		return notificationResponse(204, ""), nil
	})
	router.client = &http.Client{Transport: transport}
	router.ntfy.client = router.client
	msg := Notification{Title: "Down", Message: "Details", delivery: &notificationReceipts{}}
	var partial *partialDeliveryError
	if err := router.Send(context.Background(), msg); !errors.As(err, &partial) || partial.delivered != 1 {
		t.Fatalf("expected partial success: %v", err)
	}
	if err := router.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}
	if counts["ntfy.example.test"] != 1 || counts["discord.com"] != 2 {
		t.Fatalf("duplicate delivery: %v", counts)
	}
	if router.statuses()["webhook"].State != "sent" {
		t.Fatal("webhook retry status not recorded")
	}
}

func TestWebhookPayloadsAndSecretRedaction(t *testing.T) {
	store, router := notificationTestRouter(t)
	for _, format := range []string{"discord", "json"} {
		t.Run(format, func(t *testing.T) {
			cfg, _ := store.Get()
			cfg.Notify = nil
			cfg.Webhook.Format = format
			router.client = &http.Client{Transport: notificationTransport(func(r *http.Request) (*http.Response, error) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if format == "discord" {
					if r.URL.Query().Get("wait") != "true" || len(body["allowed_mentions"].(map[string]any)["parse"].([]any)) != 0 {
						t.Error("Discord confirmation/mention protection missing")
					}
					if len(utf16.Encode([]rune(body["content"].(string)))) > 2000 {
						t.Error("Discord content too long")
					}
				} else if body["event"] != "report" || body["schema_version"] != float64(1) || body["url"] != "https://app.test" {
					t.Errorf("JSON contract: %v", body)
				}
				return nil, errors.New("transport failed at " + r.URL.String())
			})}
			err := router.Send(context.Background(), Notification{Kind: "report", Title: strings.Repeat("😀", 2500), Click: "https://app.test"})
			status := router.statuses()["webhook"]
			if err == nil || strings.Contains(err.Error()+status.Message, "private-token") {
				t.Fatalf("webhook secret leaked: %v %v", err, status)
			}
		})
	}
}

func TestNotificationEventSelectionAndRateLimit(t *testing.T) {
	store, router := notificationTestRouter(t)
	cfg, _ := store.Get()
	no := false
	cfg.Notify.Alerts = &no
	cfg.Webhook.Reports = &no
	ntfyCalls, webhookCalls := 0, 0
	router.ntfy.client = &http.Client{Transport: notificationTransport(func(*http.Request) (*http.Response, error) { ntfyCalls++; return notificationResponse(200, ""), nil })}
	router.client = &http.Client{Transport: notificationTransport(func(*http.Request) (*http.Response, error) {
		webhookCalls++
		return notificationResponse(429, `{"retry_after":30}`), nil
	})}
	if err := router.Send(context.Background(), Notification{Kind: "report"}); err != nil {
		t.Fatal(err)
	}
	err := router.Send(context.Background(), Notification{Kind: "alert"})
	var partial *partialDeliveryError
	if !errors.As(err, &partial) || partial.retryAfter != 30*time.Second {
		t.Fatalf("rate limit not propagated: %v", err)
	}
	_ = router.Send(context.Background(), Notification{Kind: "alert"})
	if ntfyCalls != 1 || webhookCalls != 1 {
		t.Fatalf("selection/rate limit: ntfy=%d webhook=%d", ntfyCalls, webhookCalls)
	}
}

func TestWebhookValidationAndExportBoundary(t *testing.T) {
	for _, raw := range []string{"http://example.test/hook", "https://user:pass@example.test/hook", "https://example.test/hook#fragment", "https://%zz/secret"} {
		if _, err := validWebhookURL(raw, "json"); err == nil || strings.Contains(err.Error(), "pass") || strings.Contains(err.Error(), "secret") {
			t.Errorf("unsafe URL/error: %s %v", raw, err)
		}
	}
	if _, err := validWebhookURL("https://example.test/api/webhooks/123/secret", "discord"); err == nil {
		t.Fatal("Discord URL accepted wrong host")
	}
	f := newAccountFixture(t)
	f.setup(t)
	cfg, _ := f.state.store.Get()
	copyCfg := *cfg
	copyCfg.Webhook = &WebhookConfig{Format: "discord"}
	rev, _ := f.state.revision()
	endpoint := "https://discord.com/api/webhooks/123/private-webhook-token"
	if err := f.state.saveConfigAndSecrets(&copyCfg, rev, "test", map[string]string{"webhook": endpoint}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/admin/state", "/api/admin/export", "/api/me"} {
		w := f.request("GET", path, nil, false)
		if w.Code != 200 || strings.Contains(w.Body.String(), endpoint) || strings.Contains(w.Body.String(), "private-webhook-token") {
			t.Fatalf("secret in %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	rev, _ = f.state.revision()
	if err := f.state.saveSecrets(map[string]string{"webhook": ""}, rev, "test"); err == nil {
		t.Fatal("cleared endpoint on enabled webhook")
	}
}

func TestFinishedSetupCannotBeReopened(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	progress, err := f.state.onboarding()
	if err != nil {
		t.Fatal(err)
	}
	progress.Step, progress.Completed = 4, true
	w := f.request("POST", "/api/admin/setup", progress, true)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	finished, err := f.state.onboarding()
	if err != nil || !finished.Completed {
		t.Fatalf("completion was not saved: %v", err)
	}
	// Even a client with the newest revision cannot reopen or edit the wizard.
	for _, change := range []onboardingState{
		{Step: 0, Revision: finished.Revision},
		{Step: 2, Skipped: []int{1}, Revision: finished.Revision},
		{Step: 4, Completed: true, Revision: finished.Revision},
	} {
		if w := f.request("POST", "/api/admin/setup", change, true); w.Code != 409 {
			t.Fatalf("finished setup accepted an update: %d %s", w.Code, w.Body.String())
		}
	}
	after, err := f.state.onboarding()
	if err != nil || after.Revision != finished.Revision || !after.Completed || after.Step != finished.Step || !slices.Equal(after.Skipped, finished.Skipped) {
		t.Fatalf("finished progress changed: %v", err)
	}
	// Ordinary Administration settings stay editable.
	cfg, _ := f.state.store.Get()
	revision, _ := f.state.revision()
	copyCfg := *cfg
	copyCfg.Title = "Updated after setup"
	if err := f.state.saveConfig(&copyCfg, revision, f.user.ID); err != nil {
		t.Fatal(err)
	}
}

func TestSetupCodeLoggingAndProgressSurviveRestart(t *testing.T) {
	f := newAccountFixture(t)
	raw, err := os.ReadFile(filepath.Join(f.state.dir, "setup-token"))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	logger := log.New(&out, "", 0)
	if err := printSetupInstructions(f.state, logger, "http://anchor.test"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), string(raw)) || !strings.Contains(out.String(), "http://anchor.test") {
		t.Fatal("missing first-run setup instructions")
	}
	f.setup(t)
	out.Reset()
	if err := printSetupInstructions(f.state, logger, "http://anchor.test"); err != nil || out.Len() != 0 {
		t.Fatal("setup token logged after claim")
	}
	s, err := f.state.onboarding()
	if err != nil || s.Completed {
		t.Fatalf("new install onboarding: %v %v", s, err)
	}
	s.Step = 3
	s.Skipped = []int{1, 2}
	w := f.request("POST", "/api/admin/setup", s, true)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w = f.request("POST", "/api/admin/setup", s, true); w.Code != 409 {
		t.Fatal("stale setup update accepted")
	}
	second, err := newPersistentStore(f.state.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.persistent.db.Close()
	got, err := second.persistent.onboarding()
	if err != nil || got.Step != 3 || len(got.Skipped) != 2 {
		t.Fatalf("lost progress: %v %v", got, err)
	}
	if w = f.request("POST", "/api/admin/setup", got, false); w.Code != 403 {
		t.Fatal("setup progress CSRF bypass")
	}
	got.Step = 6
	if w = f.request("POST", "/api/admin/setup", got, true); w.Code != 400 {
		t.Fatal("invalid step accepted")
	}
}

func TestWizardOrderMigrationPreservesNamedSteps(t *testing.T) {
	for _, tc := range []struct {
		name       string
		oldStep    int
		oldSkipped []int
		step       int
		skipped    []int
		completed  bool
		version    int
	}{
		{"v2 Tailscale", 1, []int{2, 3}, 3, []int{0, 1}, false, 2},
		{"v2 People", 2, []int{1, 4}, 0, []int{3, 2}, false, 2},
		{"v2 Applications", 3, []int{0, 1, 2}, 1, []int{3, 0}, false, 2},
		{"v2 Completed", 5, []int{1}, 4, []int{3}, true, 2},
		{"v3 Connection", 0, []int{0, 1}, 0, []int{0}, false, 3},
		{"v3 People", 1, []int{0}, 0, []int{}, false, 3},
		{"v3 Tailscale", 2, []int{1}, 3, []int{0}, false, 3},
		{"v3 Applications", 3, []int{2}, 1, []int{3}, false, 3},
		{"v3 Notifications", 4, []int{2, 4}, 2, []int{3, 2}, false, 3},
		{"v3 Completed", 5, []int{}, 4, []int{}, true, 3},
		{"v4 People", 0, []int{1}, 0, []int{3}, false, 4},
		{"v4 Tailscale", 1, []int{2, 3}, 3, []int{1, 2}, false, 4},
		{"v4 Applications", 2, []int{1}, 1, []int{3}, false, 4},
		{"v4 Notifications", 3, []int{1}, 2, []int{3}, false, 4},
		{"v4 Completed", 4, []int{1}, 4, []int{3}, true, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAccountFixture(t)
			f.setup(t)
			configRevision, err := f.state.revision()
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(tc.oldSkipped)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.state.db.Exec(`UPDATE onboarding SET step=?,skipped=?,completed=?,revision=9 WHERE id=1`, tc.oldStep, string(raw), tc.completed); err != nil {
				t.Fatal(err)
			}
			legacyVersion := "PRAGMA user_version=" + strconv.Itoa(tc.version)
			if _, err = f.state.db.Exec(legacyVersion); err != nil {
				t.Fatal(err)
			}
			store, err := newPersistentStore(f.state.dir)
			if err != nil {
				t.Fatal(err)
			}
			defer store.persistent.db.Close()
			got, err := store.persistent.onboarding()
			if err != nil || got.Step != tc.step || !slices.Equal(got.Skipped, tc.skipped) || got.Completed != tc.completed || got.Revision != int64(9+5-tc.version) {
				t.Fatalf("migration lost named progress: %+v %v", got, err)
			}
			if current, err := store.persistent.revision(); err != nil || current != configRevision {
				t.Fatal("wizard migration changed dashboard settings")
			}
			if user, ok := store.persistent.user(f.user.ID); !ok || user.Username != f.user.Username || !user.HasPassword {
				t.Fatal("wizard migration changed administrator")
			}
			stale := onboardingState{Step: 0, Skipped: []int{}, Completed: false, Revision: 9}
			if w := f.request("POST", "/api/admin/setup", stale, true); w.Code != 409 {
				t.Fatalf("old browser indices accepted: %d %s", w.Code, w.Body.String())
			}
			again, err := newPersistentStore(f.state.dir)
			if err != nil {
				t.Fatal(err)
			}
			defer again.persistent.db.Close()
			reopened, err := again.persistent.onboarding()
			if err != nil || reopened.Step != got.Step || !slices.Equal(reopened.Skipped, got.Skipped) || reopened.Revision != got.Revision {
				t.Fatalf("migration ran again on restart: %+v %v", reopened, err)
			}
		})
	}
}

func TestObservedTailscaleIdentityBoundary(t *testing.T) {
	a := &authentication{mode: "tailscale"}
	r := httptest.NewRequest("GET", "https://anchor.test", nil)
	r.Header.Set("Tailscale-User-Login", "Alex@example.test")
	r.RemoteAddr = "127.0.0.1:1234"
	if got := a.observedTailscaleLogin(r); got != "alex@example.test" {
		t.Fatal(got)
	}
	r.RemoteAddr = "192.0.2.1:1234"
	if a.observedTailscaleLogin(r) != "" {
		t.Fatal("trusted non-loopback identity")
	}
	a.mode = "proxy"
	r.RemoteAddr = "127.0.0.1:1234"
	if a.observedTailscaleLogin(r) != "" {
		t.Fatal("trusted proxy identity header")
	}
}

func TestNotificationTestEndpointRequiresAdminCSRFAndExplicitDestination(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	cfg, _ := f.state.store.Get()
	cfg.Notify = &Notify{Server: "https://ntfy.example.test", Topic: "anchor"}
	cfg.Webhook = &WebhookConfig{Format: "json"}
	cfg.credentials["webhook"] = "https://webhook.example.test/private"
	router := newNotificationRouter(f.state.store)
	calls := map[string]int{}
	router.client = &http.Client{Transport: notificationTransport(func(r *http.Request) (*http.Response, error) {
		calls[r.URL.Host]++
		return notificationResponse(204, ""), nil
	})}
	router.ntfy.client = router.client
	h := &handler{auth: f.auth, store: f.state.store, notifier: router, reports: newRateLimiter(time.Minute, 100)}
	mux := http.NewServeMux()
	h.adminRoutes(mux)
	f.handler = externalScheme("http", f.auth.middleware(mux))
	path := "/api/admin/notifications/test"
	body := map[string]string{"destination": "webhook"}
	if w := f.request("POST", path, body, false); w.Code != 403 {
		t.Fatal("missing CSRF accepted")
	}
	if w := f.request("POST", path, map[string]string{"destination": "arbitrary-url"}, true); w.Code != 400 {
		t.Fatal("arbitrary destination accepted")
	}
	if w := f.request("POST", path, body, true); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if calls["webhook.example.test"] != 1 || calls["ntfy.example.test"] != 0 {
		t.Fatalf("test reached wrong destinations: %v", calls)
	}
	if w := f.request("POST", path, body, true); w.Code != 429 {
		t.Fatal("unlimited notification tests")
	}
	f.cookie = nil
	if w := f.request("POST", path, body, true); w.Code != 403 {
		t.Fatal("anonymous test accepted")
	}
}

func TestOutageReceiptsPersistBetweenHealthRetries(t *testing.T) {
	store, router := notificationTestRouter(t)
	counts := map[string]int{}
	var mu sync.Mutex
	router.client = &http.Client{Transport: notificationTransport(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		counts[r.URL.Host]++
		count := counts[r.URL.Host]
		mu.Unlock()
		if r.URL.Host == "discord.com" && count == 1 {
			return notificationResponse(500, ""), nil
		}
		return notificationResponse(204, ""), nil
	})}
	router.ntfy.client = router.client
	checker := NewChecker(store)
	checker.notifier = router
	now := time.Now()
	checker.now = func() time.Time { return now }
	checker.send = func(nt Notifier, msg Notification, done func(bool)) {
		err := nt.Send(context.Background(), msg)
		if done != nil {
			done(err == nil)
		}
	}
	app := App{ID: "test", Name: "Example"}
	checker.set(app, "down", "not responding", true)
	checker.set(app, "down", "not responding", true)
	now = now.Add(6 * time.Minute)
	checker.set(app, "down", "still down", true)
	if counts["ntfy.example.test"] != 1 || counts["discord.com"] != 2 {
		t.Fatalf("health retry duplicated success: %v", counts)
	}
	checker.set(app, "up", "working", false)
	if counts["ntfy.example.test"] != 2 || counts["discord.com"] != 3 {
		t.Fatalf("recovery not sent to both: %v", counts)
	}
}

func TestExistingInstallationCanOpenWizardWithoutForcedOnboarding(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	if _, err := f.state.db.Exec(`DROP TABLE onboarding`); err != nil {
		t.Fatal(err)
	}
	store, err := newPersistentStore(f.state.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.persistent.db.Close()
	s, err := store.persistent.onboarding()
	if err != nil || !s.Completed {
		t.Fatalf("existing installation unexpectedly forced through setup: %v %v", s, err)
	}
}

func TestWebhookClientBlocksLocalAddressesAndRedirects(t *testing.T) {
	store, router := notificationTestRouter(t)
	cfg, _ := store.Get()
	cfg.Notify = nil
	cfg.Webhook.Format = "json"
	for _, address := range []string{"https://127.0.0.1:1/secret", "https://169.254.169.254/secret"} {
		cfg.credentials["webhook"] = address
		if err := router.Send(context.Background(), Notification{Title: "Test"}); err == nil {
			t.Fatal("unsafe webhook destination accepted")
		}
	}
	next, _ := http.NewRequest("POST", "https://other.example.test/", nil)
	if router.client.CheckRedirect(next, nil) != http.ErrUseLastResponse {
		t.Fatal("webhook follows redirects")
	}
}

func TestPartialOutageDeliveryStillSendsRecoveryAndStopsOldRetries(t *testing.T) {
	store, router := notificationTestRouter(t)
	var mu sync.Mutex
	counts := map[string]int{}
	router.client = &http.Client{Transport: notificationTransport(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		counts[r.URL.Host]++
		mu.Unlock()
		if r.URL.Host == "discord.com" {
			return notificationResponse(500, ""), nil
		}
		return notificationResponse(204, ""), nil
	})}
	router.ntfy.client = router.client
	checker := NewChecker(store)
	checker.notifier = router
	var outage Notification
	checker.send = func(nt Notifier, msg Notification, done func(bool)) {
		if strings.HasSuffix(msg.Title, "is down") {
			outage = msg
		}
		err := nt.Send(context.Background(), msg)
		if done != nil {
			done(err == nil)
		}
	}
	app := App{ID: "test", Name: "Example"}
	checker.set(app, "down", "failed", true)
	checker.set(app, "down", "failed", true)
	checker.set(app, "up", "working", false)
	_ = router.Send(context.Background(), outage)
	if counts["ntfy.example.test"] != 2 || counts["discord.com"] != 2 {
		t.Fatalf("missing recovery or stale retry: %v", counts)
	}
}
