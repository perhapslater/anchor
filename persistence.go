package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"
)

// SQLite owns application state. ConfigStore is an immutable read projection
// used by the existing dashboard and integration services.
type persistentState struct {
	db    *sql.DB
	mu    sync.Mutex
	store *ConfigStore
	dir   string
}
type localUser struct {
	ID               string   `json:"id"`
	Username         string   `json:"username"`
	Name             string   `json:"name"`
	Tailscale        string   `json:"tailscale"`
	Groups           []string `json:"groups"`
	Enabled          bool     `json:"enabled"`
	HasPassword      bool     `json:"has_password"`
	Picture          string   `json:"picture,omitempty"`
	CustomPicture    bool     `json:"custom_picture"`
	PhotoRevision    int64    `json:"photo_revision"`
	tailscalePicture string
	password         string
	authVersion      int
}

var errConflict = errors.New("settings changed; reload before saving")

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func validSetupToken(token string) bool {
	if len(token) != 64 {
		return false
	}
	_, err := hex.DecodeString(token)
	return err == nil
}

func tokenHash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func newPersistentStore(dir string) (*ConfigStore, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "anchor.db")
	// Create with restrictive permissions before SQLite opens the file.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	fail := func(e error) (*ConfigStore, error) { db.Close(); return nil, e }
	if _, err = db.Exec(`PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;`); err != nil {
		return fail(err)
	}
	var version int
	if err = db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fail(err)
	}
	if version > 6 {
		return fail(errors.New("database is newer than this Anchor build"))
	}
	tx, err := db.Begin()
	if err != nil {
		return fail(err)
	}
	_, err = tx.Exec(`CREATE TABLE IF NOT EXISTS settings (id INTEGER PRIMARY KEY CHECK(id=1), document TEXT NOT NULL, revision INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS users (id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE, name TEXT NOT NULL, tailscale TEXT UNIQUE, groups_json TEXT NOT NULL, enabled INTEGER NOT NULL, password TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS sessions (hash TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE, csrf TEXT NOT NULL, expires INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS user_photos (user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE, png BLOB CHECK(length(png)<=300000), hash TEXT NOT NULL DEFAULT '', tailscale_url TEXT NOT NULL DEFAULT '', revision INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS secrets (name TEXT PRIMARY KEY, value TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS audit (id INTEGER PRIMARY KEY, at INTEGER NOT NULL, actor TEXT NOT NULL, action TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS onboarding (id INTEGER PRIMARY KEY CHECK(id=1), step INTEGER NOT NULL, completed INTEGER NOT NULL, skipped TEXT NOT NULL, revision INTEGER NOT NULL);
 INSERT OR IGNORE INTO onboarding SELECT 1,0,CASE WHEN count(*)>0 THEN 1 ELSE 0 END,'[]',1 FROM users;
 `)
	if err != nil {
		tx.Rollback()
		return fail(err)
	}
	initial := Config{Title: "Anchor", Subtitle: "Your self-hosted applications", Contact: "the administrator", Sections: []string{"Apps"}, Apps: []App{}}
	raw, err := yaml.Marshal(initial)
	if err != nil {
		tx.Rollback()
		return fail(err)
	}
	_, err = tx.Exec(`INSERT OR IGNORE INTO settings VALUES(1,?,1)`, string(raw))
	if err != nil {
		tx.Rollback()
		return fail(err)
	}
	if version < 2 {
		if _, err = tx.Exec(`ALTER TABLE users ADD COLUMN auth_version INTEGER NOT NULL DEFAULT 1`); err != nil {
			tx.Rollback()
			return fail(err)
		}
	}
	if version < 3 {
		if err = migratePeopleBeforeTailscale(tx); err != nil {
			tx.Rollback()
			return fail(err)
		}
	}
	if version < 4 {
		if err = migrateConnectionToReview(tx); err != nil {
			tx.Rollback()
			return fail(err)
		}
	}
	if version < 5 {
		if err = migrateTailscaleBeforeReview(tx); err != nil {
			tx.Rollback()
			return fail(err)
		}
	}
	if _, err = tx.Exec(`PRAGMA user_version=6`); err != nil {
		tx.Rollback()
		return fail(err)
	}
	if err = tx.Commit(); err != nil {
		return fail(err)
	}
	store := &ConfigStore{}
	state := &persistentState{db: db, store: store, dir: dir}
	store.persistent = state
	if err = state.reload(); err != nil {
		return fail(err)
	}
	if state.needsSetup() {
		tokenPath := filepath.Join(dir, "setup-token")
		tokenFile, e := os.OpenFile(tokenPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e == nil {
			_, e = tokenFile.WriteString(randomToken())
			tokenFile.Close()
		}
		if e != nil && !os.IsExist(e) {
			return fail(e)
		}
		// An interrupted create/write can leave an empty or partial file. Never
		// accept that as a weak (or empty) setup credential on the next startup.
		token, e := os.ReadFile(tokenPath)
		if e != nil {
			return fail(e)
		}
		if !validSetupToken(strings.TrimSpace(string(token))) {
			return fail(errors.New("setup-token is incomplete or invalid; stop Anchor, remove only setup-token, then restart to generate a new token"))
		}
	}
	return store, nil
}
func (p *persistentState) needsSetup() bool {
	var n int
	err := p.db.QueryRow(`SELECT count(*) FROM users`).Scan(&n)
	return err == nil && n == 0
}
func (p *persistentState) users() ([]localUser, error) {
	rows, err := p.db.Query(`SELECT u.id,u.username,u.name,coalesce(u.tailscale,''),u.groups_json,u.enabled,u.password,u.auth_version,coalesce(p.hash,''),coalesce(p.tailscale_url,''),coalesce(p.revision,0) FROM users u LEFT JOIN user_photos p ON p.user_id=u.id ORDER BY u.name,u.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []localUser{}
	for rows.Next() {
		var u localUser
		var groups, photoHash string
		if err = rows.Scan(&u.ID, &u.Username, &u.Name, &u.Tailscale, &groups, &u.Enabled, &u.password, &u.authVersion, &photoHash, &u.tailscalePicture, &u.PhotoRevision); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(groups), &u.Groups); err != nil {
			return nil, err
		}
		u.HasPassword = u.password != ""
		u.Picture = u.tailscalePicture
		u.CustomPicture = photoHash != ""
		if u.CustomPicture {
			u.Picture = profilePhotoPath(u.ID, photoHash)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
func (p *persistentState) user(id string) (localUser, bool) {
	users, err := p.users()
	if err != nil {
		return localUser{}, false
	}
	for _, u := range users {
		if u.ID == id {
			return u, true
		}
	}
	return localUser{}, false
}
func (p *persistentState) revision() (int64, error) {
	var r int64
	err := p.db.QueryRow(`SELECT revision FROM settings WHERE id=1`).Scan(&r)
	return r, err
}
func (p *persistentState) reload() error {
	var raw string
	var rev int64
	if err := p.db.QueryRow(`SELECT document,revision FROM settings WHERE id=1`).Scan(&raw, &rev); err != nil {
		return err
	}
	cfg, err := parseConfig([]byte(raw))
	if err != nil {
		return err
	}
	users, err := p.users()
	if err != nil {
		return err
	}
	cfg.People = map[string]Person{}
	for _, u := range users {
		if u.Enabled {
			cfg.People[u.ID] = Person{Name: u.Name, Groups: u.Groups}
		}
	}
	cfg.credentials = map[string]string{}
	rows, err := p.db.Query(`SELECT name,value FROM secrets`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var k, v string
		if err = rows.Scan(&k, &v); err != nil {
			rows.Close()
			return err
		}
		cfg.credentials[k] = v
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	cfg.scope = tokenHash(fmt.Sprintf("%s:%d", raw, rev))
	for _, r := range cfg.requestConnections() {
		r.revision = fmt.Sprint(rev)
		r.keys = &requestKeys{Seerr: cfg.credentials[requestSecretName(r, "seerr")], Plex: cfg.credentials[requestSecretName(r, "plex")], Sonarr: cfg.credentials[requestSecretName(r, "sonarr")], Radarr: cfg.credentials[requestSecretName(r, "radarr")]}
	}
	for _, e := range cfg.ebookLibraries() {
		e.secret = cfg.credentials[ebookSecretName(e)]
		e.secretMode = true
	}
	p.store.mu.Lock()
	p.store.cfg = cfg
	p.store.lastErr = ""
	p.store.mu.Unlock()
	return nil
}
func (p *persistentState) saveConfig(cfg *Config, revision int64, actor string) error {
	return p.saveConfigAndSecrets(cfg, revision, actor, nil)
}

// Integration addresses and credentials change together, or neither changes.
func (p *persistentState) saveConfigAndSecrets(cfg *Config, revision int64, actor string, keys map[string]string) error {
	return p.saveConfigAndSecretsChecked(cfg, revision, actor, keys, nil)
}

func (p *persistentState) saveConfigAndSecretsChecked(cfg *Config, revision int64, actor string, keys map[string]string, check authorizationCheck) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if check != nil {
		if err := check(); err != nil {
			return err
		}
	}
	cfg.People = nil
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	checked, err := parseConfig(raw)
	if err != nil {
		return err
	}
	if err = p.validateUserGrants(checked); err != nil {
		return err
	}
	for name := range keys {
		if strings.Contains(name, "/") && !checked.usesScopedSecret(name) {
			return errors.New("API credential does not belong to a configured connection")
		}
	}
	if checked.Webhook != nil {
		endpoint := keys["webhook"]
		if _, supplied := keys["webhook"]; !supplied {
			current, _ := p.store.Get()
			endpoint = current.credentials["webhook"]
		}
		if _, err := validWebhookURL(endpoint, checked.Webhook.Format); err != nil {
			return err
		}
	}
	tx, err := p.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE settings SET document=?,revision=revision+1 WHERE id=1 AND revision=?`, string(raw), revision)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return errConflict
	}
	if err := writeSecrets(tx, keys); err != nil {
		return err
	}
	// Remove credentials belonging to deleted connections in the same commit.
	rows, err := tx.Query(`SELECT name FROM secrets`)
	if err != nil {
		return err
	}
	var obsolete []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		if strings.Contains(name, "/") && !checked.usesScopedSecret(name) {
			obsolete = append(obsolete, name)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, name := range obsolete {
		if _, err := tx.Exec(`DELETE FROM secrets WHERE name=?`, name); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`INSERT INTO audit(at,actor,action) VALUES(?,?,?)`, time.Now().Unix(), actor, "dashboard saved"); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return p.reload()
}

