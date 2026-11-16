package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testTailscaleRequest(f *accountFixture, path, host, peer, identity string, cookie bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "https://"+host+path, nil)
	r.RemoteAddr = peer
	r.Header.Set("Tailscale-User-Login", identity)
	if cookie {
		r.AddCookie(f.cookie)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func TestTailscaleTestRunExpiryAndCapacity(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	h := &handler{store: f.state.store, auth: f.auth, checker: NewChecker(f.state.store)}
	mux := http.NewServeMux()
	h.adminRoutes(mux)
	f.handler = externalScheme("http", f.auth.middleware(mux))
	body := map[string]string{"address": "https://anchor.example.ts.net"}
	w := f.request("POST", "/api/admin/tailscale-test", body, true)
	var run struct{ Ticket string }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &run) != nil {
		t.Fatal(w.Body.String())
	}
	h.tailscaleTests.runs[run.Ticket].expires = time.Now().Add(-time.Second)
	if w := f.request("GET", "/api/admin/tailscale-test?ticket="+run.Ticket, nil, false); w.Code != 410 {
		t.Fatal("expired result was still readable")
	}
	for i := 0; i < 64; i++ {
		if w := f.request("POST", "/api/admin/tailscale-test", body, true); w.Code != 200 {
			t.Fatalf("could not start run %d: %d", i, w.Code)
		}
	}
	if w := f.request("POST", "/api/admin/tailscale-test", body, true); w.Code != 429 {
		t.Fatal("pending tests can grow without a bound")
	}
}

func TestTailscaleTestRequiresTrustedIdentityAndUsesDashboardPermissions(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	f.auth.mode, f.auth.publicURL = "tailscale", ""
	member := localUser{ID: "u_test_member", Name: "Member", Username: "member", Tailscale: "member@example.test", Groups: []string{"family"}, Enabled: true}
	if err := f.state.saveUser(member, "pass", f.user.ID); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseConfig([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Apps = append(cfg.Apps, App{ID: "disabled", Name: "Disabled app", URL: "https://disabled.test", Access: []string{"everyone"}, Disabled: true})
	revision, _ := f.state.revision()
	if err := f.state.saveConfig(cfg, revision, f.user.ID); err != nil {
		t.Fatal(err)
	}
	f.user.Tailscale = "admin@example.test"
	if err := f.state.saveUser(f.user, "", f.user.ID); err != nil {
		t.Fatal(err)
	}
	before, _ := f.state.revision()
	for _, tc := range []struct{ name, peer, identity, state string }{
		{"password alone", "127.0.0.1:1234", "", "missing"},
		{"spoofed LAN identity", "192.0.2.10:1234", member.Tailscale, "missing"},
		{"unlinked identity with admin cookie", "127.0.0.1:1234", "unknown@example.test", "unlinked"},
		{"member over Serve with admin cookie", "127.0.0.1:1234", member.Tailscale, "verified"},
		{"member over IPv6 Serve", "[::1]:1234", member.Tailscale, "verified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := testTailscaleRequest(f, "/api/tailscale-test", "anchor.example.ts.net:8443", tc.peer, tc.identity, true)
			var got tailscaleTestReport
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.State != tc.state {
				t.Fatalf("wrong result: %d %s", w.Code, w.Body.String())
			}
			if tc.state == "verified" {
				if got.Name != member.Name || len(got.Groups) != 1 || got.Groups[0] != "family" || len(got.Sections) != 1 || len(got.Sections[0].Apps) != 1 || got.Sections[0].Apps[0].Name != "Photos" {
					t.Fatalf("wrong effective permissions: %+v", got)
				}
			} else if got.Name != "" || len(got.Groups) != 0 || len(got.Sections) != 0 {
				t.Fatal("failed sign-in exposed account permissions")
			}
		})
	}
	w := testTailscaleRequest(f, "/api/tailscale-test", "anchor.example.ts.net", "127.0.0.1:1234", f.user.Tailscale, false)
	var adminReport tailscaleTestReport
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &adminReport) != nil || adminReport.State != "verified" || !equal(names(apiMe{Sections: adminReport.Sections}), []string{"Photos", "Cameras", "Alerts", "Just Mom"}) {
		t.Fatalf("administrator test did not reflect implicit access: %s", w.Body.String())
	}
	member.Enabled = false
	if err := f.state.saveUser(member, "", f.user.ID); err != nil {
		t.Fatal(err)
	}
	w = testTailscaleRequest(f, "/api/tailscale-test", "anchor.example.ts.net", "127.0.0.1:1234", member.Tailscale, false)
	if !strings.Contains(w.Body.String(), `"state":"unlinked"`) || strings.Contains(w.Body.String(), "Photos") {
		t.Fatalf("paused account passed: %s", w.Body.String())
	}
	after, _ := f.state.revision()
	if after != before+1 { // only the deliberate account pause changed settings
		t.Fatal("read-only test changed saved settings")
	}
}

