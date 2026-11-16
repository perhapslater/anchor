package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const featureConfig = `
title: Test
contact: Alex
people:
  admin@example.com: {name: Admin, groups: [admin, family]}
  wife@example.com:  {name: Wife, groups: [wife, family]}
  mom@example.com:   {name: Mom, groups: [mom, family]}
notice:
  message: "Plex is down tonight"
  style: warning
  access: [mom]
notify:
  server: https://ntfy.example.com
  topic: anchor_alert
apps:
  - {name: Photos, url: https://photos.test/, access: [family], check: "off"}
  - {name: Cameras, url: https://nvr.test/, access: [wife, admin], check: "off"}
  - {name: Books, url: https://books.test/, access: [family], check: "off", maintenance: "Being upgraded, back tonight"}
  - {name: Quiet, url: https://quiet.test/, access: [family], check: "off", alerts: false}
`

type fakeNotifier struct {
	mu   sync.Mutex
	sent []Notification
	err  error
}

func (f *fakeNotifier) Send(_ context.Context, n Notification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, n)
	return nil
}

func (f *fakeNotifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

// waitCount waits for async sends to land.
func (f *fakeNotifier) waitCount(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.count() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("want %d notifications, got %d", want, f.count())
}

func newFeatureHandler(t *testing.T, cfg string) (*handler, *fakeNotifier) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "anchor.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := NewConfigStore(filepath.Join(dir, "anchor.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, e := store.Get(); e != "" {
		t.Fatalf("config error: %s", e)
	}
	fn := &fakeNotifier{}
	ch := NewChecker(store)
	ch.notifier = fn
	return &handler{store: store, checker: ch, notifier: fn, reports: newRateLimiter(10*time.Minute, 30)}, fn
}

// ---------- notice ----------

func TestNoticeForms(t *testing.T) {
	c, err := parseConfig([]byte("notice: Back soon\npeople: {a@b.c: {groups: [x]}}"))
	if err != nil || c.Notice == nil || c.Notice.Message != "Back soon" {
		t.Fatalf("string form: %v %+v", err, c.Notice)
	}
	c, err = parseConfig([]byte("notice: \"\"\n"))
	if err != nil || c.Notice != nil {
		t.Fatalf("empty notice should mean no banner: %v %+v", err, c.Notice)
	}
	if _, err := parseConfig([]byte("notice: {message: hi, style: shouty}")); err == nil {
		t.Fatal("bad style should be rejected")
	}
	c, _ = parseConfig([]byte("apps: []"))
	if c.Notice != nil {
		t.Fatal("no notice key should mean no banner")
	}
}

func TestNoticeOnlyForItsAudience(t *testing.T) {
	h, _ := newFeatureHandler(t, featureConfig)
	mom := get(t, h, loopback, "mom@example.com", "")
	if mom.Notice == nil || mom.Notice.Message != "Plex is down tonight" || mom.Notice.Style != "warning" {
		t.Fatalf("mom should see the notice: %+v", mom.Notice)
	}
	if wife := get(t, h, loopback, "wife@example.com", ""); wife.Notice != nil {
		t.Fatal("wife shouldn't see a mom-only notice")
	}
	if s := get(t, h, loopback, "stranger@x.com", ""); s.Notice != nil {
		t.Fatal("unknown visitors never see notices")
	}
	// Admin previewing as Mom sees Mom's banner.
	if p := get(t, h, loopback, "admin@example.com", "?as=mom@example.com"); p.Notice == nil {
		t.Fatal("preview as mom should show mom's notice")
	}
}

// ---------- locked tiles ----------

func findApp(m apiMe, name string) *apiApp {
	for _, s := range m.Sections {
		for i := range s.Apps {
			if s.Apps[i].Name == name {
				return &s.Apps[i]
			}
		}
	}
	return nil
}

