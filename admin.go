package main

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type dashboardDocument struct {
	SchemaVersion int    `yaml:"schema_version"`
	Dashboard     Config `yaml:"dashboard"`
}

func (h *handler) adminRoutes(mux *http.ServeMux) {
	// Initialized before serving requests; tests have a separate, short throttle.
	h.notificationTests = newRateLimiter(5*time.Second, 120)
	h.tailscaleTests = &tailscaleTestRuns{runs: make(map[string]*tailscaleTestRun)}
	mux.HandleFunc("GET /api/tailscale-test", h.tailscaleTest)
	mux.HandleFunc("POST /api/admin/tailscale-test", h.startTailscaleTest)
	mux.HandleFunc("GET /api/admin/tailscale-test", h.tailscaleTestResult)
	mux.HandleFunc("POST /api/admin/setup", h.setupProgress)
	mux.HandleFunc("POST /api/admin/notifications/test", h.testNotification)
	mux.HandleFunc("GET /api/admin/state", h.adminState)
	mux.HandleFunc("GET /api/admin/icons/{name}", h.adminIcon)
	mux.HandleFunc("POST /api/admin/config", h.adminConfig)
	mux.HandleFunc("POST /api/admin/users", h.adminUser)
	mux.HandleFunc("DELETE /api/admin/users/{id}", h.adminRemoveUser)
	mux.HandleFunc("POST /api/admin/tailscale", h.adminTailscaleIdentity)
	mux.HandleFunc("POST /api/admin/secrets", h.adminSecrets)
	mux.HandleFunc("GET /api/admin/export", h.adminExport)
	mux.HandleFunc("POST /api/admin/backup", h.adminBackup)
	mux.HandleFunc("POST /api/admin/import/preview", h.adminImport)
}
func (h *handler) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if h.auth == nil {
		jsonReply(w, 403, "Administrator access is required.")
		return false
	}
	p := h.store.persistent
	p.mu.Lock()
	defer p.mu.Unlock()
	return h.requireAdminLocked(w, r)
}

