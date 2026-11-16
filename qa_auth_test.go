package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestPublicOriginDefaultPorts(t *testing.T) {
	for _, tc := range []struct {
		name, publicURL, target string
		want                    int
	}{
		{"HTTP default port omitted by browser", "http://anchor.test:80", "http://anchor.test/api/auth/status", http.StatusOK},
		{"HTTP default port supplied", "http://anchor.test", "http://anchor.test:80/api/auth/status", http.StatusOK},
		{"HTTPS default port omitted by browser", "https://anchor.test:443", "https://anchor.test/api/auth/status", http.StatusOK},
		{"HTTPS default port supplied", "https://anchor.test", "https://anchor.test:443/api/auth/status", http.StatusOK},
		{"host case", "https://ANCHOR.test:443", "https://anchor.test/api/auth/status", http.StatusOK},
		{"different port", "https://anchor.test:443", "https://anchor.test:444/api/auth/status", http.StatusMisdirectedRequest},
		{"different host", "https://anchor.test", "https://other.test/api/auth/status", http.StatusMisdirectedRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAccountFixture(t)
			f.auth.publicURL = tc.publicURL
			r := httptest.NewRequest(http.MethodGet, tc.target, nil)
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("configured %s, request %s: got %d, want %d: %s", tc.publicURL, tc.target, w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func TestPersistentStoreFailuresDoNotReplaceData(t *testing.T) {
	t.Run("corrupt database", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "anchor.db")
		original := []byte("synthetic corrupt database fixture")
		if err := os.WriteFile(path, original, 0600); err != nil {
			t.Fatal(err)
		}
		store, err := newPersistentStore(dir)
		if err == nil {
			store.persistent.db.Close()
			t.Fatal("corrupt database opened successfully")
		}
		current, err := os.ReadFile(path)
		if err != nil || string(current) != string(original) {
			t.Fatal("startup replaced a corrupt database")
		}
	})
	t.Run("configuration parent is a file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
		if store, err := newPersistentStore(filepath.Join(path, "config")); err == nil {
			store.persistent.db.Close()
			t.Fatal("startup accepted an unusable data directory")
		}
	})
	t.Run("SQLite exclusive lock", func(t *testing.T) {
		f := newAccountFixture(t)
		f.setup(t)
		revision, _ := f.state.revision()
		cfg, _ := f.state.store.Get()
		copyCfg := *cfg
		copyCfg.Title = "Must not apply"
		locker, err := sql.Open("sqlite", filepath.Join(f.state.dir, "anchor.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer locker.Close()
		if _, err := locker.Exec(`BEGIN EXCLUSIVE`); err != nil {
			t.Fatal(err)
		}
		defer locker.Exec(`ROLLBACK`)
		if _, err := f.state.db.Exec(`PRAGMA busy_timeout=20`); err != nil {
			t.Fatal(err)
		}
		if err := f.state.saveConfig(&copyCfg, revision, f.user.ID); err == nil {
			t.Fatal("write succeeded through an exclusive SQLite lock")
		}
		if _, err := locker.Exec(`ROLLBACK`); err != nil {
			t.Fatal(err)
		}
		after, _ := f.state.store.Get()
		afterRevision, err := f.state.revision()
		if err != nil || afterRevision != revision || after.Title != cfg.Title {
			t.Fatal("failed SQLite write partially changed state")
		}
	})
}

func TestAdministratorRecoveryRevokesSessions(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	old, _ := f.state.user(f.user.ID)
	f.state.db.Close() // Recovery is an offline operation, even in this fixture.
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previousOutput := os.Stdout
	os.Stdout = writer
	err = recoverAdministrator(f.state.dir, " ADMIN ")
	os.Stdout = previousOutput
	writer.Close()
	output, readErr := io.ReadAll(reader)
	reader.Close()
	if err != nil || readErr != nil {
		t.Fatalf("offline recovery failed: %v, %v", err, readErr)
	}
	// Inspect the generated synthetic password in memory; never print it.
	firstLine := strings.SplitN(string(output), "\n", 2)[0]
	separator := strings.LastIndex(firstLine, ": ")
	if separator < 0 {
		t.Fatal("recovery did not produce its temporary password")
	}
	password := firstLine[separator+2:]
	store, err := newPersistentStore(f.state.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.persistent.db.Close()
	updated, ok := store.persistent.user(old.ID)
	if !ok || updated.ID != old.ID || updated.authVersion <= old.authVersion || !checkPassword(updated.password, password) || checkPassword(updated.password, "correct-password-123") {
		t.Fatal("recovery did not replace the password on the stable account")
	}
	var sessions int
	if err := store.persistent.db.QueryRow(`SELECT count(*) FROM sessions WHERE user_id=?`, old.ID).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatal("recovery retained existing sessions")
	}
	if err := recoverAdministrator(f.state.dir, "missing"); err == nil {
		t.Fatal("recovery accepted an unknown administrator")
	}
}

func TestConcurrentSettingsAndInvalidImportAreAtomic(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	revision, _ := f.state.revision()
	cfg, _ := f.state.store.Get()
	results := make(chan error, 2)
	for _, title := range []string{"First writer", "Second writer"} {
		go func() {
			copyCfg := *cfg
			copyCfg.Title = title
			results <- f.state.saveConfig(&copyCfg, revision, f.user.ID)
		}()
	}
	winners, conflicts := 0, 0
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			winners++
		case errors.Is(err, errConflict):
			conflicts++
		default:
			t.Fatalf("concurrent save failed unexpectedly: %v", err)
		}
	}
	afterRevision, err := f.state.revision()
	if err != nil || winners != 1 || conflicts != 1 || afterRevision != revision+1 {
		t.Fatalf("concurrent settings lost an update: %d winners, %d conflicts, revision %d: %v", winners, conflicts, afterRevision, err)
	}
	revision = afterRevision
	if err := f.state.saveSecrets(map[string]string{"plex": "synthetic-original"}, revision, f.user.ID); err != nil {
		t.Fatal(err)
	}
	revision, _ = f.state.revision()
	if err := f.state.saveSecrets(map[string]string{"plex": "must-not-apply", "unknown": "invalid"}, revision, f.user.ID); err == nil {
		t.Fatal("invalid integration key batch accepted")
	}
	afterRevision, _ = f.state.revision()
	cfg, _ = f.state.store.Get()
	if afterRevision != revision || cfg.credentials["plex"] != "synthetic-original" {
		t.Fatal("invalid key batch partially changed settings")
	}
	for name, document := range map[string]string{
		"future schema":   "schema_version: 2\ndashboard: {}\n",
		"unknown field":   "schema_version: 1\ndashboard: {unknown: true}\n",
		"multiple docs":   "schema_version: 1\ndashboard: {}\n---\n{}\n",
		"malformed":       "schema_version: [\n",
		"unsafe tile URL": "schema_version: 1\ndashboard:\n  apps: [{id: bad, name: Bad, url: 'javascript:alert(1)', access: [everyone]}]\n",
	} {
		t.Run(name, func(t *testing.T) {
			w := f.request("POST", "/api/admin/import/preview", map[string]string{"yaml": document}, true)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("invalid import: %d %s", w.Code, w.Body.String())
			}
			if current, _ := f.state.revision(); current != revision {
				t.Fatal("invalid import mutated settings")
			}
		})
	}
}