func TestMaintenanceAndDownLockTiles(t *testing.T) {
	h, _ := newFeatureHandler(t, featureConfig)
	m := get(t, h, loopback, "mom@example.com", "")
	if b := findApp(m, "Books"); b == nil || !b.Locked || b.Maintenance != "Being upgraded, back tonight" {
		t.Fatalf("maintenance app should be locked with message: %+v", b)
	}
	if p := findApp(m, "Photos"); p == nil || p.Locked {
		t.Fatalf("working app shouldn't be locked: %+v", p)
	}
	// Two failed rounds -> down -> locked.
	app := *h.store.cfg.appByID("photos")
	h.checker.set(app, "down", "boom", true)
	h.checker.set(app, "down", "boom", true)
	m = get(t, h, loopback, "mom@example.com", "")
	if p := findApp(m, "Photos"); !p.Locked {
		t.Fatal("down app should be locked")
	}
	h.checker.set(app, "up", "200 OK", false)
	m = get(t, h, loopback, "mom@example.com", "")
	if p := findApp(m, "Photos"); p.Locked {
		t.Fatal("recovered app should unlock")
	}
}

// The admin can still open a down app to fix it, but Preview as must show
// exactly what that person gets: greyed out and not openable.
func TestPreviewShowsDownTilesExactlyAsTheySeeThem(t *testing.T) {
	h, _ := newFeatureHandler(t, featureConfig)
	app := *h.store.cfg.appByID("photos")
	h.checker.set(app, "down", "boom", true)
	h.checker.set(app, "down", "boom", true)

	if p := findApp(get(t, h, loopback, "admin@example.com", ""), "Photos"); !p.Locked || !p.AdminOpen {
		t.Fatalf("admin's own page: want greyed out but openable: %+v", p)
	}
	for who, query := range map[string]string{
		"mom@example.com":   "",
		"admin@example.com": "?as=mom@example.com",
	} {
		if p := findApp(get(t, h, loopback, who, query), "Photos"); !p.Locked || p.AdminOpen {
			t.Errorf("%s%s: want greyed out and not openable: %+v", who, query, p)
		}
	}
	if q := findApp(get(t, h, loopback, "admin@example.com", ""), "Quiet"); q.AdminOpen {
		t.Error("a working app isn't locked, so there's nothing to open past")
	}
}

// The tile describes enabled alerts, without claiming successful delivery.
func TestTilesDescribeConfiguredAlerts(t *testing.T) {
	h, _ := newFeatureHandler(t, featureConfig)
	m := get(t, h, loopback, "mom@example.com", "")
	if p := findApp(m, "Photos"); !p.Alerts {
		t.Error("Photos alerts the admin when it goes down")
	}
	if q := findApp(m, "Quiet"); q.Alerts {
		t.Error("an app with alerts: false must not claim the admin was told")
	}

	cfg := strings.Replace(featureConfig, "notify:\n  server: https://ntfy.example.com\n  topic: anchor_alert\n", "", 1)
	h, _ = newFeatureHandler(t, cfg)
	if p := findApp(get(t, h, loopback, "mom@example.com", ""), "Photos"); p.Alerts {
		t.Error("without notify set up, no app alerts the admin")
	}
}

// ---------- alerts ----------

func TestDownAndRecoveredAlerts(t *testing.T) {
	h, fn := newFeatureHandler(t, featureConfig)
	app := *h.store.cfg.appByID("photos")

	h.checker.set(app, "down", "timeout", true) // first failure: still not red
	time.Sleep(50 * time.Millisecond)
	if fn.count() != 0 {
		t.Fatal("one failure must not alert")
	}
	h.checker.set(app, "down", "timeout", true)
	fn.waitCount(t, 1)
	if !strings.Contains(fn.sent[0].Title, "Photos is down") {
		t.Fatalf("unexpected alert: %+v", fn.sent[0])
	}
	h.checker.set(app, "down", "timeout", true) // still down: no repeat
	time.Sleep(50 * time.Millisecond)
	if fn.count() != 1 {
		t.Fatal("staying down must not repeat the alert")
	}
	h.checker.set(app, "up", "200 OK", false)
	fn.waitCount(t, 2)
	if !strings.Contains(fn.sent[1].Title, "Photos is back") {
		t.Fatalf("expected recovery alert: %+v", fn.sent[1])
	}
}

