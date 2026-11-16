package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

const apiConfig = `
title: Test
contact: Alex
people:
  admin@example.com: {name: Admin, groups: [admin]}
  mom@example.com:   {name: Mom, groups: [family]}
  dad@example.com:   {name: Dad, groups: [nope]}
apps:
  - {name: Seerr, url: https://seerr.test/, access: [family], check: "off"}
  - {name: Other, url: https://other.test/, access: [nope], check: "off"}
requests: {app: seerr, seerr: 'http://s:1', plex: 'http://p:1'}
`

const apiConfigOff = `
people:
  mom@example.com: {name: Mom, groups: [family]}
apps:
  - {name: Seerr, url: https://seerr.test/, access: [family], check: "off"}
`

// api is a handler with the two routes, behind the same middleware as the
// real server, and a service whose refresh is faked.
type api struct {
	h     http.Handler
	rs    *requestStatus
	clock *clock
	fb    *fakeBuild
}

func newAPI(t *testing.T, cfg string) *api {
	t.Helper()
	store := storeFor(t, cfg)
	ck := &clock{t: t0}
	fb := &fakeBuild{}
	rs := newRequestStatus(store, requestKeys{}, false)
	rs.now, rs.build = ck.Now, func(ctx context.Context, c *Config) (*snapshot, error) {
		snap, err := fb.build(ctx, c)
		if snap != nil {
			snap.Rows = []requestRow{{Title: "Dune", Year: 2024, Kind: "movie", State: "soon", Date: "2026-11-12", Detail: "Radarr: secret detail"}}
			snap.Problems = []string{"Sonarr answered 401 Unauthorized"}
			snap.Coming = &comingList{
				Rows:    []comingRow{{Title: "Slow Burn", Year: 2026, Kind: "show", Date: "2026-10-08", Note: "episode"}},
				Missing: []string{"movies"},
			}
		}
		return snap, err
	}
	rs.forced.now = ck.Now
	reads := newRateLimiter(2*time.Second, 3600)
	reads.now = ck.Now
	h := &handler{store: store, checker: NewChecker(store), requests: rs, reqReads: reads}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/me", h.me)
	mux.HandleFunc("GET /api/requests", h.requestsList)
	mux.HandleFunc("POST /api/requests/refresh", h.requestsRefresh)
	return &api{h: blockFunnel(securityHeaders(mux)), rs: rs, clock: ck, fb: fb}
}

type reqOpt func(*http.Request)

func from(addr string) reqOpt   { return func(r *http.Request) { r.RemoteAddr = addr } }
func header(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }
func as(login string) reqOpt    { return header("Tailscale-User-Login", login) }
func withBody(body string) reqOpt {
	return func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader(body)) }
}
func without(k string) reqOpt { return func(r *http.Request) { r.Header.Del(k) } }

func (a *api) get(path string, opts ...reqOpt) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = loopback
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	return rec
}

// post sends Refresh now the way the page does.
func (a *api) post(opts ...reqOpt) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/requests/refresh", strings.NewReader("{}"))
	req.RemoteAddr = loopback
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://example.com")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	return rec
}

func decodeRequests(t *testing.T, rec *httptest.ResponseRecorder) apiRequests {
	t.Helper()
	var out apiRequests
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("bad JSON (%d): %v", rec.Code, err)
	}
	return out
}

// ---------- who may ask ----------