// User-specific grants must reference existing stable IDs. Validate using the
// same normalization as allowed, including notices and import previews.
func (p *persistentState) validateUserGrants(cfg *Config) error {
	users, err := p.users()
	if err != nil {
		return err
	}
	ids := map[string]bool{}
	for _, u := range users {
		ids[u.ID] = true
	}
	access := make([][]string, 0, len(cfg.Apps)+1)
	for _, app := range cfg.Apps {
		access = append(access, app.Access)
	}
	if cfg.Notice != nil {
		access = append(access, cfg.Notice.Access)
	}
	for _, rules := range access {
		for _, rule := range rules {
			rule = strings.ToLower(strings.TrimSpace(rule))
			if strings.HasPrefix(rule, "u_") && !ids[rule] {
				return fmt.Errorf("unknown user permission %q", rule)
			}
		}
	}
	return nil
}

func groupAdmin(groups []string) bool {
	for _, g := range groups {
		if strings.EqualFold(g, "admin") {
			return true
		}
	}
	return false
}

func sameGroupMemberships(a, b []string) bool {
	groups := func(values []string) map[string]bool {
		set := make(map[string]bool, len(values))
		for _, group := range values {
			set[strings.ToLower(strings.TrimSpace(group))] = true
		}
		return set
	}
	left, right := groups(a), groups(b)
	if len(left) != len(right) {
		return false
	}
	for group := range left {
		if !right[group] {
			return false
		}
	}
	return true
}