func TestTailscaleTestTicketsScopeResultsToAdminAndTarget(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	f.auth.mode, f.auth.publicURL = "tailscale", ""
	body := map[string]string{"address": "https://anchor.example.ts.net:8443"}
	if w := f.request("POST", "/api/admin/tailscale-test", body, false); w.Code != 403 {
		t.Fatal("test creation bypassed CSRF")
	}
	w := f.request("POST", "/api/admin/tailscale-test", body, true)
	var run struct{ Ticket, URL string }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &run) != nil || run.Ticket == "" || !strings.Contains(run.URL, "#ticket=") {
		t.Fatalf("could not start run: %d %s", w.Code, w.Body.String())
	}
	path := "/api/tailscale-test?ticket=" + run.Ticket
	if w := testTailscaleRequest(f, path, "other.example.ts.net:8443", "127.0.0.1:1234", "", false); w.Code != 400 {
		t.Fatal("different target submitted result")
	}
	if w := f.request("GET", "/api/admin/tailscale-test?ticket="+run.Ticket, nil, false); !strings.Contains(w.Body.String(), `"report":null`) {
		t.Fatal("wrong target recorded a result")
	}
	if w := testTailscaleRequest(f, path, "anchor.example.ts.net:8443", "127.0.0.1:1234", "unknown@example.test", false); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := f.request("GET", "/api/admin/tailscale-test?ticket="+run.Ticket, nil, false); !strings.Contains(w.Body.String(), `"state":"unlinked"`) {
		t.Fatal("setup did not receive test result")
	}
	cookie := f.cookie
	f.cookie = nil
	if w := f.request("GET", "/api/admin/tailscale-test?ticket="+run.Ticket, nil, false); w.Code != 403 {
		t.Fatal("token exposed result without admin authentication")
	}
	f.cookie = cookie
	u := localUser{ID: "u_other_admin", Username: "other-admin", Name: "Other", Groups: []string{"admin"}, Enabled: true}
	if err := f.state.saveUser(u, "pass", f.user.ID); err != nil {
		t.Fatal(err)
	}
	w = f.request("POST", "/api/auth/login", map[string]string{"Username": u.Username, "Password": "pass"}, false)
	f.cookie = w.Result().Cookies()[0]
	if w := f.request("GET", "/api/admin/tailscale-test?ticket="+run.Ticket, nil, false); w.Code != 410 {
		t.Fatal("another administrator read the test result")
	}
	f.cookie = cookie
	if w := f.request("GET", "/api/admin/tailscale-test?ticket="+run.Ticket, nil, false); !strings.Contains(w.Body.String(), `"state":"stale"`) {
		t.Fatal("changed accounts did not invalidate result")
	}
	for _, address := range []string{"http://anchor.example.ts.net", "https://example.com", "https://user:pass@anchor.example.ts.net", "https://anchor.example.ts.net/path", "https://anchor.example.ts.net:0", "https://anchor.example.ts.net:65536"} {
		if w := f.request("POST", "/api/admin/tailscale-test", map[string]string{"address": address}, true); w.Code != 400 {
			t.Errorf("invalid test address accepted: %s", address)
		}
	}
}