func TestEveryEndpointChecksWhoIsAskingAndTheirAccess(t *testing.T) {
	a := newAPI(t, apiConfig)
	type caller func(opts ...reqOpt) *httptest.ResponseRecorder
	endpoints := map[string]caller{
		"GET /api/requests": func(o ...reqOpt) *httptest.ResponseRecorder { return a.get("/api/requests", o...) },
		"POST refresh":      a.post,
	}
	denied := map[string][]reqOpt{
		"nobody":                      nil,
		"a stranger":                  {as("stranger@x.com")},
		"known, but no Seerr access":  {as("dad@example.com")},
		"a forged login from the LAN": {as("mom@example.com"), from("192.168.1.9:5000")},
		"a forged admin from the LAN": {as("admin@example.com"), from("[2001:db8::1]:5000")},
	}
	for ename, call := range endpoints {
		var firstBody string
		for who, opts := range denied {
			rec := call(opts...)
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s as %s: got %d, want 404", ename, who, rec.Code)
			}
			// The same answer for every reason, so nothing leaks.
			if firstBody == "" {
				firstBody = rec.Body.String()
			} else if rec.Body.String() != firstBody {
				t.Errorf("%s as %s: a different answer from the others", ename, who)
			}
		}
		if rec := call(as("mom@example.com")); rec.Code != http.StatusOK {
			t.Errorf("%s as Mom: got %d (%s)", ename, rec.Code, rec.Body.String())
		}
		if rec := call(as("admin@example.com")); rec.Code != http.StatusOK {
			t.Errorf("%s as administrator without family membership: got %d (%s)", ename, rec.Code, rec.Body.String())
		}
	}
	if a.fb.calls.Load() > 1 {
		t.Errorf("denied callers caused %d refreshes", a.fb.calls.Load())
	}
}

func TestDisabledRequestsApplicationBlocksAdministrators(t *testing.T) {
	a := newAPI(t, strings.Replace(apiConfig, "name: Seerr,", "name: Seerr, disabled: true,", 1))
	for _, rec := range []*httptest.ResponseRecorder{a.get("/api/requests", as("admin@example.com")), a.post(as("admin@example.com"))} {
		if rec.Code != http.StatusNotFound {
			t.Fatalf("disabled Request status returned %d", rec.Code)
		}
	}
	if a.fb.calls.Load() != 0 {
		t.Fatal("disabled application fetched upstream data")
	}
}

func TestTheFeatureIsInvisibleWhenNotConfigured(t *testing.T) {
	a := newAPI(t, apiConfigOff)
	// No requests: block, so even a known person with access gets a 404.
	// (The service exists because main always builds one; the gate says no.)
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"list":    a.get("/api/requests", as("mom@example.com")),
		"refresh": a.post(as("mom@example.com")),
	} {
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: got %d", name, rec.Code)
		}
	}
	if a.fb.calls.Load() != 0 {
		t.Error("an unconfigured feature refreshed")
	}
}

func TestFunnelTrafficGetsNothing(t *testing.T) {
	a := newAPI(t, apiConfig)
	if rec := a.get("/api/requests", as("mom@example.com"), header("Tailscale-Funnel-Request", "?1")); rec.Code != http.StatusForbidden {
		t.Errorf("list via Funnel: got %d", rec.Code)
	}
	if rec := a.post(as("mom@example.com"), header("Tailscale-Funnel-Request", "?1")); rec.Code != http.StatusForbidden {
		t.Errorf("refresh via Funnel: got %d", rec.Code)
	}
	if a.fb.calls.Load() != 0 {
		t.Error("Funnel traffic caused a refresh")
	}
}

func TestPreviewAsIsIgnored(t *testing.T) {
	a := newAPI(t, apiConfig)
	// Mom asking to be treated as the admin gets Mom's view: no admin detail.
	rec := a.get("/api/requests?as=admin@example.com", as("mom@example.com"))
	if strings.Contains(rec.Body.String(), "secret detail") || strings.Contains(rec.Body.String(), "problems") {
		t.Fatalf("?as= gave a non-admin the admin view: %s", rec.Body.String())
	}
	// And an admin previewing as Dad (who has no access) still asks as themselves.
	if rec := a.get("/api/requests?as=dad@example.com", as("admin@example.com")); rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
}

// ---------- Refresh now has to come from Anchor's own page ----------

