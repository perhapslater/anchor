package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRevokedSessionCannotFinishPausedCredentialChange(t *testing.T) {
	for _, revoke := range []string{"logout", "expire"} {
		for _, endpoint := range []string{"account", "password"} {
			t.Run(revoke+"/"+endpoint, func(t *testing.T) {
				f := newAccountFixture(t)
				f.setup(t)
				before, _ := f.state.user(f.user.ID)
				revision := mustUserRevision(t, f)
				payload := map[string]string{"Current": "correct-password-123"}
				if endpoint == "account" {
					payload["Username"] = "changed-after-revocation"
				} else {
					payload["Password"] = "changed-after-revocation"
				}
				raw, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				body := &auditPausedBody{Reader: bytes.NewReader(raw), entered: make(chan struct{}), release: make(chan struct{})}
				release := sync.OnceFunc(func() { close(body.release) })
				t.Cleanup(release)
				r := httptest.NewRequest("POST", "http://anchor.test/api/auth/"+endpoint, nil)
				r.Body = body
				r.RemoteAddr = "192.0.2.10:1234"
				r.Header.Set("Origin", "http://anchor.test")
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("X-CSRF-Token", f.csrf)
				r.AddCookie(f.cookie)
				w := httptest.NewRecorder()
				done := make(chan struct{})
				go func() { defer close(done); f.handler.ServeHTTP(w, r) }()
				select {
				case <-body.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("credential request never began decoding")
				}
				if revoke == "logout" {
					if response := f.request("POST", "/api/auth/logout", map[string]any{}, true); response.Code != 200 {
						t.Fatal("logout failed")
					}
				} else if _, err := f.state.db.Exec(`UPDATE sessions SET expires=? WHERE hash=?`, time.Now().Add(-time.Second).Unix(), tokenHash(f.cookie.Value)); err != nil {
					t.Fatal(err)
				}
				release()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("credential request did not finish")
				}
				after, _ := f.state.user(f.user.ID)
				if w.Code != 403 || after.Username != before.Username || after.password != before.password || after.authVersion != before.authVersion || mustUserRevision(t, f) != revision {
					t.Fatalf("revoked session changed credentials: HTTP %d", w.Code)
				}
			})
		}
	}
}

func TestLoggedOutSessionCannotFinishPausedPhoto(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	raw, _ := json.Marshal(map[string]any{"image": photoFixture("png"), "revision": 0})
	body := &auditPausedBody{Reader: bytes.NewReader(raw), entered: make(chan struct{}), release: make(chan struct{})}
	r := httptest.NewRequest("POST", "http://anchor.test/api/auth/photo", nil)
	r.Body = body
	r.RemoteAddr = "192.0.2.10:1234"
	r.Header.Set("Origin", "http://anchor.test")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-CSRF-Token", f.csrf)
	r.AddCookie(f.cookie)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); f.handler.ServeHTTP(w, r) }()
	select {
	case <-body.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("photo request never decoded")
	}
	logout := f.request("POST", "/api/auth/logout", map[string]any{}, true)
	close(body.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("photo request did not finish")
	}
	if logout.Code != 200 {
		t.Fatal(logout.Body.String())
	}
	user, _ := f.state.user(f.user.ID)
	if w.Code != 403 || user.CustomPicture {
		t.Fatalf("revoked local session changed photo: HTTP %d", w.Code)
	}
}

func TestIdentityEditorClearsPreviousProviderPhoto(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	u, _ := f.state.user(f.user.ID)
	if err := f.state.saveTailscaleIdentity(u.ID, "old@example.test", u.ID, tokenHash(f.cookie.Value), mustUserRevision(t, f)); err != nil {
		t.Fatal(err)
	}
	u, _ = f.state.user(u.ID)
	if _, err := f.state.rememberTailscalePhoto(u, "https://images.example.test/old.png"); err != nil {
		t.Fatal(err)
	}
	if err := f.state.saveTailscaleIdentity(u.ID, "new@example.test", u.ID, tokenHash(f.cookie.Value), mustUserRevision(t, f)); err != nil {
		t.Fatal(err)
	}
	u, _ = f.state.user(u.ID)
	if u.Picture != "" {
		t.Fatal("previous identity's provider photo survived relinking")
	}
}

type auditPausedBody struct {
	io.Reader
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *auditPausedBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered); <-b.release })
	return b.Reader.Read(p)
}
func (b *auditPausedBody) Close() error { return nil }