func (h *handler) requireAdminLocked(w http.ResponseWriter, r *http.Request) bool {
	if err := h.store.persistent.checkActorLocked(authFrom(r), true); err != nil {
		jsonReply(w, 403, "Administrator access is required.")
		return false
	}
	return true
}
func (h *handler) adminState(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	p := h.store.persistent
	p.mu.Lock()
	defer p.mu.Unlock()
	if !h.requireAdminLocked(w, r) {
		return
	}
	cfg, _ := h.store.Get()
	copyCfg := *cfg
	copyCfg.People = nil
	users, err := p.users()
	if err != nil {
		jsonReply(w, 500, "Could not read users.")
		return
	}
	rev, err := p.revision()
	if err != nil {
		jsonReply(w, 500, "Could not read settings.")
		return
	}
	keys := map[string]bool{}
	for _, k := range secretNames {
		keys[k] = cfg.credentials[k] != ""
	}
	for name, value := range cfg.credentials {
		if validSecretName(name) {
			keys[name] = value != ""
		}
	}
	setup, err := p.onboarding()
	if err != nil {
		jsonReply(w, 500, "Could not read setup progress.")
		return
	}
	var delivery any = map[string]notificationDeliveryStatus{}
	if router, ok := h.notifier.(*notificationRouter); ok {
		delivery = router.statuses()
	}
	identity := ""
	if h.auth != nil {
		identity = h.auth.observedTailscaleLogin(r)
	}
	writeJSON(w, map[string]any{"revision": rev, "config": copyCfg, "users": users, "keys": keys, "icons": bundledServices, "setup": setup, "delivery": delivery, "tailscale_identity": identity})
}
func (h *handler) saved(w http.ResponseWriter, err error) {
	if errors.Is(err, errAuthorization) {
		jsonReply(w, 403, err.Error())
		return
	}
	if errors.Is(err, errConflict) {
		jsonReply(w, 409, err.Error())
		return
	}
	if err != nil {
		jsonReply(w, 400, err.Error())
		return
	}
	h.checker.Kick()
	h.checker.changes.Notify()
	jsonReply(w, 200, "Saved.")
}
func (h *handler) adminConfig(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	var body struct {
		Revision int64             `json:"revision"`
		Config   Config            `json:"config"`
		Keys     map[string]string `json:"keys,omitempty"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	p := h.store.persistent
	h.saved(w, p.saveConfigAndSecretsChecked(&body.Config, body.Revision, authFrom(r).user.ID, body.Keys, p.authorizationGuard(authFrom(r), true)))
}
func (h *handler) adminUser(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	var body struct {
		User         localUser `json:"user"`
		Password     string    `json:"password"`
		Revision     int64     `json:"revision"`
		DisableLocal bool      `json:"disable_local,omitempty"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	p := h.store.persistent
	h.saved(w, p.saveUserSignInChecked(body.User, body.Password, body.DisableLocal, authFrom(r).user.ID, p.authorizationGuard(authFrom(r), true), body.Revision))
}
func (h *handler) adminRemoveUser(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	var body struct {
		Revision int64 `json:"revision"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	id := r.PathValue("id")
	p := h.store.persistent
	err := p.removeUserChecked(id, body.Revision, authFrom(r).user.ID, p.authorizationGuard(authFrom(r), true))
	if err == nil && id == authFrom(r).user.ID {
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: h.auth.mode != "lan", SameSite: http.SameSiteStrictMode, MaxAge: -1})
	}
	h.saved(w, err)
}
func (h *handler) adminSecrets(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	var body struct {
		Revision int64             `json:"revision"`
		Values   map[string]string `json:"values"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	p := h.store.persistent
	h.saved(w, p.saveSecretsChecked(body.Values, body.Revision, authFrom(r).user.ID, p.authorizationGuard(authFrom(r), true)))
}
func (h *handler) adminExport(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	p := h.store.persistent
	p.mu.Lock()
	defer p.mu.Unlock()
	if !h.requireAdminLocked(w, r) {
		return
	}
	cfg, _ := h.store.Get()
	copyCfg := *cfg
	copyCfg.People = nil
	raw, err := yaml.Marshal(dashboardDocument{SchemaVersion: 1, Dashboard: copyCfg})
	if err != nil {
		jsonReply(w, 500, "Could not export settings.")
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Disposition", `attachment; filename="anchor-dashboard.yaml"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(raw)
}
func (h *handler) adminImport(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	var body struct {
		YAML string `json:"yaml"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	var doc dashboardDocument
	if !h.requireAdmin(w, r) {
		return
	}
	decoder := yaml.NewDecoder(strings.NewReader(body.YAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		jsonReply(w, 400, "Invalid dashboard YAML: "+err.Error())
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF || doc.SchemaVersion != 1 {
		jsonReply(w, 400, "Expected one dashboard document with schema_version: 1.")
		return
	}
	if len(doc.Dashboard.People) > 0 {
		jsonReply(w, 400, "Manage accounts in Users. Dashboard imports cannot create or replace accounts.")
		return
	}
	raw, err := yaml.Marshal(doc.Dashboard)
	if err != nil {
		jsonReply(w, 400, "Could not read dashboard settings.")
		return
	}
	cfg, err := parseConfig(raw)
	if err != nil {
		jsonReply(w, 400, err.Error())
		return
	}
	if err = h.store.persistent.validateUserGrants(cfg); err != nil {
		jsonReply(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"config": cfg, "summary": map[string]any{"applications": len(cfg.Apps), "sections": len(cfg.Sections), "title": cfg.Title}, "message": "Preview only. Apply replaces dashboard settings. Accounts and credentials for retained connections are preserved. Removing a connection also removes its scoped API credentials."})
}

func (h *handler) adminBackup(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	p := h.store.persistent
	path := filepath.Join(p.dir, "backup-"+randomToken()+".db")
	defer os.Remove(path)
	file, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		jsonReply(w, 500, "Could not create protected backup file.")
		return
	}
	file.Close()
	p.mu.Lock()
	if !h.requireAdminLocked(w, r) {
		p.mu.Unlock()
		return
	}
	_, err := p.db.Exec(`VACUUM INTO ?`, path)
	p.mu.Unlock()
	if err != nil {
		jsonReply(w, 500, "Could not create a consistent database backup.")
		return
	}
	if err = os.Chmod(path, 0600); err != nil {
		jsonReply(w, 500, "Could not protect database backup.")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		jsonReply(w, 500, "Could not open database backup.")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("Content-Disposition", `attachment; filename="anchor-backup.db"`)
	w.Header().Set("Cache-Control", "no-store")
	io.Copy(w, f)
}

// Preview only the vetted bundled PNGs; configured custom icons still take
// precedence on dashboard tiles. The endpoint never downloads remote artwork.
func (h *handler) adminIcon(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	name := r.PathValue("name")
	known := false
	for _, icon := range bundledServices {
		if icon.Name == name {
			known = true
			break
		}
	}
	if !known {
		jsonReply(w, 404, "Icon not found.")
		return
	}
	data, err := webFiles.ReadFile("web/icons/" + name + ".png")
	if err != nil {
		jsonReply(w, 404, "Icon not found.")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	w.Write(data)
}
