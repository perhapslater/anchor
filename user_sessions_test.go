package main

import (
	"encoding/json"
	"testing"
)

func TestSavingOwnProfileKeepsLocalSessionsAndWizardProgress(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	secondLogin := f.request("POST", "/api/auth/login", map[string]string{"Username": f.user.Username, "Password": "correct-password-123"}, false)
	if secondLogin.Code != 200 {
		t.Fatal(secondLogin.Body.String())
	}
	secondCookie := secondLogin.Result().Cookies()[0]
	before, _ := f.state.user(f.user.ID)
	progress, _ := f.state.onboarding()
	originalCookie, originalCSRF := f.cookie, f.csrf
	for _, name := range []string{before.Name, "Updated administrator"} {
		u, _ := f.state.user(f.user.ID)
		u.Name = name
		revision, _ := f.state.revision()
		w := f.request("POST", "/api/admin/users", map[string]any{"revision": revision, "user": u, "password": ""}, true)
		if w.Code != 200 || len(w.Result().Cookies()) != 0 {
			t.Fatalf("profile save changed session: %d %s", w.Code, w.Body.String())
		}
		var status struct {
			Authenticated bool      `json:"authenticated"`
			CSRF          string    `json:"csrf"`
			User          localUser `json:"user"`
		}
		w = f.request("GET", "/api/auth/status", nil, false)
		if json.Unmarshal(w.Body.Bytes(), &status) != nil || !status.Authenticated || status.CSRF != originalCSRF || status.User.Name != name {
			t.Fatalf("current session lost: %s", w.Body.String())
		}
		after, _ := f.state.user(f.user.ID)
		if after.authVersion != before.authVersion || after.password != before.password {
			t.Fatal("profile edit changed authentication credentials/version")
		}
		f.cookie = secondCookie
		if w := f.request("GET", "/api/admin/state", nil, false); w.Code != 200 {
			t.Fatal("profile save revoked another local session")
		}
		f.cookie = originalCookie
		currentProgress, _ := f.state.onboarding()
		if currentProgress.Step != progress.Step || currentProgress.Completed != progress.Completed || currentProgress.Revision != progress.Revision {
			t.Fatal("profile save changed wizard progress")
		}
	}
}

func TestUserSecurityChangesStillRevokeSessions(t *testing.T) {
	for _, field := range []string{"password", "username", "tailscale", "groups", "enabled", "disable_local"} {
		t.Run(field, func(t *testing.T) {
			f := newAccountFixture(t)
			f.setup(t)
			// Keep a second administrator for the disable/pause cases.
			if err := f.state.saveUser(localUser{ID: "u_backup", Username: "backup", Name: "Backup", Groups: []string{"admin"}, Enabled: true}, "backup-password", f.user.ID); err != nil {
				t.Fatal(err)
			}
			u, _ := f.state.user(f.user.ID)
			before := u.authVersion
			password, disableLocal := "", false
			switch field {
			case "password":
				password = "changed-password"
			case "username":
				u.Username = "renamed-login"
			case "tailscale":
				u.Tailscale = "admin@example.test"
			case "groups":
				u.Groups = append(u.Groups, "family")
			case "enabled":
				u.Enabled = false
			case "disable_local":
				// Establish the identity before testing the password removal.
				if err := f.state.saveTailscaleIdentity(u.ID, "admin@example.test", u.ID, tokenHash(f.cookie.Value), mustUserRevision(t, f)); err != nil {
					t.Fatal(err)
				}
				u, _ = f.state.user(u.ID)
				before = u.authVersion
				disableLocal = true
			}
			w := f.request("POST", "/api/admin/users", map[string]any{"revision": mustUserRevision(t, f), "user": u, "password": password, "disable_local": disableLocal}, true)
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			if w := f.request("GET", "/api/admin/state", nil, false); w.Code != 403 {
				t.Fatalf("%s change retained a local session", field)
			}
			after, _ := f.state.user(u.ID)
			if after.authVersion != before+1 {
				t.Fatalf("%s change did not invalidate in-flight authentication", field)
			}
		})
	}
}

func mustUserRevision(t *testing.T, f *accountFixture) int64 {
	t.Helper()
	revision, err := f.state.revision()
	if err != nil {
		t.Fatal(err)
	}
	return revision
}

func TestEquivalentGroupsKeepLocalSession(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	u, _ := f.state.user(f.user.ID)
	u.Groups = []string{" ADMIN ", "admin"}
	u.Username = " ADMIN "
	if w := f.request("POST", "/api/admin/users", map[string]any{"revision": mustUserRevision(t, f), "user": u, "password": ""}, true); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := f.request("GET", "/api/admin/state", nil, false); w.Code != 200 {
		t.Fatal("equivalent normalized groups/login revoked session")
	}
}
