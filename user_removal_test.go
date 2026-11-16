package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestRemoveUserPreservesRecoveryAdministrator(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	for _, backup := range []localUser{
		{ID: "u_paused_admin", Username: "paused", Name: "Paused admin", Groups: []string{"admin"}, Enabled: false},
		{ID: "u_tailscale_admin", Username: "tailscale", Name: "Tailscale admin", Tailscale: "backup@example.test", Groups: []string{"admin"}, Enabled: true},
	} {
		password := ""
		if !backup.Enabled {
			password = "backup-password"
		}
		if err := f.state.saveUser(backup, password, f.user.ID); err != nil {
			t.Fatal(err)
		}
	}
	revision, _ := f.state.revision()
	w := f.request("DELETE", "/api/admin/users/"+f.user.ID, map[string]any{"revision": revision}, true)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "keep at least one enabled administrator with a local password") {
		t.Fatalf("last recovery administrator removal: %d %s", w.Code, w.Body.String())
	}
	if current, _ := f.state.revision(); current != revision {
		t.Fatal("rejected removal changed settings")
	}
	if w := f.request("GET", "/api/admin/state", nil, false); w.Code != 200 {
		t.Fatal("rejected removal ended the administrator's session")
	}
	if err := f.state.saveUser(localUser{ID: "u_recovery_admin", Username: "recovery", Name: "Recovery", Groups: []string{"admin"}, Enabled: true}, "recovery-password", f.user.ID); err != nil {
		t.Fatal(err)
	}
	revision, _ = f.state.revision()
	w = f.request("DELETE", "/api/admin/users/"+f.user.ID, map[string]any{"revision": revision}, true)
	if w.Code != 200 || len(w.Result().Cookies()) != 1 || w.Result().Cookies()[0].MaxAge != -1 {
		t.Fatalf("removing own account with a recovery administrator failed: %d %s", w.Code, w.Body.String())
	}
	if _, exists := f.state.user(f.user.ID); exists {
		t.Fatal("removed administrator remains in database")
	}
	if w := f.request("GET", "/api/admin/state", nil, false); w.Code != 403 {
		t.Fatal("removed administrator retained a session")
	}
}

func TestRemoveUserRevokesSignInAndCleansDirectGrantsAtomically(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	member := localUser{ID: "u_removed", Username: "removed", Name: "Removed member", Tailscale: "removed@example.test", Groups: []string{"family"}, Enabled: true}
	if err := f.state.saveUser(member, "member-password", f.user.ID); err != nil {
		t.Fatal(err)
	}
	member, _ = f.state.user(member.ID)
	var memberCookies []*http.Cookie
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		if err := f.auth.issue(w, member); err != nil {
			t.Fatal(err)
		}
		memberCookies = append(memberCookies, w.Result().Cookies()[0])
	}
	cfg, _ := f.state.store.Get()
	copyCfg := *cfg
	copyCfg.Apps = []App{
		{ID: "personal", Name: "Personal app", URL: "https://personal.test", Access: []string{member.ID}, Check: "off"},
		{ID: "shared", Name: "Shared app", URL: "https://shared.test", Access: []string{"family", "  U_REMOVED  ", member.ID}, Check: "off"},
		{ID: "public", Name: "Everyone app", URL: "https://public.test", Access: []string{"everyone"}, Check: "off"},
	}
	copyCfg.Notice = &Notice{Message: "Personal announcement", Access: []string{member.ID}}
	revision, _ := f.state.revision()
	if err := f.state.saveConfigAndSecrets(&copyCfg, revision, f.user.ID, map[string]string{"plex": "keep-this-secret"}); err != nil {
		t.Fatal(err)
	}
	revision, _ = f.state.revision()
	progress, _ := f.state.onboarding()
	for name, request := range map[string]struct {
		id       string
		revision int64
		csrf     bool
		status   int
	}{
		"missing CSRF": {member.ID, revision, false, 403},
		"stale state":  {member.ID, revision - 1, true, 409},
		"unknown user": {"u_unknown", revision, true, 400},
	} {
		w := f.request("DELETE", "/api/admin/users/"+request.id, map[string]any{"revision": request.revision}, request.csrf)
		if w.Code != request.status {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
		if _, exists := f.state.user(member.ID); !exists {
			t.Fatalf("%s changed the account", name)
		}
		current, _ := f.state.revision()
		var sessions int
		if err := f.state.db.QueryRow(`SELECT count(*) FROM sessions WHERE user_id=?`, member.ID).Scan(&sessions); err != nil || sessions != 2 || current != revision {
			t.Fatalf("%s partially changed saved state: revision=%d sessions=%d err=%v", name, current, sessions, err)
		}
	}
	w := f.request("DELETE", "/api/admin/users/"+member.ID, map[string]any{"revision": revision}, true)
	if w.Code != 200 {
		t.Fatalf("removal: %d %s", w.Code, w.Body.String())
	}
	if current, _ := f.state.revision(); current != revision+1 {
		t.Fatal("removal did not use exactly one settings revision")
	}
	if _, exists := f.state.user(member.ID); exists {
		t.Fatal("removed user still exists")
	}
	updated, _ := f.state.store.Get()
	if !equal(updated.Apps[0].Access, []string{"admin"}) || !equal(updated.Apps[1].Access, []string{"family"}) || !equal(updated.Apps[2].Access, []string{"everyone"}) || !equal(updated.Notice.Access, []string{"admin"}) {
		t.Fatalf("incorrect audience cleanup: %+v %+v", updated.Apps, updated.Notice)
	}
	if updated.credentials["plex"] != "keep-this-secret" {
		t.Fatal("user removal changed integration credentials")
	}
	if err := f.state.validateUserGrants(updated); err != nil {
		t.Fatalf("removed user left invalid access rules: %v", err)
	}
	currentProgress, _ := f.state.onboarding()
	if currentProgress.Step != progress.Step || currentProgress.Completed != progress.Completed || currentProgress.Revision != progress.Revision {
		t.Fatal("removal changed wizard progress")
	}
	var sessions, audits int
	if err := f.state.db.QueryRow(`SELECT count(*) FROM sessions WHERE user_id=?`, member.ID).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatalf("removed user's sessions remain: %d %v", sessions, err)
	}
	if err := f.state.db.QueryRow(`SELECT count(*) FROM audit WHERE actor=? AND action=?`, f.user.ID, "user removed: "+member.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatal("removal was not audited once")
	}
	if w := f.request("GET", "/api/admin/state", nil, false); w.Code != 200 {
		t.Fatal("removing another user ended the administrator's session")
	}
	for _, cookie := range memberCookies {
		f.cookie = cookie
		w := f.request("GET", "/api/auth/status", nil, false)
		var status struct{ Authenticated bool }
		if json.Unmarshal(w.Body.Bytes(), &status) != nil || status.Authenticated {
			t.Fatal("removed user retained a local session")
		}
	}
	f.cookie = nil
	if w := f.request("POST", "/api/auth/login", map[string]string{"Username": member.Username, "Password": "member-password"}, false); w.Code != 401 {
		t.Fatal("removed user could still sign in locally")
	}
	if err := f.auth.issue(httptest.NewRecorder(), member); err == nil {
		t.Fatal("in-flight sign-in recreated a removed user's session")
	}
	f.auth.mode, f.auth.publicURL = "tailscale", ""
	w = testTailscaleRequest(f, "/api/auth/status", "anchor.example.ts.net", "127.0.0.1:1234", member.Tailscale, false)
	if strings.Contains(w.Body.String(), `"authenticated":true`) {
		t.Fatal("removed user's Tailscale identity still authenticates")
	}
}