func TestRefreshNowOnlyWorksFromAnchorsOwnPage(t *testing.T) {
	a := newAPI(t, apiConfig)
	mom := as("mom@example.com")
	cases := map[string]struct {
		opts []reqOpt
		want int
	}{
		"the page itself":             {nil, http.StatusOK},
		"no Origin":                   {[]reqOpt{without("Origin")}, http.StatusForbidden},
		"another site's Origin":       {[]reqOpt{header("Origin", "https://evil.test")}, http.StatusForbidden},
		"cross-site fetch metadata":   {[]reqOpt{header("Sec-Fetch-Site", "cross-site")}, http.StatusForbidden},
		"same-site (a sibling app)":   {[]reqOpt{header("Sec-Fetch-Site", "same-site")}, http.StatusForbidden},
		"a form post":                 {[]reqOpt{header("Content-Type", "application/x-www-form-urlencoded")}, http.StatusUnsupportedMediaType},
		"plain text":                  {[]reqOpt{header("Content-Type", "text/plain")}, http.StatusUnsupportedMediaType},
		"no content type":             {[]reqOpt{without("Content-Type")}, http.StatusUnsupportedMediaType},
		"a huge body is just ignored": {[]reqOpt{withBody(strings.Repeat("x", 1<<20))}, http.StatusOK},
	}
	for name, c := range cases {
		a2 := newAPI(t, apiConfig) // fresh limits for each case
		rec := a2.post(append([]reqOpt{mom}, c.opts...)...)
		if rec.Code != c.want {
			t.Errorf("%s: got %d, want %d", name, rec.Code, c.want)
		}
	}
	// A stranger with a bad Origin still just gets the 404.
	if rec := a.post(as("stranger@x.com"), header("Origin", "https://evil.test")); rec.Code != http.StatusNotFound {
		t.Errorf("stranger with a bad origin: got %d", rec.Code)
	}
}

// ---------- never cached ----------

func TestEveryAnswerIsNoStore(t *testing.T) {
	a := newAPI(t, apiConfig)
	a.fb.fail.Store(true)
	answers := map[string]*httptest.ResponseRecorder{
		"404 stranger":     a.get("/api/requests", as("stranger@x.com")),
		"404 refresh":      a.post(as("stranger@x.com")),
		"403 bad origin":   a.post(as("mom@example.com"), header("Origin", "https://evil.test")),
		"415 bad type":     a.post(as("mom@example.com"), header("Content-Type", "text/plain")),
		"503 nothing yet":  a.get("/api/requests", as("admin@example.com")),
		"429 too many":     a.get("/api/requests", as("admin@example.com")),
		"403 funnel":       a.get("/api/requests", as("mom@example.com"), header("Tailscale-Funnel-Request", "?1")),
		"405 wrong method": func() *httptest.ResponseRecorder { return a.get("/api/requests/refresh", as("mom@example.com")) }(),
	}
	a.fb.fail.Store(false)
	a.clock.advance(2 * time.Minute) // past the back-off
	answers["200 list"] = a.get("/api/requests", as("mom@example.com"))
	answers["200 refresh"] = a.post(as("mom@example.com"))
	answers["200 cool-down"] = a.post(as("mom@example.com"))

	for name, rec := range answers {
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s (%d): Cache-Control %q", name, rec.Code, cc)
		}
	}
	for name, want := range map[string]int{"429 too many": 429, "503 nothing yet": 503, "200 list": 200} {
		if answers[name].Code != want {
			t.Errorf("%s: got %d, want %d", name, answers[name].Code, want)
		}
	}
}

// ---------- reading the list ----------

