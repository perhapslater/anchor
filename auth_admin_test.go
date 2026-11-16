package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type accountFixture struct {
	state   *persistentState
	auth    *authentication
	handler http.Handler
	cookie  *http.Cookie
	csrf    string
	user    localUser
}

func newAccountFixture(t *testing.T) *accountFixture {
	t.Helper()
	store, err := newPersistentStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.persistent.db.Close() })
	auth := &authentication{state: store.persistent, mode: "lan", publicURL: "http://anchor.test", csrfKey: randomToken(), loginLimits: newRateLimiter(0, 1000), slots: make(chan struct{}, 2)}
	h := &handler{store: store, auth: auth, checker: NewChecker(store)}
	mux := http.NewServeMux()
	auth.routes(mux)
	h.adminRoutes(mux)
	mux.HandleFunc("GET /api/me", h.me)
	return &accountFixture{state: store.persistent, auth: auth, handler: externalScheme("http", auth.middleware(mux))}
}
func (f *accountFixture) request(method, path string, body any, csrf bool) *httptest.ResponseRecorder {
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(method, "http://anchor.test"+path, bytes.NewReader(data))
	r.RemoteAddr = "192.0.2.10:1234"
	if method != "GET" {
		r.Header.Set("Origin", "http://anchor.test")
		r.Header.Set("Content-Type", "application/json")
	}
	if f.cookie != nil {
		r.AddCookie(f.cookie)
	}
	if csrf {
		r.Header.Set("X-CSRF-Token", f.csrf)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}
func (f *accountFixture) setup(t *testing.T) {
	t.Helper()
	token, err := os.ReadFile(filepath.Join(f.state.dir, "setup-token"))
	if err != nil {
		t.Fatal(err)
	}
	response := f.request("POST", "/api/auth/setup", map[string]string{"Token": string(token), "Username": "admin", "Name": "Administrator", "Password": "correct-password-123"}, false)
	if response.Code != 200 {
		t.Fatalf("setup %d: %s", response.Code, response.Body.String())
	}
	f.cookie = response.Result().Cookies()[0]
	var status struct {
		CSRF string    `json:"csrf"`
		User localUser `json:"user"`
	}
	response = f.request("GET", "/api/auth/status", nil, false)
	if err = json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	f.csrf = status.CSRF
	f.user = status.User
}
func TestSetupAndSessionBoundary(t *testing.T) {
	f := newAccountFixture(t)
	if w := f.request("GET", "/api/admin/state", nil, false); w.Code != 403 {
		t.Fatal("anonymous admin access")
	}
	if w := f.request("POST", "/api/auth/setup", map[string]string{"Token": "wrong"}, false); w.Code != 403 {
		t.Fatal("setup token bypass")
	}
	f.setup(t)
	if _, err := os.Stat(filepath.Join(f.state.dir, "setup-token")); !os.IsNotExist(err) {
		t.Fatal("setup token was not consumed")
	}
	if w := f.request("POST", "/api/auth/setup", map[string]string{}, false); w.Code != 409 {
		t.Fatal("setup could be repeated")
	}
	if f.cookie.HttpOnly != true || f.cookie.Secure || f.cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("LAN session cookie policy")
	}
	if w := f.request("GET", "/api/admin/state", nil, false); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	revision, _ := f.state.revision()
	body := map[string]any{"revision": revision, "values": map[string]string{"plex": "private-plex-key"}}
	if w := f.request("POST", "/api/admin/secrets", body, false); w.Code != 403 {
		t.Fatal("missing CSRF accepted")
	}
	if w := f.request("POST", "/api/admin/secrets", body, true); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, path := range []string{"/api/admin/state", "/api/admin/export"} {
		w := f.request("GET", path, nil, false)
		if strings.Contains(w.Body.String(), "private-plex-key") || strings.Contains(w.Body.String(), "argon2id$") {
			t.Fatal("secret leaked through " + path)
		}
	}
	backup := f.request("POST", "/api/admin/backup", map[string]any{}, true)
	if backup.Code != 200 || !bytes.HasPrefix(backup.Body.Bytes(), []byte("SQLite format 3")) {
		t.Fatal("database backup failed")
	}
	restoreDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(restoreDir, "anchor.db"), backup.Body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	restored, err := newPersistentStore(restoreDir)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.persistent.db.Close()
	cfg, _ := restored.Get()
	if cfg.credentials["plex"] != "private-plex-key" {
		t.Fatal("backup did not preserve secrets")
	}
	if w := f.request("POST", "/api/auth/logout", map[string]any{}, true); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := f.request("GET", "/api/admin/state", nil, false); w.Code != 403 {
		t.Fatal("revoked session accepted")
	}
}
func TestSQLiteSaveConflictAndLastAdmin(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	revision, _ := f.state.revision()
	cfg, _ := f.state.store.Get()
	copyCfg := *cfg
	copyCfg.Title = "Updated"
	if err := f.state.saveConfig(&copyCfg, revision, f.user.ID); err != nil {
		t.Fatal(err)
	}
	copyCfg.Title = "Stale"
	if err := f.state.saveConfig(&copyCfg, revision, f.user.ID); err != errConflict {
		t.Fatalf("stale save: %v", err)
	}
	cfg, _ = f.state.store.Get()
	if cfg.Title != "Updated" {
		t.Fatal("stale save changed settings")
	}
	disabled := f.user
	disabled.Enabled = false
	if err := f.state.saveUser(disabled, "", f.user.ID); err == nil {
		t.Fatal("last local administrator disabled")
	}
	if err := f.state.saveUser(f.user, "new-password-1234", f.user.ID); err != nil {
		t.Fatal(err)
	}
	if w := f.request("GET", "/api/admin/state", nil, false); w.Code != 403 {
		t.Fatal("password change did not revoke sessions")
	}
	f.cookie = nil
	if w := f.request("POST", "/api/auth/login", map[string]string{"Username": "admin", "Password": "correct-password-123"}, false); w.Code != 401 {
		t.Fatal("old password still works")
	}
	if w := f.request("POST", "/api/auth/login", map[string]string{"Username": "admin", "Password": "new-password-1234"}, false); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
}
func TestExplicitTailscaleLinkAndMemberAuthorization(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	member := localUser{ID: "u_member", Username: "member", Name: "Member", Tailscale: "member@example.test", Groups: []string{"family"}, Enabled: true}
	if err := f.state.saveUser(member, "member-password-123", f.user.ID); err != nil {
		t.Fatal(err)
	}
	f.auth.mode = "tailscale"
	for _, test := range []struct {
		remote, login string
		known         bool
	}{{"192.0.2.10:12", "member@example.test", false}, {"127.0.0.1:12", "unknown@example.test", false}, {"127.0.0.1:12", "member@example.test", true}} {
		r := httptest.NewRequest("GET", "http://anchor.test/api/auth/status", nil)
		r.RemoteAddr = test.remote
		r.Header.Set("Tailscale-User-Login", test.login)
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		var status struct {
			Authenticated bool `json:"authenticated"`
		}
		json.Unmarshal(w.Body.Bytes(), &status)
		if status.Authenticated != test.known {
			t.Fatalf("identity boundary: %+v %s", test, w.Body.String())
		}
	}
	r := httptest.NewRequest("GET", "http://anchor.test/api/admin/state", nil)
	r.RemoteAddr = "127.0.0.1:12"
	r.Header.Set("Tailscale-User-Login", "member@example.test")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("member obtained admin access")
	}
}
func TestYAMLPreviewDoesNotApplyAndRejectsAccounts(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	exported := f.request("GET", "/api/admin/export", nil, false)
	var doc dashboardDocument
	if err := yaml.Unmarshal(exported.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	doc.Dashboard.Title = "Preview only"
	raw, _ := yaml.Marshal(doc)
	w := f.request("POST", "/api/admin/import/preview", map[string]string{"yaml": string(raw)}, true)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	cfg, _ := f.state.store.Get()
	if cfg.Title == "Preview only" {
		t.Fatal("preview applied changes")
	}
	doc.Dashboard.People = map[string]Person{"attacker@example.test": {Name: "Attacker", Groups: []string{"admin"}}}
	raw, _ = yaml.Marshal(doc)
	w = f.request("POST", "/api/admin/import/preview", map[string]string{"yaml": string(raw)}, true)
	if w.Code != 400 {
		t.Fatal("import could replace accounts")
	}
	r := httptest.NewRequest("POST", "http://anchor.test/api/admin/config", strings.NewReader(`{}`))
	r.RemoteAddr = "127.0.0.1:2"
	r.AddCookie(f.cookie)
	r.Header.Set("Origin", "https://evil.example")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-CSRF-Token", f.csrf)
	w = httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross-origin write accepted")
	}
}

