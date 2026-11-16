package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

var localLoginPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9@._-]{0,119}$`)
var groupPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

const sessionCookie = "anchor_session"

type authContext struct {
	user               localUser
	csrf, hash, method string
	picture            string // Effective uploaded or remembered photo for this account.
}
type authContextKey struct{}
type authentication struct {
	state           *persistentState
	mode, publicURL string
	csrfKey         string
	loginLimits     *rateLimiter
	slots           chan struct{}
	photoLimits     *bucketLimiter
	photoReads      *bucketLimiter
	photoSlots      chan struct{}
	setupMu         sync.Mutex
}

func hashPassword(password string) (string, error) {
	if len(password) == 0 || len(password) > 1024 {
		return "", errors.New("password must not be empty or exceed 1024 bytes")
	}
	salt := randomToken()[:32]
	hash := argon2.IDKey([]byte(password), []byte(salt), 3, 64*1024, 2, 32)
	return "argon2id$" + salt + "$" + base64.RawStdEncoding.EncodeToString(hash), nil
}
func checkPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	// Do the same expensive work for an unknown user.
	salt := "00000000000000000000000000000000"
	expected := make([]byte, 32)
	valid := len(parts) == 3 && parts[0] == "argon2id" && len(parts[1]) == 32
	if valid {
		salt = parts[1]
		b, err := base64.RawStdEncoding.DecodeString(parts[2])
		valid = err == nil && len(b) == 32
		if valid {
			expected = b
		}
	}
	actual := argon2.IDKey([]byte(password), []byte(salt), 3, 64*1024, 2, 32)
	return subtle.ConstantTimeCompare(actual, expected) == 1 && valid
}
func newAuthentication(state *persistentState) (*authentication, error) {
	mode := env("ANCHOR_MODE", "tailscale")
	if mode != "tailscale" && mode != "proxy" && mode != "lan" {
		return nil, errors.New("ANCHOR_MODE must be tailscale, proxy or lan")
	}
	if mode == "lan" && os.Getenv("ANCHOR_ALLOW_HTTP") != "1" {
		return nil, errors.New("HTTP LAN mode requires ANCHOR_ALLOW_HTTP=1")
	}
	publicURL := strings.TrimRight(os.Getenv("ANCHOR_PUBLIC_URL"), "/")
	if mode != "tailscale" && publicURL == "" {
		return nil, errors.New("set ANCHOR_PUBLIC_URL to the address used by browsers")
	}
	if publicURL != "" {
		u, err := url.Parse(publicURL)
		scheme := "https"
		if mode == "lan" {
			scheme = "http"
		}
		if err != nil || u.Scheme != scheme || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("ANCHOR_PUBLIC_URL must be an origin with the mode's scheme and no path, credentials or query")
		}
	}
	return &authentication{state: state, mode: mode, publicURL: publicURL, csrfKey: randomToken(), loginLimits: newRateLimiter(time.Second, 120), slots: make(chan struct{}, 2)}, nil
}
func (a *authentication) csrfFor(id string) string {
	m := hmac.New(sha256.New, []byte(a.csrfKey))
	m.Write([]byte(id))
	return hex.EncodeToString(m.Sum(nil))
}
func (a *authentication) identify(r *http.Request) authContext {
	if a.mode == "tailscale" {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			login := strings.ToLower(strings.TrimSpace(decodeHeader(r.Header.Get("Tailscale-User-Login"))))
			if login != "" {
				users, err := a.state.users()
				if err == nil {
					for _, u := range users {
						if u.Enabled && u.Tailscale == login {
							picture := strings.TrimSpace(r.Header.Get("Tailscale-User-Profile-Pic"))
							if parsed, err := url.Parse(picture); err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || len(picture) > 2048 {
								picture = ""
							}
							if picture != "" && picture != u.tailscalePicture {
								if updated, err := a.state.rememberTailscalePhoto(u, picture); err == nil {
									u = updated
								}
							}
							return authContext{user: u, csrf: a.csrfFor(u.ID), method: "tailscale", picture: u.Picture}
						}
					}
				}
			}
		}
	}
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || len(cookie.Value) != 64 {
		return authContext{}
	}
	hash := tokenHash(cookie.Value)
	var id, csrf string
	var expires int64
	err = a.state.db.QueryRow(`SELECT user_id,csrf,expires FROM sessions WHERE hash=?`, hash).Scan(&id, &csrf, &expires)
	if err != nil || expires <= time.Now().Unix() {
		return authContext{}
	}
	u, ok := a.state.user(id)
	if !ok || !u.Enabled {
		return authContext{}
	}
	return authContext{user: u, csrf: csrf, hash: hash, method: "local", picture: u.Picture}
}
func (a *authentication) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.publicURL != "" && r.URL.Path != "/healthz" {
			u, _ := url.Parse(a.publicURL)
			// Browsers omit an origin's default port from Host. Compare the
			// effective authority, as sameOrigin does, so :80/:443 remain usable.
			if strings.HasSuffix(r.Host, ":") || !strings.EqualFold(originAuthority(&url.URL{Scheme: u.Scheme, Host: r.Host}), originAuthority(u)) {
				jsonReply(w, 421, "Use Anchor's configured public address.")
				return
			}
		}
		who := a.identify(r)
		if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" {
			if !sameOrigin(r) || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				jsonReply(w, 403, "Changes must be sent as JSON from Anchor itself.")
				return
			}
			if r.URL.Path != "/api/auth/login" && r.URL.Path != "/api/auth/setup" {
				supplied := r.Header.Get("X-CSRF-Token")
				if who.user.ID == "" || who.csrf == "" || subtle.ConstantTimeCompare([]byte(supplied), []byte(who.csrf)) != 1 {
					jsonReply(w, 403, "Your session changed. Reload and try again.")
					return
				}
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authContextKey{}, who)))
	})
}

// Console-only account maintenance; never an HTTP sign-in shortcut.
func linkTailscaleUser(dir, username, login string) error {
	store, err := newPersistentStore(dir)
	if err != nil {
		return err
	}
	defer store.persistent.db.Close()
	users, err := store.persistent.users()
	if err != nil {
		return err
	}
	username = strings.ToLower(strings.TrimSpace(username))
	login = strings.ToLower(strings.TrimSpace(login))
	if login == "" || !strings.Contains(login, "@") {
		return errors.New("provide the exact Tailscale sign-in email")
	}
	for _, user := range users {
		if user.Username != username {
			continue
		}
		if user.Tailscale != login {
			user.Tailscale = login
			if err := store.persistent.saveUser(user, "", "server console"); err != nil {
				return err
			}
		}
		fmt.Printf("Linked Tailscale sign-in for local account %s.\n", username)
		return nil
	}
	return errors.New("local account not found")
}

func authFrom(r *http.Request) authContext {
	a, _ := r.Context().Value(authContextKey{}).(authContext)
	return a
}

var errAuthorization = errors.New("your authorization changed; sign in again")

// authorizationCheck runs while the persistence mutex is held, immediately
// before a protected read or mutation. Trusted console/setup callers omit it.
type authorizationCheck func() error

func (p *persistentState) authorizationGuard(who authContext, admin bool) authorizationCheck {
	return func() error { return p.checkActorLocked(who, admin) }
}

func (p *persistentState) checkActorLocked(who authContext, admin bool) error {
	current, ok := p.user(who.user.ID)
	if !ok || !current.Enabled || current.authVersion != who.user.authVersion || admin && !groupAdmin(current.Groups) {
		return errAuthorization
	}
	switch who.method {
	case "local":
		var count int
		if who.hash == "" || p.db.QueryRow(`SELECT count(*) FROM sessions WHERE hash=? AND user_id=? AND expires>?`, who.hash, current.ID, time.Now().Unix()).Scan(&count) != nil || count != 1 {
			return errAuthorization
		}
	case "tailscale":
		if current.Tailscale == "" || current.Tailscale != who.user.Tailscale {
			return errAuthorization
		}
	default:
		return errAuthorization
	}
	return nil
}
func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(value)
}
func readJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		jsonReply(w, 400, "Invalid JSON request.")
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		jsonReply(w, 400, "Expected one JSON document.")
		return false
	}
	return true
}
func (a *authentication) routes(mux *http.ServeMux) {
	a.photoLimits = newBucketLimiter(5, 3*time.Minute)
	a.photoReads = newBucketLimiter(100, 500*time.Millisecond)
	a.photoSlots = make(chan struct{}, 2)
	mux.HandleFunc("GET /api/people/{id}/photo", a.profilePhoto)
	mux.HandleFunc("POST /api/auth/photo", a.changeProfilePhoto)
	mux.HandleFunc("DELETE /api/auth/photo", a.changeProfilePhoto)
	mux.HandleFunc("POST /api/admin/users/{id}/photo", a.changeProfilePhoto)
	mux.HandleFunc("DELETE /api/admin/users/{id}/photo", a.changeProfilePhoto)
	mux.HandleFunc("GET /api/auth/status", a.status)
	mux.HandleFunc("POST /api/auth/setup", a.setup)
	mux.HandleFunc("POST /api/auth/login", a.login)
	mux.HandleFunc("POST /api/auth/logout", a.logout)
	mux.HandleFunc("POST /api/auth/password", a.password)
	mux.HandleFunc("POST /api/auth/account", a.account)
}
func (a *authentication) status(w http.ResponseWriter, r *http.Request) {
	who := authFrom(r)
	writeJSON(w, map[string]any{"setup_required": a.state.needsSetup(), "authenticated": who.user.ID != "", "user": who.user, "csrf": who.csrf, "method": who.method, "admin": who.user.Enabled && groupAdmin(who.user.Groups), "mode": a.mode, "insecure": a.mode == "lan"})
}
func (a *authentication) issue(w http.ResponseWriter, u localUser) error {
	token, csrf := randomToken(), randomToken()
	expires := time.Now().Add(12 * time.Hour)
	tx, err := a.state.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var enabled bool
	var authVersion int
	if err = tx.QueryRow(`SELECT enabled,auth_version FROM users WHERE id=?`, u.ID).Scan(&enabled, &authVersion); err != nil {
		return err
	}
	if !enabled || authVersion != u.authVersion {
		return errors.New("account changed during sign-in; try again")
	}
	if _, err = tx.Exec(`DELETE FROM sessions WHERE expires<=? OR hash IN (SELECT hash FROM sessions WHERE user_id=? ORDER BY expires DESC LIMIT -1 OFFSET 9)`, time.Now().Unix(), u.ID); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO sessions VALUES(?,?,?,?)`, tokenHash(token), u.ID, csrf, expires.Unix()); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: a.mode != "lan", SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: 12 * 60 * 60})
	return nil
}
func (a *authentication) setup(w http.ResponseWriter, r *http.Request) {
	var body struct{ Token, Username, Name, Password string }
	if !readJSON(w, r, &body) {
		return
	}
	a.setupMu.Lock()
	defer a.setupMu.Unlock()
	if !a.state.needsSetup() {
		jsonReply(w, 409, "Setup is already complete.")
		return
	}
	token, err := os.ReadFile(filepath.Join(a.state.dir, "setup-token"))
	expected := strings.TrimSpace(string(token))
	if err != nil || !validSetupToken(expected) || subtle.ConstantTimeCompare([]byte(body.Token), []byte(expected)) != 1 {
		jsonReply(w, 403, "The setup token is incorrect.")
		return
	}
	u := localUser{ID: "u_" + randomToken()[:20], Username: body.Username, Name: body.Name, Groups: []string{"admin"}, Enabled: true}
	if err = a.state.saveUser(u, body.Password, "setup"); err != nil {
		jsonReply(w, 400, err.Error())
		return
	}
	os.Remove(filepath.Join(a.state.dir, "setup-token"))
	u, _ = a.state.user(u.ID)
	if err = a.issue(w, u); err != nil {
		jsonReply(w, 500, "Administrator created. Sign in to continue.")
		return
	}
	jsonReply(w, 200, "Administrator created.")
}
func (a *authentication) login(w http.ResponseWriter, r *http.Request) {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if !a.loginLimits.allow(host) {
		jsonReply(w, 429, "Too many sign-in attempts. Please wait before trying again.")
		return
	}
	select {
	case a.slots <- struct{}{}:
		defer func() { <-a.slots }()
	default:
		jsonReply(w, 429, "Sign-in is busy. Please try again.")
		return
	}
	var body struct{ Username, Password string }
	if !readJSON(w, r, &body) {
		return
	}
	if len(body.Password) > 1024 {
		jsonReply(w, 401, "Incorrect login or password.")
		return
	}
	users, err := a.state.users()
	if err != nil {
		jsonReply(w, 503, "Sign-in is unavailable.")
		return
	}
	var found localUser
	for _, u := range users {
		if u.Username == strings.ToLower(strings.TrimSpace(body.Username)) {
			found = u
		}
	}
	valid := checkPassword(found.password, body.Password)
	if !valid || !found.Enabled || found.ID == "" {
		jsonReply(w, 401, "Incorrect login or password.")
		return
	}
	if err = a.issue(w, found); err != nil {
		jsonReply(w, 500, "Could not create a session.")
		return
	}
	jsonReply(w, 200, "Signed in.")
}
func (a *authentication) logout(w http.ResponseWriter, r *http.Request) {
	who := authFrom(r)
	if who.hash != "" {
		a.state.mu.Lock()
		defer a.state.mu.Unlock()
		if _, err := a.state.db.Exec(`DELETE FROM sessions WHERE hash=?`, who.hash); err != nil {
			jsonReply(w, 500, "Could not end the session.")
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: a.mode != "lan", SameSite: http.SameSiteStrictMode, MaxAge: -1})
	jsonReply(w, 200, "Local session ended. Tailscale identity remains active while connected.")
}
func (a *authentication) password(w http.ResponseWriter, r *http.Request) {
	a.updateLocalCredentials(w, r, true)
}
func (a *authentication) account(w http.ResponseWriter, r *http.Request) {
	a.updateLocalCredentials(w, r, false)
}
func (a *authentication) updateLocalCredentials(w http.ResponseWriter, r *http.Request, passwordRequired bool) {
	select {
	case a.slots <- struct{}{}:
		defer func() { <-a.slots }()
	default:
		jsonReply(w, 429, "Password verification is busy. Please try again.")
		return
	}
	who := authFrom(r)
	if who.user.ID == "" {
		jsonReply(w, 401, "Sign in first.")
		return
	}
	var body struct {
		Current, Password string
		Username          *string
	}
	if !readJSON(w, r, &body) {
		return
	}
	if passwordRequired && len(body.Password) == 0 || len(body.Password) > 1024 {
		jsonReply(w, 400, "Password must not be empty or exceed 1024 bytes.")
		return
	}
	revision, err := a.state.revision()
	if err != nil {
		jsonReply(w, 503, "Account settings are unavailable.")
		return
	}
	current, ok := a.state.user(who.user.ID)
	if !ok || !current.Enabled {
		jsonReply(w, 401, "Sign in again.")
		return
	}
	if current.authVersion != who.user.authVersion {
		jsonReply(w, 409, "Account settings changed. Reload and try again.")
		return
	}
	// A trusted Tailscale identity authorizes changes to this account only.
	// It must not bypass an administrator's decision to disable local sign-in.
	if current.password == "" {
		jsonReply(w, 403, "Local password sign-in is disabled. Ask your administrator to enable it.")
		return
	}
	if who.method != "tailscale" && !checkPassword(current.password, body.Current) {
		jsonReply(w, 403, "Current local password is incorrect.")
		return
	}
	changed := body.Password != ""
	if body.Username != nil {
		username := strings.ToLower(strings.TrimSpace(*body.Username))
		if !localLoginPattern.MatchString(username) {
			jsonReply(w, 400, "Local login must use letters, digits, dots, @, underscores or hyphens.")
			return
		}
		changed = changed || username != current.Username
		current.Username = username
	}
	if err := a.state.saveUserSignInChecked(current, body.Password, false, who.user.ID, a.state.authorizationGuard(who, false), revision); err != nil {
		if errors.Is(err, errAuthorization) {
			jsonReply(w, 403, err.Error())
			return
		}
		jsonReply(w, 400, err.Error())
		return
	}
	if who.method == "tailscale" {
		writeJSON(w, map[string]any{"message": "Local sign-in details saved. Your Tailscale sign-in stays active.", "reauthenticate": false})
	} else {
		writeJSON(w, map[string]any{"message": "Local sign-in details saved.", "reauthenticate": changed})
	}
}
func recoverAdministrator(dir, username string) error {
	store, err := newPersistentStore(dir)
	if err != nil {
		return err
	}
	defer store.persistent.db.Close()
	users, err := store.persistent.users()
	if err != nil {
		return err
	}
	for _, u := range users {
		if u.Username == strings.ToLower(strings.TrimSpace(username)) && u.Enabled && groupAdmin(u.Groups) {
			password := randomToken()[:24]
			if err = store.persistent.saveUser(u, password, "server recovery"); err != nil {
				return err
			}
			fmt.Printf("Temporary password for %s: %s\nSign in locally and change it immediately.\n", username, password)
			return nil
		}
	}
	return errors.New("enabled local administrator not found")
}
