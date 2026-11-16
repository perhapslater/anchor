package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func tailscaleAccountUpdate(f *accountFixture, login, body, peer string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "http://anchor.test/api/auth/account", strings.NewReader(body))
	r.RemoteAddr = peer
	r.Header.Set("Origin", "http://anchor.test")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Tailscale-User-Login", login)
	users, _ := f.state.users()
	for _, user := range users {
		if user.Tailscale == login {
			r.Header.Set("X-CSRF-Token", f.auth.csrfFor(user.ID))
		}
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func TestTailscaleSelfServiceKeepsIdentityAndRevokesPasswordSessions(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	f.auth.mode, f.auth.publicURL = "tailscale", ""
	member := localUser{ID: "u_credentials", Username: "member", Name: "Member", Tailscale: "member@example.test", Groups: []string{"family"}, Enabled: true}
	if err := f.state.saveUser(member, "old-password", f.user.ID); err != nil {
		t.Fatal(err)
	}
	member, _ = f.state.user(member.ID)
	local := httptest.NewRecorder()
	if err := f.auth.issue(local, member); err != nil {
		t.Fatal(err)
	}
	before, _ := f.state.revision()
	for _, body := range []string{`{"Username":"admin"}`, `{"Username":""}`, `{"Username":"changed","Groups":["admin"]}`, `{"Username":"changed","ID":"` + f.user.ID + `"}`} {
		w := tailscaleAccountUpdate(f, member.Tailscale, body, loopback)
		if w.Code != 400 {
			t.Fatalf("invalid self-service update accepted: %d %s", w.Code, w.Body.String())
		}
	}
	if current, _ := f.state.revision(); current != before {
		t.Fatal("rejected update changed saved state")
	}
	if w := tailscaleAccountUpdate(f, member.Tailscale, `{"Username":"changed"}`, "192.0.2.10:1234"); w.Code != 403 {
		t.Fatal("untrusted identity changed credentials")
	}
	w := tailscaleAccountUpdate(f, member.Tailscale, `{"Username":"member-new","Password":"new-password"}`, loopback)
	var result struct{ Reauthenticate bool }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Reauthenticate {
		t.Fatalf("Tailscale update: %d %s", w.Code, w.Body.String())
	}
	after, _ := f.state.user(member.ID)
	if after.Username != "member-new" || !checkPassword(after.password, "new-password") || after.Name != member.Name || after.Tailscale != member.Tailscale || !after.Enabled || !equal(after.Groups, member.Groups) {
		t.Fatalf("wrong account changes: %+v", after)
	}
	if a := testTailscaleRequest(f, "/api/auth/status", "anchor.test", loopback, member.Tailscale, false); !strings.Contains(a.Body.String(), `"authenticated":true`) || !strings.Contains(a.Body.String(), `"method":"tailscale"`) {
		t.Fatalf("Tailscale sign-in ended: %s", a.Body.String())
	}
	f.cookie = local.Result().Cookies()[0]
	if a := f.request("GET", "/api/auth/status", nil, false); strings.Contains(a.Body.String(), `"authenticated":true`) {
		t.Fatal("old password session survived a credential change")
	}
	// This fresh trusted request needs no current local password, including for username-only edits.
	if w := tailscaleAccountUpdate(f, member.Tailscale, `{"Username":"member-renamed","Password":""}`, loopback); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	updated, _ := f.state.user(member.ID)
	if !checkPassword(updated.password, "new-password") {
		t.Fatal("blank password cleared the saved password")
	}
	if err := f.state.saveUserSignIn(updated, "", true, f.user.ID); err != nil {
		t.Fatal(err)
	}
	if w := tailscaleAccountUpdate(f, member.Tailscale, `{"Username":"bypass","Password":"bypass"}`, loopback); w.Code != 403 {
		t.Fatal("member re-enabled administrator-disabled local sign-in")
	}
	if a := testTailscaleRequest(f, "/api/auth/status", "anchor.test", loopback, member.Tailscale, false); !strings.Contains(a.Body.String(), `"authenticated":true`) {
		t.Fatal("disabling local sign-in ended Tailscale access")
	}
}

func TestPasswordSelfServiceRequiresCurrentPasswordAndOnlyRevokesForChanges(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	body := map[string]string{"Username": f.user.Username, "Current": "wrong"}
	if w := f.request("POST", "/api/auth/account", body, true); w.Code != 403 {
		t.Fatal("wrong password authorized account changes")
	}
	body["Current"] = "correct-password-123"
	if w := f.request("POST", "/api/auth/account", body, false); w.Code != 403 {
		t.Fatal("self-service bypassed CSRF")
	}
	before, _ := f.state.user(f.user.ID)
	if w := f.request("POST", "/api/auth/account", body, true); w.Code != 200 || strings.Contains(w.Body.String(), `"reauthenticate":true`) {
		t.Fatalf("unchanged save: %s", w.Body.String())
	}
	if w := f.request("GET", "/api/admin/state", nil, false); w.Code != 200 {
		t.Fatal("unchanged self-service save ended the session")
	}
	body["Username"] = "renamed-admin"
	w := f.request("POST", "/api/auth/account", body, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"reauthenticate":true`) {
		t.Fatalf("username change: %s", w.Body.String())
	}
	after, _ := f.state.user(f.user.ID)
	if after.ID != before.ID || after.Name != before.Name || !equal(after.Groups, before.Groups) || after.password != before.password {
		t.Fatal("username edit changed unrelated account fields")
	}
	if w := f.request("GET", "/api/admin/state", nil, false); w.Code != 403 {
		t.Fatal("changed local credentials retained the old password session")
	}
}