func TestInterruptedSetupTokenCannotAuthorizeSetup(t *testing.T) {
	for name, token := range map[string]string{"empty": "", "partial": "partial-token", "nonhex": strings.Repeat("z", 64)} {
		t.Run(name, func(t *testing.T) {
			f := newAccountFixture(t)
			if err := os.WriteFile(filepath.Join(f.state.dir, "setup-token"), []byte(token), 0600); err != nil {
				t.Fatal(err)
			}
			w := f.request("POST", "/api/auth/setup", map[string]string{
				"Token": token, "Username": "intruder", "Name": "Synthetic intruder", "Password": "synthetic-password-123",
			}, false)
			if w.Code != http.StatusForbidden || !f.state.needsSetup() {
				t.Fatalf("incomplete setup token authorized first administrator: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestSetupTokenRestartValidationAndConcurrentSetup(t *testing.T) {
	for name, token := range map[string]string{"empty": "", "partial": "1234", "nonhex": strings.Repeat("z", 64)} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "setup-token"), []byte(token), 0600); err != nil {
				t.Fatal(err)
			}
			store, err := newPersistentStore(dir)
			if err == nil {
				store.persistent.db.Close()
				t.Fatal("startup accepted a malformed setup token")
			}
			if !strings.Contains(err.Error(), "setup-token is incomplete or invalid") {
				t.Fatalf("unhelpful malformed-token error: %v", err)
			}
		})
	}
	dir := t.TempDir()
	store, err := newPersistentStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "setup-token"))
	store.persistent.db.Close()
	if err != nil {
		t.Fatal(err)
	}
	store, err = newPersistentStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	store.persistent.db.Close()
	after, err := os.ReadFile(filepath.Join(dir, "setup-token"))
	if err != nil || string(before) != string(after) || !validSetupToken(string(after)) {
		t.Fatal("restart replaced or invalidated a valid setup token")
	}
	f := newAccountFixture(t)
	token, err := os.ReadFile(filepath.Join(f.state.dir, "setup-token"))
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan int, 2)
	for _, username := range []string{"first", "second"} {
		go func() {
			w := f.request("POST", "/api/auth/setup", map[string]string{
				"Token": string(token), "Username": username, "Name": username, "Password": "synthetic-setup-password",
			}, false)
			results <- w.Code
		}()
	}
	counts := map[int]int{<-results: 1}
	counts[<-results]++
	if counts[http.StatusOK] != 1 || counts[http.StatusConflict] != 1 {
		t.Fatalf("concurrent setup did not produce one winner: %v", counts)
	}
	users, err := f.state.users()
	if err != nil || len(users) != 1 {
		t.Fatalf("concurrent setup created unexpected accounts: %d %v", len(users), err)
	}
	if _, err := os.Stat(filepath.Join(f.state.dir, "setup-token")); !os.IsNotExist(err) {
		t.Fatal("successful setup left the token behind")
	}
}