func (p *persistentState) saveUser(u localUser, password, actor string, expected ...int64) error {
	return p.saveUserSignIn(u, password, false, actor, expected...)
}

func (p *persistentState) saveUserSignIn(u localUser, password string, disableLocal bool, actor string, expected ...int64) error {
	return p.saveUserSignInChecked(u, password, disableLocal, actor, nil, expected...)
}

func (p *persistentState) saveUserSignInChecked(u localUser, password string, disableLocal bool, actor string, check authorizationCheck, expected ...int64) error {
	if disableLocal && password != "" {
		return errors.New("cannot set a password while disabling local sign-in")
	}
	u.Username = strings.ToLower(strings.TrimSpace(u.Username))
	u.Tailscale = strings.ToLower(strings.TrimSpace(u.Tailscale))
	u.Name = strings.TrimSpace(u.Name)
	if u.Name == "" || len(u.Name) > 120 || len(u.Username) > 120 || len(u.Tailscale) > 254 {
		return errors.New("provide a name and valid login fields")
	}
	if u.Username != "" && !localLoginPattern.MatchString(u.Username) {
		return errors.New("local login must use letters, digits, dots, @, underscores or hyphens")
	}
	if len(u.Groups) > 50 {
		return errors.New("too many groups")
	}
	for i, g := range u.Groups {
		g = strings.ToLower(strings.TrimSpace(g))
		if !groupPattern.MatchString(g) {
			return errors.New("group names must use letters, digits, underscores or hyphens")
		}
		u.Groups[i] = g
	}
	if u.ID == "" {
		u.ID = "u_" + randomToken()[:20]
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if check != nil {
		if err := check(); err != nil {
			return err
		}
	}
	if len(expected) > 0 {
		revision, e := p.revision()
		if e != nil {
			return e
		}
		if revision != expected[0] {
			return errConflict
		}
	}
	users, err := p.users()
	if err != nil {
		return err
	}
	exists := false
	var previous localUser
	for _, old := range users {
		if old.ID == u.ID {
			u.password = old.password
			previous = old
			exists = true
		}
	}
	if !exists && actor != "setup" && strings.HasPrefix(u.ID, "u_") == false {
		return errors.New("invalid user id")
	}
	if password != "" {
		u.password, err = hashPassword(password)
		if err != nil {
			return err
		}
	}
	if disableLocal {
		u.password = ""
	}
	if u.Username == "" {
		u.Username = u.ID
	}
	if u.password == "" && u.Tailscale == "" {
		return errors.New("provide a local password or a Tailscale identity")
	}
	usableAdmin := u.Enabled && groupAdmin(u.Groups) && u.password != ""
	for _, old := range users {
		if old.ID != u.ID && old.Enabled && groupAdmin(old.Groups) && old.password != "" {
			usableAdmin = true
		}
	}
	if !usableAdmin {
		return errors.New("keep at least one enabled administrator with a local password")
	}
	// Profile edits and unchanged saves do not affect authentication. Compare
	// effective group membership so ordering, case and duplicates are harmless.
	revokeSessions := exists && (previous.password != u.password || previous.Username != u.Username ||
		previous.Tailscale != u.Tailscale || previous.Enabled != u.Enabled || !sameGroupMemberships(previous.Groups, u.Groups))
	authVersionChange := 0
	if revokeSessions {
		authVersionChange = 1
	}
	groups, _ := json.Marshal(u.Groups)
	var ts any
	if u.Tailscale != "" {
		ts = u.Tailscale
	}
	tx, err := p.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO users(id,username,name,tailscale,groups_json,enabled,password) VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET username=excluded.username,name=excluded.name,tailscale=excluded.tailscale,groups_json=excluded.groups_json,enabled=excluded.enabled,password=excluded.password,auth_version=users.auth_version+?`, u.ID, u.Username, u.Name, ts, string(groups), u.Enabled, u.password, authVersionChange)
	if err != nil {
		return errors.New("could not save user; check for duplicate local or Tailscale logins")
	}
	if exists && previous.Tailscale != u.Tailscale {
		if _, err = tx.Exec(`UPDATE user_photos SET tailscale_url='',revision=revision+1 WHERE user_id=? AND tailscale_url!=''`, u.ID); err != nil {
			return err
		}
	}
	if revokeSessions {
		if _, err = tx.Exec(`DELETE FROM sessions WHERE user_id=?`, u.ID); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`UPDATE settings SET revision=revision+1 WHERE id=1`); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO audit(at,actor,action) VALUES(?,?,?)`, time.Now().Unix(), actor, "user saved: "+u.ID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return p.reload()
}

// Removing a user and their direct grants is one revisioned transaction. Keep
// empty audiences restricted: an empty notice audience would otherwise mean everyone.
func (p *persistentState) removeUser(id string, revision int64, actor string) error {
	return p.removeUserChecked(id, revision, actor, nil)
}

func (p *persistentState) removeUserChecked(id string, revision int64, actor string, check authorizationCheck) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if check != nil {
		if err := check(); err != nil {
			return err
		}
	}
	users, err := p.users()
	if err != nil {
		return err
	}
	exists, usableAdmin, authorized := false, false, false
	for _, user := range users {
		if user.ID == actor && user.Enabled && groupAdmin(user.Groups) {
			authorized = true
		}
		if user.ID == id {
			exists = true
		} else if user.Enabled && groupAdmin(user.Groups) && user.password != "" {
			usableAdmin = true
		}
	}
	if !authorized {
		return errors.New("administrator account changed; reload before removing a user")
	}
	if !exists {
		return errors.New("user no longer exists; reload before removing another user")
	}
	if !usableAdmin {
		return errors.New("keep at least one enabled administrator with a local password")
	}
	var raw string
	if err = p.db.QueryRow(`SELECT document FROM settings WHERE id=1`).Scan(&raw); err != nil {
		return err
	}
	cfg, err := parseConfig([]byte(raw))
	if err != nil {
		return err
	}
	removeGrant := func(access []string) []string {
		kept := make([]string, 0, len(access))
		removed := false
		for _, rule := range access {
			if strings.EqualFold(strings.TrimSpace(rule), id) {
				removed = true
			} else {
				kept = append(kept, rule)
			}
		}
		if !removed {
			return access
		}
		if len(kept) == 0 {
			return []string{"admin"}
		}
		return kept
	}
	for i := range cfg.Apps {
		cfg.Apps[i].Access = removeGrant(cfg.Apps[i].Access)
	}
	if cfg.Notice != nil {
		cfg.Notice.Access = removeGrant(cfg.Notice.Access)
	}
	document, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	tx, err := p.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE settings SET document=?,revision=revision+1 WHERE id=1 AND revision=?`, string(document), revision)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errConflict
	}
	// Foreign keys cascade the deletion to every local session for this user.
	if _, err = tx.Exec(`DELETE FROM users WHERE id=?`, id); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO audit(at,actor,action) VALUES(?,?,?)`, time.Now().Unix(), actor, "user removed: "+id); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return p.reload()
}

