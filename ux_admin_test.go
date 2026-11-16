package main

import (
	"strings"
	"testing"
)

func TestConsoleTailscaleLinkPreservesAccountAndRejectsDuplicates(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	if err := linkTailscaleUser(f.state.dir, "admin", "admin@example.test"); err != nil {
		t.Fatal(err)
	}
	u, _ := f.state.user(f.user.ID)
	if u.Tailscale != "admin@example.test" || !groupAdmin(u.Groups) || !u.HasPassword || !u.Enabled {
		t.Fatal("linking changed account privileges or password availability")
	}
	if err := linkTailscaleUser(f.state.dir, "admin", "admin@example.test"); err != nil {
		t.Fatal(err)
	}
	member := localUser{ID: "u_console_member", Username: "tester", Name: "Tester", Enabled: true}
	if err := f.state.saveUser(member, "test-password-123", f.user.ID); err != nil {
		t.Fatal(err)
	}
	if err := linkTailscaleUser(f.state.dir, "tester", "admin@example.test"); err == nil {
		t.Fatal("the same Tailscale identity was assigned to two accounts")
	}
	if err := linkTailscaleUser(f.state.dir, "tester", "tester@example.test"); err != nil {
		t.Fatal(err)
	}
	u, _ = f.state.user(member.ID)
	if u.Tailscale != "tester@example.test" || groupAdmin(u.Groups) {
		t.Fatal("member identity link changed their permissions")
	}
}

func TestSignInControlsPreserveRecoveryAndRevokeSessions(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	revision, _ := f.state.revision()
	admin := f.user
	admin.Tailscale = "admin@example.test"
	response := f.request("POST", "/api/admin/users", map[string]any{
		"revision": revision, "user": admin, "password": "", "disable_local": true,
	}, true)
	if response.Code != 400 || !strings.Contains(response.Body.String(), "keep at least one") {
		t.Fatalf("last admin protection: %d %s", response.Code, response.Body.String())
	}
	member := localUser{ID: "u_signin_member", Name: "Member", Username: "member", Groups: []string{"family"}, Enabled: true}
	if err := f.state.saveUser(member, "member-password-123", f.user.ID); err != nil {
		t.Fatal(err)
	}
	member, _ = f.state.user(member.ID)
	if err := f.state.saveUserSignIn(member, "", true, f.user.ID); err == nil {
		t.Fatal("must retain a Tailscale identity when removing a password")
	}
	member.Tailscale = "member@example.test"
	if err := f.state.saveUser(member, "", f.user.ID); err != nil {
		t.Fatal(err)
	}
	response = f.request("POST", "/api/auth/login", map[string]string{"Username": "member", "Password": "member-password-123"}, false)
	if response.Code != 200 {
		t.Fatalf("member login: %s", response.Body.String())
	}
	memberCookie := response.Result().Cookies()[0]
	revision, _ = f.state.revision()
	response = f.request("POST", "/api/admin/users", map[string]any{
		"revision": revision, "user": member, "password": "", "disable_local": true,
	}, true)
	if response.Code != 200 {
		t.Fatalf("disable local: %d %s", response.Code, response.Body.String())
	}
	updated, _ := f.state.user(member.ID)
	if updated.HasPassword || updated.Tailscale != member.Tailscale || !updated.Enabled {
		t.Fatalf("only password should be removed: %+v", updated)
	}
	f.cookie = memberCookie
	if response = f.request("GET", "/api/auth/status", nil, false); strings.Contains(response.Body.String(), `"authenticated":true`) {
		t.Fatal("old local session survived disabling sign-in")
	}
	if err := f.state.saveUserSignIn(updated, "replacement-password-123", false, f.user.ID); err != nil {
		t.Fatal(err)
	}
	updated, _ = f.state.user(member.ID)
	if !updated.HasPassword || !checkPassword(updated.password, "replacement-password-123") {
		t.Fatal("local sign-in was not re-enabled")
	}
}

func TestIntegrationSettingsAndKeysSaveTogether(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	cfg, _ := f.state.store.Get()
	copyCfg := *cfg
	copyCfg.Title = "Updated integration settings"
	revision, _ := f.state.revision()
	response := f.request("POST", "/api/admin/config", map[string]any{
		"revision": revision, "config": copyCfg, "keys": map[string]string{"abs": "synthetic-key"},
	}, true)
	if response.Code != 200 {
		t.Fatalf("combined save: %d %s", response.Code, response.Body.String())
	}
	cfg, _ = f.state.store.Get()
	if cfg.Title != copyCfg.Title || cfg.credentials["abs"] != "synthetic-key" {
		t.Fatal("settings and keys did not both change")
	}
	// Stale saves and invalid keys must roll back the complete operation.
	for _, invalid := range []struct {
		revision int64
		keys     map[string]string
	}{
		{revision, map[string]string{"abs": "stale"}},
		{revision + 1, map[string]string{"abs": "must-roll-back", "unsupported": "invalid"}},
	} {
		copyCfg.Title = "Must not apply"
		response = f.request("POST", "/api/admin/config", map[string]any{
			"revision": invalid.revision, "config": copyCfg, "keys": invalid.keys,
		}, true)
		if response.Code == 200 {
			t.Fatal("invalid combined save succeeded")
		}
		cfg, _ = f.state.store.Get()
		currentRevision, _ := f.state.revision()
		if cfg.Title == "Must not apply" || cfg.credentials["abs"] != "synthetic-key" || currentRevision != revision+1 {
			t.Fatal("failed save changed settings, keys or revision")
		}
	}
}