func TestIntegrationAddressesRequireHostname(t *testing.T) {
	for _, raw := range []string{"http://:8080", "https://:443/api", "http://[]:8080"} {
		if _, err := validUpstream(raw); err == nil {
			t.Errorf("accepted integration address without hostname: %s", raw)
		}
	}
}

func TestAuthAdminRouteBoundariesAndStrictJSON(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	adminCookie, adminCSRF := f.cookie, f.csrf
	member := localUser{ID: "u_qa_member", Username: "qa-member", Name: "QA member", Groups: []string{"family"}, Enabled: true}
	if err := f.state.saveUser(member, "synthetic-member-password", f.user.ID); err != nil {
		t.Fatal(err)
	}
	member, _ = f.state.user(member.ID)
	issued := httptest.NewRecorder()
	if err := f.auth.issue(issued, member); err != nil {
		t.Fatal(err)
	}
	f.cookie = issued.Result().Cookies()[0]
	var status struct {
		CSRF string `json:"csrf"`
	}
	if err := json.Unmarshal(f.request("GET", "/api/auth/status", nil, false).Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	f.csrf = status.CSRF
	adminRoutes := []struct{ method, path string }{
		{"GET", "/api/admin/state"}, {"GET", "/api/admin/icons/plex"},
		{"POST", "/api/admin/config"}, {"POST", "/api/admin/users"},
		{"POST", "/api/admin/secrets"}, {"GET", "/api/admin/export"},
		{"POST", "/api/admin/backup"}, {"POST", "/api/admin/import/preview"},
	}
	for _, route := range adminRoutes {
		t.Run("member "+route.path, func(t *testing.T) {
			if w := f.request(route.method, route.path, map[string]any{}, true); w.Code != http.StatusForbidden {
				t.Fatalf("member admin request: %d %s", w.Code, w.Body.String())
			}
		})
	}
	f.cookie, f.csrf = nil, ""
	for _, route := range adminRoutes {
		t.Run("anonymous "+route.path, func(t *testing.T) {
			if w := f.request(route.method, route.path, map[string]any{}, false); w.Code != http.StatusForbidden {
				t.Fatalf("anonymous admin request: %d %s", w.Code, w.Body.String())
			}
		})
	}
	f.cookie, f.csrf = adminCookie, adminCSRF
	for _, route := range append(adminRoutes, []struct{ method, path string }{
		{"GET", "/api/auth/status"}, {"POST", "/api/auth/setup"}, {"POST", "/api/auth/login"},
		{"POST", "/api/auth/logout"}, {"POST", "/api/auth/password"},
	}...) {
		t.Run("unsupported method "+route.path, func(t *testing.T) {
			w := f.request(http.MethodDelete, route.path, map[string]any{}, true)
			if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") == "" {
				t.Fatalf("unsupported method: %d %s", w.Code, w.Body.String())
			}
		})
	}
	for _, path := range []string{"/api/auth/login", "/api/auth/setup", "/api/auth/password", "/api/admin/config", "/api/admin/users", "/api/admin/secrets", "/api/admin/import/preview"} {
		for name, raw := range map[string]string{
			"malformed": "{", "empty": "", "unknown property": `{"unexpected":true}`,
			"multiple documents": `{} {}`, "oversized": `{"unexpected":"` + strings.Repeat("x", 2<<20) + `"}`,
		} {
			t.Run(path+" "+name, func(t *testing.T) {
				r := httptest.NewRequest(http.MethodPost, "http://anchor.test"+path, strings.NewReader(raw))
				r.RemoteAddr = "192.0.2.10:1234"
				r.AddCookie(adminCookie)
				r.Header.Set("Origin", "http://anchor.test")
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("X-CSRF-Token", adminCSRF)
				w := httptest.NewRecorder()
				f.handler.ServeHTTP(w, r)
				if w.Code != http.StatusBadRequest || w.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("invalid JSON: %d %s", w.Code, w.Body.String())
				}
			})
		}
	}
}