func TestDatabaseKeyRotationReplacesRuntimeCredentials(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	revision, _ := f.state.revision()
	cfg, _ := f.state.store.Get()
	copyCfg := *cfg
	copyCfg.Apps = []App{{ID: "media", Name: "Media", URL: "https://media.example.test", Access: []string{"everyone"}, Check: "off"}}
	copyCfg.Requests = &Requests{App: "media", Seerr: "https://seerr.example.test", Plex: "https://plex.example.test"}
	copyCfg.Ebooks = &EbooksConfig{App: "media", Server: "https://books.example.test", LibraryID: "library"}
	if err := f.state.saveConfig(&copyCfg, revision, f.user.ID); err != nil {
		t.Fatal(err)
	}
	revision, _ = f.state.revision()
	if err := f.state.saveSecrets(map[string]string{"seerr": "first", "plex": "plex-key", "abs": "book-key"}, revision, f.user.ID); err != nil {
		t.Fatal(err)
	}
	cfg, _ = f.state.store.Get()
	oldSig := cfg.Requests.sig()
	rs := &requestStatus{keys: requestKeys{Seerr: "environment-must-not-override"}}
	if rs.keysFor(cfg.Requests).Seerr != "first" {
		t.Fatal("request service did not use database key")
	}
	ebooks := &ebookService{key: "environment-must-not-override"}
	if ebooks.keyFor(cfg.Ebooks) != "book-key" {
		t.Fatal("ebook service did not use database key")
	}
	revision, _ = f.state.revision()
	if err := f.state.saveSecrets(map[string]string{"seerr": "second", "abs": ""}, revision, f.user.ID); err != nil {
		t.Fatal(err)
	}
	cfg, _ = f.state.store.Get()
	if cfg.Requests.sig() == oldSig || rs.keysFor(cfg.Requests).Seerr != "second" || ebooks.keyFor(cfg.Ebooks) != "" {
		t.Fatal("rotation/clear did not invalidate credentials and cache identity")
	}
}

