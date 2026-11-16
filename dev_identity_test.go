//go:build anchordev

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func identityFixture(t *testing.T) (*accountFixture, *developmentIdentity) {
	t.Helper()
	f := newAccountFixture(t)
	f.setup(t)
	f.user.Tailscale = "admin@example.test"
	if err := f.state.saveUser(f.user, "", f.user.ID); err != nil {
		t.Fatal(err)
	}
	f.user, _ = f.state.user(f.user.ID)
	for _, user := range []localUser{
		{ID: "u_member", Name: "Member", Tailscale: "member@example.test", Groups: []string{"family"}, Enabled: true},
		{ID: "u_disabled", Name: "Disabled", Tailscale: "disabled@example.test", Groups: []string{"family"}},
		{ID: "u_unlinked", Username: "unlinked", Name: "No email linked", Groups: []string{"family"}, Enabled: true},
	} {
		password := ""
		if user.Tailscale == "" {
			password = "synthetic-unlinked-password"
		}
		if err := f.state.saveUser(user, password, f.user.ID); err != nil {
			t.Fatal(err)
		}
	}
	f.auth.mode, f.auth.publicURL = "tailscale", ""
	handler, err := newDevelopmentIdentities(f.handler, f.auth, "127.0.0.1:18080")
	if err != nil {
		t.Fatal(err)
	}
	return f, handler.(*developmentIdentity)
}

func identityRequest(d *developmentIdentity, method, path string, body any, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	data, _ := json.Marshal(body)
	r := httptest.NewRequest(method, "http://127.0.0.1:18080"+path, bytes.NewReader(data))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Origin", "http://127.0.0.1:18080")
	r.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	d.ServeHTTP(w, r)
	return w
}