func TestTheListShape(t *testing.T) {
	a := newAPI(t, apiConfig)
	rec := a.get("/api/requests", as("mom@example.com"))
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("%d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	out := decodeRequests(t, rec)
	if len(out.Rows) != 1 || out.Rows[0].Title != "Dune" || out.Rows[0].State != "soon" || out.Rows[0].Date != "2026-11-12" {
		t.Fatalf("rows: %+v", out.Rows)
	}
	if out.Updated != t0.UTC().Format(time.RFC3339) || out.Age != 0 || !out.PlexOK || out.Stale || out.Message != "" {
		t.Fatalf("meta: %+v", out)
	}
	if out.Rows[0].Detail != "" || out.Problems != nil {
		t.Fatalf("a family member saw admin details: %+v", out)
	}
	var raw map[string]any
	json.Unmarshal(rec.Body.Bytes(), &raw)
	for _, k := range []string{"problems", "stale", "message"} {
		if _, ok := raw[k]; ok {
			t.Errorf("key %q should be left out when empty", k)
		}
	}
}

func TestOnlyAdminsSeeDetailsAndProblems(t *testing.T) {
	a := newAPI(t, apiConfig)
	out := decodeRequests(t, a.get("/api/requests", as("admin@example.com")))
	if out.Rows[0].Detail != "Radarr: secret detail" || len(out.Problems) != 1 {
		t.Fatalf("admin: %+v", out)
	}
	// Reading as the admin must not have changed what the snapshot holds.
	out = decodeRequests(t, a.get("/api/requests", as("mom@example.com")))
	if out.Rows[0].Detail != "" || out.Problems != nil {
		t.Fatalf("a family member now sees admin details: %+v", out)
	}
}

func TestATitleIsJustTextInTheAnswer(t *testing.T) {
	a := newAPI(t, apiConfig)
	hostile := "<img src=x onerror=alert(1)>&\"'"
	a.rs.build = func(context.Context, *Config) (*snapshot, error) {
		return &snapshot{Rows: []requestRow{{Title: hostile, Kind: "movie", State: "ready"}}, PlexOK: true}, nil
	}
	rec := a.get("/api/requests", as("mom@example.com"))
	body := rec.Body.String()
	if strings.Contains(body, "<img") || strings.Contains(body, "<script") {
		t.Fatalf("raw markup in the answer: %s", body)
	}
	if got := decodeRequests(t, rec).Rows[0].Title; got != hostile {
		t.Fatalf("title changed on the way: %q", got)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("nosniff missing")
	}
}

func TestComingUpRidesWithTheList(t *testing.T) {
	a := newAPI(t, apiConfig)
	for _, who := range []string{"mom@example.com", "admin@example.com"} {
		out := decodeRequests(t, a.get("/api/requests", as(who)))
		if out.Coming == nil || len(out.Coming.Rows) != 1 || !reflect.DeepEqual(out.Coming.Rows[0], comingRow{Title: "Slow Burn", Year: 2026, Kind: "show", Date: "2026-10-08", Note: "episode"}) {
			t.Fatalf("%s: coming %+v", who, out.Coming)
		}
		if !reflect.DeepEqual(out.Coming.Missing, []string{"movies"}) {
			t.Fatalf("%s: missing %v", who, out.Coming.Missing)
		}
		a.clock.advance(3 * time.Second) // reads are limited to one per 2 seconds each
	}
	// Refresh now carries it too.
	if out := decodeRequests(t, a.post(as("mom@example.com"))); out.Coming == nil || len(out.Coming.Rows) != 1 {
		t.Fatalf("refresh: %+v", out.Coming)
	}
}

func TestComingUpIsLeftOutWhenThereIsNothingToBuildItFrom(t *testing.T) {
	a := newAPI(t, apiConfig)
	a.rs.build = func(context.Context, *Config) (*snapshot, error) {
		return &snapshot{Rows: []requestRow{{Title: "Dune", Kind: "movie", State: "ready"}}, PlexOK: true}, nil
	}
	var raw map[string]any
	json.Unmarshal(a.get("/api/requests", as("mom@example.com")).Body.Bytes(), &raw)
	if _, ok := raw["coming"]; ok {
		t.Fatalf("with no Sonarr or Radarr the page must not get a Coming up tab: %v", raw)
	}
}

func TestAComingUpTitleIsJustTextToo(t *testing.T) {
	a := newAPI(t, apiConfig)
	hostile := "<img src=x onerror=alert(1)>"
	a.rs.build = func(context.Context, *Config) (*snapshot, error) {
		return &snapshot{PlexOK: true, Coming: &comingList{Rows: []comingRow{{Title: hostile, Kind: "movie", Date: "2026-10-08", Note: "home"}}}}, nil
	}
	rec := a.get("/api/requests", as("mom@example.com"))
	if strings.Contains(rec.Body.String(), "<img") {
		t.Fatalf("raw markup in the answer: %s", rec.Body.String())
	}
	if got := decodeRequests(t, rec).Coming.Rows[0].Title; got != hostile {
		t.Fatalf("title changed on the way: %q", got)
	}
}

func TestReadsAreRateLimitedPerPerson(t *testing.T) {
	a := newAPI(t, apiConfig)
	if rec := a.get("/api/requests", as("mom@example.com")); rec.Code != 200 {
		t.Fatalf("first read: %d", rec.Code)
	}
	if rec := a.get("/api/requests", as("mom@example.com")); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("an immediate second read: %d", rec.Code)
	}
	if rec := a.get("/api/requests", as("admin@example.com")); rec.Code != 200 {
		t.Fatalf("somebody else's read: %d", rec.Code)
	}
	if a.fb.calls.Load() != 1 {
		t.Fatalf("%d refreshes", a.fb.calls.Load())
	}
}