var secretNames = []string{"ntfy", "seerr", "plex", "sonarr", "radarr", "abs", "webhook"}

func (p *persistentState) saveSecrets(values map[string]string, revision int64, actor string) error {
	return p.saveSecretsChecked(values, revision, actor, nil)
}

func (p *persistentState) saveSecretsChecked(values map[string]string, revision int64, actor string, check authorizationCheck) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if check != nil {
		if err := check(); err != nil {
			return err
		}
	}
	if endpoint, supplied := values["webhook"]; supplied {
		cfg, _ := p.store.Get()
		if cfg.Webhook != nil {
			if _, err := validWebhookURL(endpoint, cfg.Webhook.Format); err != nil {
				return err
			}
		}
	}
	tx, err := p.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE settings SET revision=revision+1 WHERE id=1 AND revision=?`, revision)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return errConflict
	}
	if err := writeSecrets(tx, values); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO audit(at,actor,action) VALUES(?,?,?)`, time.Now().Unix(), actor, "integration keys updated"); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return p.reload()
}

func writeSecrets(tx *sql.Tx, values map[string]string) error {
	for k, v := range values {
		if !validSecretName(k) || len(v) > 8192 {
			return errors.New("invalid integration key")
		}
		if k == "webhook" && strings.TrimSpace(v) != "" {
			if _, err := validWebhookURL(v, "json"); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`INSERT INTO secrets(name,value) VALUES(?,?) ON CONFLICT(name) DO UPDATE SET value=excluded.value`, k, strings.TrimSpace(v)); err != nil {
			return err
		}
	}
	return nil
}