func TestRemoveUserRequiresAdministratorAndValidJSON(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	member := localUser{ID: "u_member", Username: "member", Name: "Member", Groups: []string{"family"}, Enabled: true}
	if err := f.state.saveUser(member, "member-password", f.user.ID); err != nil {
		t.Fatal(err)
	}
	path := "/api/admin/users/" + f.user.ID
	for _, body := range []any{nil, map[string]any{"revision": 1, "unexpected": true}} {
		if w := f.request("DELETE", path, body, true); w.Code != 400 {
			t.Fatalf("invalid removal JSON accepted: %d", w.Code)
		}
	}
	w := f.request("POST", "/api/auth/login", map[string]string{"Username": member.Username, "Password": "member-password"}, false)
	f.cookie = w.Result().Cookies()[0]
	var status struct{ CSRF string }
	json.Unmarshal(f.request("GET", "/api/auth/status", nil, false).Body.Bytes(), &status)
	f.csrf = status.CSRF
	if w := f.request("DELETE", path, map[string]any{"revision": 1}, true); w.Code != 403 {
		t.Fatal("member could remove a user")
	}
	f.cookie = nil
	if w := f.request("DELETE", path, map[string]any{"revision": 1}, false); w.Code != 403 {
		t.Fatal("anonymous visitor could remove a user")
	}
}

func TestConcurrentAdministratorRemovalKeepsOneAdministrator(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	backup := localUser{ID: "u_backup", Username: "backup", Name: "Backup", Groups: []string{"admin"}, Enabled: true}
	if err := f.state.saveUser(backup, "backup-password", f.user.ID); err != nil {
		t.Fatal(err)
	}
	revision, _ := f.state.revision()
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []string{f.user.ID, backup.ID} {
		wg.Go(func() { <-start; results <- f.state.removeUser(id, revision, f.user.ID) })
	}
	close(start)
	wg.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		}
	}
	users, err := f.state.users()
	if err != nil || succeeded != 1 || len(users) != 1 || !users[0].Enabled || !groupAdmin(users[0].Groups) || !users[0].HasPassword {
		t.Fatalf("concurrent removal lost the recovery administrator: users=%+v succeeded=%d err=%v", users, succeeded, err)
	}
}