func TestAccountChangeCannotRaceWithSessionCreation(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	before, ok := f.state.user(f.user.ID)
	if !ok {
		t.Fatal("missing administrator")
	}
	// A login could have verified this snapshot's password before an admin edit.
	if err := f.state.saveUser(before, "changed-password-123", before.ID); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	if err := f.auth.issue(w, before); err == nil {
		t.Fatal("stale login created a session after account change")
	}
	if len(w.Result().Cookies()) != 0 {
		t.Fatal("stale login issued a cookie")
	}
	after, ok := f.state.user(before.ID)
	if !ok {
		t.Fatal("missing updated administrator")
	}
	if err := f.auth.issue(httptest.NewRecorder(), after); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseMigrationPreservesExistingAccounts(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "anchor.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE users (id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE, name TEXT NOT NULL, tailscale TEXT UNIQUE, groups_json TEXT NOT NULL, enabled INTEGER NOT NULL, password TEXT NOT NULL); PRAGMA user_version=1;`)
	if err != nil {
		t.Fatal(err)
	}
	password, err := hashPassword("migration-password-123")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO users VALUES('u_legacy','legacy','Existing admin',NULL,'["admin"]',1,?)`, password)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	store, err := newPersistentStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.persistent.db.Close()
	user, ok := store.persistent.user("u_legacy")
	if !ok || user.authVersion != 1 || !checkPassword(user.password, "migration-password-123") {
		t.Fatal("migration lost the existing account")
	}
	var version int
	if err = store.persistent.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 6 {
		t.Fatal("schema version was not upgraded")
	}
	if store.persistent.needsSetup() {
		t.Fatal("migration reopened first-run setup")
	}
}