func TestDevelopmentIdentityCannotVerifyTailscaleTest(t *testing.T) {
	f, d := identityFixture(t)
	cookie := &http.Cookie{Name: d.cookie, Value: d.signed(f.user.ID)}
	w := identityRequest(d, "GET", "/api/tailscale-test", nil, cookie)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"simulated"`) || strings.Contains(w.Body.String(), `"sections"`) {
		t.Fatalf("simulation produced a real sign-in result: %d %s", w.Code, w.Body.String())
	}
}

func TestDevelopmentIdentitiesUseRealAuthenticationAndStayPerBrowser(t *testing.T) {
	f, d := identityFixture(t)
	before, _ := f.state.users()
	selectUser := func(id string) *http.Cookie {
		t.Helper()
		w := identityRequest(d, "POST", "/__dev/identity", map[string]string{"ID": id, "CSRF": d.secret})
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		cookie := w.Result().Cookies()[0]
		if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
			t.Fatal("identity cookie is not protected")
		}
		return cookie
	}
	admin, member := selectUser(f.user.ID), selectUser("u_member")
	for _, tc := range []struct {
		id     string
		cookie *http.Cookie
		admin  bool
	}{{f.user.ID, admin, true}, {"u_member", member, false}, {f.user.ID, admin, true}} {
		w := identityRequest(d, "GET", "/api/auth/status", nil, tc.cookie)
		var status struct {
			User   localUser
			Method string
			Admin  bool
		}
		json.Unmarshal(w.Body.Bytes(), &status)
		if w.Code != 200 || status.User.ID != tc.id || status.Method != "tailscale" || status.Admin != tc.admin || w.Header().Get("X-Anchor-Dev-Identities") != "1" {
			t.Fatalf("wrong simulated account: %s", w.Body.String())
		}
		w = identityRequest(d, "GET", "/api/admin/state", nil, tc.cookie)
		if tc.admin && w.Code != 200 || !tc.admin && w.Code != 403 {
			t.Fatalf("administrator boundary: %d", w.Code)
		}
	}
	// A valid old local login cannot disguise a disabled/unlinked test identity.
	w := httptest.NewRecorder()
	if err := f.auth.issue(w, f.user); err != nil {
		t.Fatal(err)
	}
	local := w.Result().Cookies()[0]
	for _, id := range []string{"u_disabled", "u_unlinked", "unlinked", "signed-out"} {
		w := identityRequest(d, "GET", "/api/auth/status", nil, selectUser(id), local)
		var status struct{ Authenticated bool }
		json.Unmarshal(w.Body.Bytes(), &status)
		if status.Authenticated {
			t.Fatalf("%s inherited local authentication", id)
		}
	}
	for _, cookie := range []*http.Cookie{nil, {Name: d.cookie, Value: f.user.ID + ".forged"}} {
		var cookies []*http.Cookie
		if cookie != nil {
			cookies = append(cookies, cookie)
		}
		w := identityRequest(d, "GET", "/api/admin/state", nil, cookies...)
		if w.Code != 403 {
			t.Fatal("unsigned identity obtained administrator access")
		}
	}
	// Switching users invalidates the previous identity's CSRF token.
	r := httptest.NewRequest("POST", "http://127.0.0.1:18080/api/auth/password", strings.NewReader(`{}`))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Origin", "http://127.0.0.1:18080")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-CSRF-Token", f.auth.csrfFor(f.user.ID))
	r.AddCookie(member)
	w = httptest.NewRecorder()
	d.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("previous identity's CSRF token remained usable")
	}
	// A restart creates a new simulator secret, invalidating old choices.
	restarted, err := newDevelopmentIdentities(f.handler, f.auth, d.authority)
	if err != nil {
		t.Fatal(err)
	}
	if w := identityRequest(restarted.(*developmentIdentity), "GET", "/api/admin/state", nil, admin); w.Code != 403 {
		t.Fatal("identity choice survived simulator restart")
	}
	after, _ := f.state.users()
	if !reflect.DeepEqual(before, after) {
		t.Fatal("switching identities changed account records")
	}
}

func TestDevelopmentIdentityPickerGuardsAndCredentials(t *testing.T) {
	f, d := identityFixture(t)
	r := httptest.NewRequest("GET", "http://127.0.0.1:18080/api/admin/state", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Tailscale-User-Login", f.user.Tailscale)
	w := httptest.NewRecorder()
	d.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("visitor-supplied identity header bypassed the picker")
	}
	for _, tc := range []struct{ remote, host string }{
		{"192.0.2.10:1234", d.authority}, {"127.0.0.1:1234", "attacker.test:18080"}, {"127.0.0.1:1234", "anchor.example.test"},
	} {
		r := httptest.NewRequest("GET", "http://127.0.0.1:18080/__dev/identity-state", nil)
		r.RemoteAddr, r.Host = tc.remote, tc.host
		w := httptest.NewRecorder()
		d.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal("picker accepted nonlocal authority or remote")
		}
	}
	for _, tc := range []struct{ origin, csrf string }{
		{"http://attacker.test", d.secret}, {"", d.secret}, {"http://127.0.0.1:18080", "wrong-token"},
	} {
		data, _ := json.Marshal(map[string]string{"ID": "u_member", "CSRF": tc.csrf})
		r := httptest.NewRequest("POST", "http://127.0.0.1:18080/__dev/identity", bytes.NewReader(data))
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		d.ServeHTTP(w, r)
		if w.Code != 403 || len(w.Result().Cookies()) != 0 {
			t.Fatal("picker accepted an invalid selection request")
		}
	}
	w = identityRequest(d, "POST", "/__dev/identity", map[string]string{"ID": "nonexistent", "CSRF": d.secret})
	if w.Code != 400 {
		t.Fatal("invented Anchor account accepted")
	}
	w = identityRequest(d, "GET", "/__dev/identity-state", nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "password") || strings.Contains(w.Body.String(), "has_password") || strings.Contains(w.Body.String(), "auth_version") {
		t.Fatal("picker metadata exposed account credentials")
	}
	for _, path := range []string{"/__dev/identities", "/__dev/identities.js", "/__dev/identities.css"} {
		if w := identityRequest(d, "GET", path, nil); w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("missing protected dev asset: %s", path)
		}
	}
	if w := identityRequest(d, "POST", "/api/auth/login", nil); w.Code != 409 {
		t.Fatal("simulator permitted misleading local sign-in")
	}
}

func TestDevelopmentIdentityRefusesDeploymentAndFreshSetup(t *testing.T) {
	f, _ := identityFixture(t)
	for _, listen := range []string{"0.0.0.0:18080", "192.0.2.10:18080", "localhost:18080", "127.0.0.1:18080,[::1]:18080", "127.0.0.1:0"} {
		if _, err := newDevelopmentIdentities(f.handler, f.auth, listen); err == nil {
			t.Fatalf("accepted unsafe listener %s", listen)
		}
	}
	f.auth.publicURL = "https://anchor.example.test"
	if _, err := newDevelopmentIdentities(f.handler, f.auth, "127.0.0.1:18080"); err == nil {
		t.Fatal("accepted a public URL")
	}
	f.auth.publicURL, f.auth.mode = "", "lan"
	if _, err := newDevelopmentIdentities(f.handler, f.auth, "127.0.0.1:18080"); err == nil {
		t.Fatal("accepted LAN authentication mode")
	}
	fresh := newAccountFixture(t)
	fresh.auth.mode, fresh.auth.publicURL = "tailscale", ""
	if _, err := newDevelopmentIdentities(fresh.handler, fresh.auth, "127.0.0.1:18080"); err == nil {
		t.Fatal("simulator accepted an installation without people")
	}
}