func TestDemotedAdminCannotFinishPausedUserPromotion(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	backup := localUser{ID: "u_audit_backup", Username: "backup", Name: "Recovery", Groups: []string{"admin"}, Enabled: true}
	if err := f.state.saveUser(backup, "recovery-password", f.user.ID); err != nil {
		t.Fatal(err)
	}
	u, _ := f.state.user(f.user.ID)
	revision := mustUserRevision(t, f)
	// A former administrator can predict the single revision increment for demotion.
	raw, _ := json.Marshal(map[string]any{"revision": revision + 1, "user": u, "password": ""})
	body := &auditPausedBody{Reader: strings.NewReader(string(raw)), entered: make(chan struct{}), release: make(chan struct{})}
	r := httptest.NewRequest("POST", "http://anchor.test/api/admin/users", nil)
	r.Body = body
	r.RemoteAddr = "192.0.2.10:1234"
	r.Header.Set("Origin", "http://anchor.test")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-CSRF-Token", f.csrf)
	r.AddCookie(f.cookie)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); f.handler.ServeHTTP(w, r) }()
	select {
	case <-body.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request never began decoding")
	}
	u.Groups = []string{"family"}
	err := f.state.saveUser(u, "", backup.ID, revision)
	close(body.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not finish")
	}
	if err != nil {
		t.Fatal(err)
	}
	current, _ := f.state.user(u.ID)
	if w.Code == 200 || groupAdmin(current.Groups) {
		t.Fatalf("demoted administrator regained privileges: HTTP %d", w.Code)
	}
}

func TestRevokedAdministratorCannotFinishPausedMutations(t *testing.T) {
	for _, revoke := range []string{"demote", "logout"} {
		for _, operation := range []string{"users", "config", "secrets", "tailscale", "setup", "delete"} {
			t.Run(revoke+"/"+operation, func(t *testing.T) {
				f := newAccountFixture(t)
				f.setup(t)
				backup := localUser{ID: "u_audit_recovery", Username: "recovery", Name: "Recovery", Groups: []string{"admin"}, Enabled: true}
				if err := f.state.saveUser(backup, "recovery-password", f.user.ID); err != nil {
					t.Fatal(err)
				}
				revision := mustUserRevision(t, f)
				if revoke == "demote" {
					revision++
				}
				method, path := "POST", "/api/admin/"+operation
				var payload any
				switch operation {
				case "users":
					payload = map[string]any{"revision": revision, "user": f.user, "password": ""}
				case "config":
					cfg, _ := f.state.store.Get()
					copyCfg := *cfg
					copyCfg.Title = "Unauthorized change"
					payload = map[string]any{"revision": revision, "config": copyCfg}
				case "secrets":
					payload = map[string]any{"revision": revision, "values": map[string]string{"plex": "synthetic-key"}}
				case "tailscale":
					payload = map[string]any{"revision": revision, "id": backup.ID, "email": "changed@example.test"}
				case "setup":
					progress, _ := f.state.onboarding()
					progress.Step = 1
					payload = progress
				case "delete":
					method, path = "DELETE", "/api/admin/users/"+backup.ID
					payload = map[string]any{"revision": revision}
				}
				raw, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				body := &auditPausedBody{Reader: strings.NewReader(string(raw)), entered: make(chan struct{}), release: make(chan struct{})}
				r := httptest.NewRequest(method, "http://anchor.test"+path, nil)
				r.Body = body
				r.RemoteAddr = "192.0.2.10:1234"
				r.Header.Set("Origin", "http://anchor.test")
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("X-CSRF-Token", f.csrf)
				r.AddCookie(f.cookie)
				w := httptest.NewRecorder()
				done := make(chan struct{})
				go func() { defer close(done); f.handler.ServeHTTP(w, r) }()
				select {
				case <-body.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("request never decoded")
				}
				if revoke == "demote" {
					user, _ := f.state.user(f.user.ID)
					user.Groups = []string{"family"}
					err = f.state.saveUser(user, "", backup.ID)
				} else {
					if response := f.request("POST", "/api/auth/logout", map[string]any{}, true); response.Code != 200 {
						err = io.ErrUnexpectedEOF
					}
				}
				close(body.release)
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("paused request did not finish")
				}
				if err != nil {
					t.Fatal(err)
				}
				if w.Code != 403 {
					t.Fatalf("revoked administrator mutation returned HTTP %d: %s", w.Code, w.Body.String())
				}
			})
		}
	}
}