// ---------- Refresh now ----------

func TestRefreshNowAnswers(t *testing.T) {
	a := newAPI(t, apiConfig)
	mom := as("mom@example.com")
	a.get("/api/requests", mom) // first build

	a.clock.advance(12 * time.Second)
	out := decodeRequests(t, a.post(mom))
	if out.Message != "Already up to date. Checked 12 seconds ago." || out.Age != 12 || a.fb.calls.Load() != 1 {
		t.Fatalf("too soon: %q, %d builds", out.Message, a.fb.calls.Load())
	}
	if len(out.Rows) != 1 {
		t.Fatal("a cool-down should still send the current list")
	}

	a.clock.advance(30 * time.Second)
	out = decodeRequests(t, a.post(mom))
	if out.Message != "" || a.fb.calls.Load() != 2 || out.Updated != t0.Add(42*time.Second).UTC().Format(time.RFC3339) {
		t.Fatalf("a real refresh: %+v, %d builds", out, a.fb.calls.Load())
	}

	a.clock.advance(3 * time.Second)
	if out = decodeRequests(t, a.post(mom)); out.Message != "Already up to date. Checked just now." {
		t.Fatalf("message %q", out.Message)
	}
	a.clock.advance(2 * time.Second)
	a.clock.advance(100 * time.Second)
	rec := a.post(mom)
	if out = decodeRequests(t, rec); a.fb.calls.Load() != 3 || out.Message != "" {
		t.Fatalf("after the gap: %+v", out)
	}
}

func TestWhenRefreshingFails(t *testing.T) {
	a := newAPI(t, apiConfig)
	mom, admin := as("mom@example.com"), as("admin@example.com")

	// Nothing has ever worked: a 503, and only the admin is told why.
	a.fb.fail.Store(true)
	rec := a.get("/api/requests", mom)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "Seerr could not be reached") {
		t.Fatalf("a family member was shown the technical reason: %s", rec.Body.String())
	}
	if rec = a.get("/api/requests", admin); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "Seerr could not be reached: nope") {
		t.Fatalf("the admin should be told why: %d %s", rec.Code, rec.Body.String())
	}

	// It worked, then broke: the old list stays, marked stale.
	a.fb.fail.Store(false)
	a.clock.advance(2 * time.Minute)
	a.get("/api/requests", admin) // builds
	a.clock.advance(6 * time.Minute)
	a.fb.fail.Store(true)
	out := decodeRequests(t, a.get("/api/requests", mom))
	if !out.Stale || len(out.Rows) != 1 {
		t.Fatalf("stale: %+v", out)
	}

	// Refresh now while the services are being left alone.
	out = decodeRequests(t, a.post(mom))
	if out.Message != "Couldn't refresh just now. Please try again in a minute." || !out.Stale {
		t.Fatalf("refresh during back-off: %+v", out)
	}
}

