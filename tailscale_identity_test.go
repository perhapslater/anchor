package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestIdentitySelfLinkPreservesOnlyCurrentLocalSession(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	originalCookie := f.cookie
	other := f.request("POST", "/api/auth/login", map[string]string{"Username": "admin", "Password": "correct-password-123"}, false)
	if other.Code != 200 {
		t.Fatal(other.Body.String())
	}
	otherCookie := other.Result().Cookies()[0]
	revision, _ := f.state.revision()
	body := map[string]any{"id": f.user.ID, "email": "admin@example.test", "revision": revision}
	if w := f.request("POST", "/api/admin/tailscale", body, false); w.Code != 403 {
		t.Fatal("identity linking accepted missing CSRF")
	}
	w := f.request("POST", "/api/admin/tailscale", body, true)
	if w.Code != 200 {
		t.Fatalf("self-link failed: %d %s", w.Code, w.Body.String())
	}
	if len(w.Result().Cookies()) != 0 {
		t.Fatal("identity linking replaced the active session cookie")
	}
	var status struct {
		Authenticated bool   `json:"authenticated"`
		Method        string `json:"method"`
		CSRF          string `json:"csrf"`
	}
	w = f.request("GET", "/api/auth/status", nil, false)
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Authenticated || status.Method != "local" || status.CSRF != f.csrf {
		t.Fatalf("self-link changed current authentication: %+v", status)
	}
	f.cookie = otherCookie
	if w := f.request("GET", "/api/admin/state", nil, false); w.Code != 403 {
		t.Fatal("other local session survived identity change")
	}
	f.cookie = originalCookie
	u, _ := f.state.user(f.user.ID)
	if u.Tailscale != "admin@example.test" || !u.Enabled || !groupAdmin(u.Groups) || !checkPassword(u.password, "correct-password-123") {
		t.Fatal("identity link changed unrelated account settings")
	}
	revision, _ = f.state.revision()
	if w := f.request("POST", "/api/admin/tailscale", map[string]any{"id": u.ID, "email": "other@example.test", "revision": revision, "groups": []string{"admin"}}, true); w.Code != 400 {
		t.Fatal("identity route accepted permission fields")
	}
	// Ordinary password edits retain their original revocation behavior.
	w = f.request("POST", "/api/admin/users", map[string]any{"user": u, "password": "new-pass", "revision": revision}, true)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := f.request("GET", "/api/admin/state", nil, false); w.Code != 403 {
		t.Fatal("password edit preserved current session")
	}
}

func TestIdentityLinkRejectsDuplicateStaleAndUnauthorizedChanges(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	member := localUser{ID: "u_identity_member", Username: "member", Name: "Member", Groups: []string{"family"}, Enabled: true}
	if err := f.state.saveUser(member, "pass", f.user.ID); err != nil {
		t.Fatal(err)
	}
	login := f.request("POST", "/api/auth/login", map[string]string{"Username": "member", "Password": "pass"}, false)
	if login.Code != 200 {
		t.Fatal(login.Body.String())
	}
	memberCookie := login.Result().Cookies()[0]
	revision, _ := f.state.revision()
	body := map[string]any{"id": member.ID, "email": "member@example.test", "revision": revision}
	if w := f.request("POST", "/api/admin/tailscale", body, true); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := f.request("POST", "/api/admin/tailscale", body, true); w.Code != 409 {
		t.Fatal("stale identity update accepted")
	}
	revision, _ = f.state.revision()
	w := f.request("POST", "/api/admin/tailscale", map[string]any{"id": f.user.ID, "email": "member@example.test", "revision": revision}, true)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "already linked") {
		t.Fatalf("duplicate email accepted: %d %s", w.Code, w.Body.String())
	}
	if current, _ := f.state.revision(); current != revision {
		t.Fatal("failed identity link changed settings")
	}
	adminCookie := f.cookie
	f.cookie = memberCookie
	if w := f.request("GET", "/api/admin/state", nil, false); w.Code != 403 {
		t.Fatal("changed member session not revoked")
	}
	f.cookie = adminCookie
	if w := f.request("GET", "/api/admin/state", nil, false); w.Code != 200 {
		t.Fatal("editing another identity ended administrator session")
	}
	f.cookie = nil
	if w := f.request(http.MethodPost, "/api/admin/tailscale", body, true); w.Code != 403 {
		t.Fatal("anonymous identity edit accepted")
	}
}