func TestSessionExpiryAdmissionAndSecureCookie(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	user, _ := f.state.user(f.user.ID)
	for i := 0; i < 12; i++ {
		w := httptest.NewRecorder()
		f.auth.mode = "proxy"
		if err := f.auth.issue(w, user); err != nil {
			t.Fatal(err)
		}
		cookie := w.Result().Cookies()[0]
		f.cookie = cookie
		if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.MaxAge != 12*60*60 {
			t.Fatal("HTTPS session cookie lacks required restrictions")
		}
	}
	var count int
	if err := f.state.db.QueryRow(`SELECT count(*) FROM sessions WHERE user_id=?`, f.user.ID).Scan(&count); err != nil || count != 10 {
		t.Fatalf("unbounded sessions: %d, %v", count, err)
	}
	if w := f.request("GET", "/api/admin/state", nil, false); w.Code != http.StatusOK {
		t.Fatalf("current session not valid before expiry: %d", w.Code)
	}
	if _, err := f.state.db.Exec(`UPDATE sessions SET expires=?`, time.Now().Add(-time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	if w := f.request("GET", "/api/admin/state", nil, false); w.Code != http.StatusForbidden {
		t.Fatalf("expired session retained access: %d", w.Code)
	}
	f.cookie = nil
	f.auth.slots <- struct{}{}
	f.auth.slots <- struct{}{}
	w := f.request("POST", "/api/auth/login", map[string]string{"Username": "admin", "Password": "correct-password-123"}, false)
	<-f.auth.slots
	<-f.auth.slots
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("unbounded password work: %d", w.Code)
	}
	f.auth.loginLimits = newRateLimiter(time.Hour, 3)
	w = f.request("POST", "/api/auth/login", map[string]string{"Username": "unknown", "Password": "synthetic-wrong-password"}, false)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unknown login: %d", w.Code)
	}
	if w := f.request("POST", "/api/auth/login", map[string]string{}, false); w.Code != http.StatusTooManyRequests {
		t.Fatalf("repeated sign-in not limited: %d", w.Code)
	}
}

func TestUserGrantValidationMatchesRuntimeNormalization(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	for _, tc := range []struct {
		name, rule string
		notice     bool
	}{
		{"normalized unknown application grant", " U_MISSING ", false},
		{"unknown notice grant", "u_missing", true},
		{"normalized unknown notice grant", " U_MISSING ", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _ := f.state.store.Get()
			copyCfg := *cfg
			copyCfg.People = nil
			if tc.notice {
				copyCfg.Notice = &Notice{Message: "Private notice", Access: []string{tc.rule}}
			} else {
				copyCfg.Apps = []App{{ID: "private", Name: "Private", URL: "https://app.example.test", Access: []string{tc.rule}, Check: "off"}}
			}
			revision, _ := f.state.revision()
			w := f.request(http.MethodPost, "/api/admin/config", map[string]any{"revision": revision, "config": copyCfg}, true)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "unknown user permission") {
				t.Errorf("unknown user grant accepted: %d %s", w.Code, w.Body.String())
			}
			if after, _ := f.state.revision(); after != revision {
				t.Error("invalid user grant changed settings")
			}
			raw, err := yaml.Marshal(dashboardDocument{SchemaVersion: 1, Dashboard: copyCfg})
			if err != nil {
				t.Fatal(err)
			}
			w = f.request(http.MethodPost, "/api/admin/import/preview", map[string]string{"yaml": string(raw)}, true)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "unknown user permission") {
				t.Fatalf("preview accepted unknown user grant: %d %s", w.Code, w.Body.String())
			}
		})
	}
	// A grant to an existing stable ID uses exactly the same normalization as
	// the runtime permission check, and ordinary group labels remain valid.
	cfg, _ := f.state.store.Get()
	copyCfg := *cfg
	copyCfg.Apps = []App{{ID: "known", Name: "Known", URL: "https://app.example.test", Access: []string{" " + strings.ToUpper(f.user.ID) + " ", "family"}, Check: "off"}}
	copyCfg.Notice = &Notice{Message: "Known user", Access: []string{f.user.ID}}
	revision, _ := f.state.revision()
	w := f.request(http.MethodPost, "/api/admin/config", map[string]any{"revision": revision, "config": copyCfg}, true)
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	updated, _ := f.state.store.Get()
	if !updated.visibleTo(&updated.Apps[0], f.user.ID) || updated.noticeFor(f.user.ID) == nil {
		t.Fatal("known normalized user grant lost access")
	}
}