func TestNoAlertsForOptedOutOrMaintenance(t *testing.T) {
	h, fn := newFeatureHandler(t, featureConfig)
	for _, id := range []string{"quiet", "books"} {
		app := *h.store.cfg.appByID(id)
		h.checker.set(app, "down", "x", true)
		h.checker.set(app, "down", "x", true)
		h.checker.set(app, "up", "ok", false)
	}
	time.Sleep(100 * time.Millisecond)
	if fn.count() != 0 {
		t.Fatalf("alerts: false and maintenance apps must stay quiet, got %d", fn.count())
	}
}

func TestNoAlertsWithoutNotify(t *testing.T) {
	cfg := strings.Replace(featureConfig, "notify:\n  server: https://ntfy.example.com\n  topic: anchor_alert\n", "", 1)
	h, fn := newFeatureHandler(t, cfg)
	app := *h.store.cfg.appByID("photos")
	h.checker.set(app, "down", "x", true)
	h.checker.set(app, "down", "x", true)
	time.Sleep(50 * time.Millisecond)
	if fn.count() != 0 {
		t.Fatal("no notify section means no alerts")
	}
	if m := get(t, h, loopback, "mom@example.com", ""); m.Reports || m.Alerts {
		t.Fatal("report button and alert text must be off without notify")
	}
}

// ---------- report button ----------

func postReport(h *handler, login, app string, mod func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "https://anchor.test/api/report", strings.NewReader(`{"app":"`+app+`"}`))
	req.Host = "anchor.test"
	req.RemoteAddr = loopback
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://anchor.test")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if login != "" {
		req.Header.Set("Tailscale-User-Login", login)
	}
	if mod != nil {
		mod(req)
	}
	rec := httptest.NewRecorder()
	h.report(rec, req)
	return rec
}

func TestReportSendsOnceThenRateLimits(t *testing.T) {
	h, fn := newFeatureHandler(t, featureConfig)
	rec := postReport(h, "mom@example.com", "photos", nil)
	if rec.Code != 200 || fn.count() != 1 {
		t.Fatalf("first report: %d %s (sent %d)", rec.Code, rec.Body, fn.count())
	}
	if !strings.Contains(fn.sent[0].Title, "Mom says Photos isn't working") {
		t.Fatalf("title: %q", fn.sent[0].Title)
	}
	if rec := postReport(h, "mom@example.com", "photos", nil); rec.Code != http.StatusTooManyRequests || fn.count() != 1 {
		t.Fatalf("second report within 10 min should be limited: %d", rec.Code)
	}
	// A different app, or a different person, is a separate report.
	if rec := postReport(h, "wife@example.com", "photos", nil); rec.Code != 200 {
		t.Fatalf("other person: %d", rec.Code)
	}
}

func TestReportGuards(t *testing.T) {
	h, fn := newFeatureHandler(t, featureConfig)
	cases := map[string]struct {
		login, app string
		mod        func(*http.Request)
		want       int
	}{
		"cross-site":        {"mom@example.com", "photos", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, 403},
		"no origin":         {"mom@example.com", "photos", func(r *http.Request) { r.Header.Del("Origin"); r.Header.Del("Sec-Fetch-Site") }, 403},
		"other origin":      {"mom@example.com", "photos", func(r *http.Request) { r.Header.Set("Origin", "https://evil.test"); r.Header.Del("Sec-Fetch-Site") }, 403},
		"form post":         {"mom@example.com", "photos", func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }, 415},
		"unknown person":    {"stranger@x.com", "photos", nil, 403},
		"no identity":       {"", "photos", nil, 403},
		"identity from LAN": {"mom@example.com", "photos", func(r *http.Request) { r.RemoteAddr = "192.168.1.9:5000" }, 403},
		"app not theirs":    {"mom@example.com", "cameras", nil, 404},
		"app doesn't exist": {"mom@example.com", "nope", nil, 404},
		"oversized body":    {"mom@example.com", strings.Repeat("a", 2000), nil, 400},
	}
	for name, c := range cases {
		if rec := postReport(h, c.login, c.app, c.mod); rec.Code != c.want {
			t.Errorf("%s: got %d, want %d (%s)", name, rec.Code, c.want, strings.TrimSpace(rec.Body.String()))
		}
	}
	if fn.count() != 0 {
		t.Fatalf("rejected requests must not notify, sent %d", fn.count())
	}
}