func TestErrorsFromTheServiceAreNotLeaked(t *testing.T) {
	a := newAPI(t, apiConfig)
	a.rs.build = func(context.Context, *Config) (*snapshot, error) {
		return nil, errors.New("Plex could not be reached: dial tcp 192.168.1.5:32400: refused")
	}
	rec := a.get("/api/requests", as("mom@example.com"))
	if rec.Code != 503 || strings.Contains(rec.Body.String(), "192.168") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// ---------- the tile ----------

func TestOnlyTheConfiguredTileGetsTheSection(t *testing.T) {
	a := newAPI(t, apiConfig)
	var me apiMe
	rec := a.get("/api/me", as("mom@example.com"))
	if err := json.NewDecoder(rec.Body).Decode(&me); err != nil {
		t.Fatal(err)
	}
	if app := findApp(me, "Seerr"); app == nil || !app.RequestStatus {
		t.Fatalf("Seerr should offer Request status: %+v", app)
	}
	if app := findApp(me, "Other"); app != nil {
		t.Fatalf("Mom shouldn't see Other: %+v", app)
	}
	// Dad has no access to the Seerr tile, so there is nothing to offer him.
	me = apiMe{}
	json.NewDecoder(a.get("/api/me", as("dad@example.com")).Body).Decode(&me)
	if app := findApp(me, "Seerr"); app != nil {
		t.Fatalf("Dad shouldn't see Seerr: %+v", app)
	}
	if app := findApp(me, "Other"); app == nil || app.RequestStatus {
		t.Fatalf("Other shouldn't offer Request status: %+v", app)
	}
	// Not configured: no section anywhere.
	b := newAPI(t, apiConfigOff)
	me = apiMe{}
	json.NewDecoder(b.get("/api/me", as("mom@example.com")).Body).Decode(&me)
	if app := findApp(me, "Seerr"); app == nil || app.RequestStatus {
		t.Fatalf("no requests: block, no section: %+v", app)
	}
}

// ---------- all the way through ----------

func TestEndToEndThroughTheRealPipeline(t *testing.T) {
	st := newStack(t)
	store := storeFor(t, `
people:
  mom@example.com: {name: Mom, groups: [family]}
  admin@example.com: {name: Admin, groups: [admin, family]}
apps:
  - {name: Seerr, url: https://seerr.test/, access: [family], check: "off"}
`+st.block())
	rs := newRequestStatus(store, stackKeys, true)
	rs.now = func() time.Time { return t0 }
	h := &handler{store: store, checker: NewChecker(store), requests: rs, reqReads: newRateLimiter(time.Millisecond, 3600)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/requests", h.requestsList)
	a := &api{h: blockFunnel(securityHeaders(mux))}

	rec := a.get("/api/requests", as("mom@example.com"))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	out := decodeRequests(t, rec)
	if len(out.Rows) != 6 || out.Rows[0].Title != "Ready Movie" || out.Rows[0].State != "ready" || out.Rows[1].State != "partial" {
		t.Fatalf("rows: %+v", out.Rows)
	}
	if out.Rows[5].Title != "<script>alert(1)</script>Evil" {
		t.Errorf("hostile title: %q", out.Rows[5].Title)
	}
	body := rec.Body.String()
	for _, secret := range []string{stackKeys.Seerr, stackKeys.Plex, stackKeys.Sonarr, stackKeys.Radarr, st.seerr.URL, st.plex.URL} {
		if strings.Contains(body, secret) {
			t.Errorf("the answer contains %q", secret)
		}
	}
	if strings.Contains(body, "<script") {
		t.Error("raw markup in the answer")
	}
}