func TestReportWhenNtfyFails(t *testing.T) {
	h, fn := newFeatureHandler(t, featureConfig)
	fn.err = errors.New("ntfy down")
	rec := postReport(h, "mom@example.com", "photos", nil)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "text Alex") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestRateLimiterHourlyCap(t *testing.T) {
	rl := newRateLimiter(time.Minute, 3)
	now := time.Now()
	rl.now = func() time.Time { return now }
	for i, k := range []string{"a", "b", "c"} {
		if !rl.allow(k) {
			t.Fatalf("event %d should pass", i)
		}
	}
	if rl.allow("d") {
		t.Fatal("hourly cap should stop the 4th event")
	}
	now = now.Add(time.Hour)
	if !rl.allow("d") {
		t.Fatal("cap should reset after an hour")
	}
}

// ---------- ntfy wire format ----------

func TestNtfyRequest(t *testing.T) {
	var gotAuth string
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
	}))
	defer srv.Close()

	cfg := strings.Replace(featureConfig, "https://ntfy.example.com", srv.URL, 1)
	h, _ := newFeatureHandler(t, cfg)
	n := newNtfyNotifier(h.store, " tk_secret \n")
	// Wire-format fixture explicitly permits its owned loopback receiver.
	// Production destination restrictions are tested separately.
	n.client = newUpstreamClient(true)
	err := n.Send(context.Background(), Notification{Title: "Mom says Photos isn't working", Message: "hi", Priority: 4, Tags: []string{"raising_hand"}})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer tk_secret" {
		t.Fatalf("auth header: %q", gotAuth)
	}
	if got["topic"] != "anchor_alert" || got["title"] != "Mom says Photos isn't working" || got["priority"] != float64(4) {
		t.Fatalf("body: %v", got)
	}
}

func TestTokenNeverReachesTheBrowser(t *testing.T) {
	t.Setenv("ANCHOR_NTFY_TOKEN", "tk_secret_value")
	h, _ := newFeatureHandler(t, featureConfig)
	for _, login := range []string{"admin@example.com", "mom@example.com"} {
		req := httptest.NewRequest("GET", "/api/me", nil)
		req.RemoteAddr = loopback
		req.Header.Set("Tailscale-User-Login", login)
		rec := httptest.NewRecorder()
		h.me(rec, req)
		if strings.Contains(rec.Body.String(), "tk_secret") || strings.Contains(rec.Body.String(), "ntfy.example.com") {
			t.Fatalf("%s: token or ntfy server leaked to the page", login)
		}
	}
}

func TestMobileServerIsJustTheHost(t *testing.T) {
	c, err := parseConfig([]byte(`
apps:
  - name: Immich
    url: https://photos.example.ts.net/auth/login?autoLaunch=1
    access: [x]
    mobile: {ios: https://apps.apple.com/x}
  - name: Custom
    url: https://a.example.ts.net/deep/link
    access: [x]
    mobile: {server: https://a.example.ts.net/sub}
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Apps[0].Mobile.Server; got != "https://photos.example.ts.net" {
		t.Errorf("default server address: %q", got)
	}
	if got := c.Apps[1].Mobile.Server; got != "https://a.example.ts.net/sub" {
		t.Errorf("explicit server address must be kept: %q", got)
	}
}
