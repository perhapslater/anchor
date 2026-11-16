//go:build anchordev

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

//go:embed devtools/identities.html devtools/identities.js devtools/identities.css
var identityTools embed.FS

func init() { developmentIdentities = newDevelopmentIdentities }

type developmentIdentity struct {
	auth                      *authentication
	next                      http.Handler
	authority, cookie, secret string
}

func newDevelopmentIdentities(next http.Handler, auth *authentication, listen string) (http.Handler, error) {
	host, port, err := net.SplitHostPort(listen)
	portNumber, portErr := strconv.Atoi(port)
	if err != nil || host != "127.0.0.1" || portErr != nil || portNumber < 1 || portNumber > 65535 || auth.mode != "tailscale" || auth.publicURL != "" {
		return nil, errors.New("identity simulation requires a single 127.0.0.1 listener, tailscale mode and no public URL; use dev.ps1 -SimulateTailscale")
	}
	if auth.state.needsSetup() {
		return nil, errors.New("finish setup with dev.ps1 before simulating existing Anchor people")
	}
	return &developmentIdentity{auth: auth, next: next, authority: listen, cookie: "anchor_dev_identity_" + port, secret: randomToken()}, nil
}

func (d *developmentIdentity) signed(id string) string {
	m := hmac.New(sha256.New, []byte(d.secret))
	m.Write([]byte(id))
	return id + "." + hex.EncodeToString(m.Sum(nil))
}

func (d *developmentIdentity) selected(r *http.Request) string {
	c, err := r.Cookie(d.cookie)
	if err != nil {
		return "signed-out"
	}
	id, _, ok := strings.Cut(c.Value, ".")
	if !ok || !hmac.Equal([]byte(c.Value), []byte(d.signed(id))) {
		return "signed-out"
	}
	return id
}

func (d *developmentIdentity) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() || r.Host != d.authority {
		jsonReply(w, 403, "Identity simulation is restricted to its exact localhost address.")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if strings.HasPrefix(r.URL.Path, "/__dev/") {
		d.tools(w, r)
		return
	}
	// A previous local administrator cookie must never mask a failed simulated
	// sign-in. Strip visitor-supplied identity headers before injecting our choice.
	r = r.Clone(r.Context())
	for name := range r.Header {
		if strings.HasPrefix(strings.ToLower(name), "tailscale-user-") {
			r.Header.Del(name)
		}
	}
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cookies {
		if c.Name != sessionCookie {
			r.AddCookie(c)
		}
	}
	selected := d.selected(r)
	if selected == "unlinked" {
		r.Header.Set("Tailscale-User-Login", "unlinked-"+randomToken()+"@example.invalid")
	} else if selected != "signed-out" {
		if user, ok := d.auth.state.user(selected); ok {
			// Disabled and unlinked users deliberately fail the normal auth check.
			r.Header.Set("Tailscale-User-Login", user.Tailscale)
		}
	}
	if r.URL.Path == "/api/auth/status" || r.URL.Path == "/api/tailscale-test" {
		w.Header().Set("X-Anchor-Dev-Identities", "1")
	}
	if r.URL.Path == "/api/auth/login" && r.Method == http.MethodPost {
		jsonReply(w, 409, "Use Change test user during identity simulation. Restart without -SimulateTailscale to test passwords.")
		return
	}
	d.next.ServeHTTP(w, r)
}

func (d *developmentIdentity) tools(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/__dev/identity-state":
		if r.Method != http.MethodGet {
			jsonReply(w, 405, "Use GET.")
			return
		}
		users, err := d.auth.state.users()
		if err != nil {
			jsonReply(w, 500, "Could not read Anchor people.")
			return
		}
		type person struct {
			ID        string   `json:"id"`
			Name      string   `json:"name"`
			Tailscale string   `json:"tailscale"`
			Groups    []string `json:"groups"`
			Enabled   bool     `json:"enabled"`
		}
		people := make([]person, 0, len(users))
		for _, user := range users {
			people = append(people, person{user.ID, user.Name, user.Tailscale, user.Groups, user.Enabled})
		}
		sort.Slice(people, func(i, j int) bool { return people[i].Name < people[j].Name })
		writeJSON(w, map[string]any{"people": people, "selected": d.selected(r), "csrf": d.secret})
	case "/__dev/identity":
		if r.Method != http.MethodPost || !sameOrigin(r) || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			jsonReply(w, 403, "Choose an identity from the local picker.")
			return
		}
		var body struct{ ID, CSRF string }
		if !readJSON(w, r, &body) {
			return
		}
		if !hmac.Equal([]byte(body.CSRF), []byte(d.secret)) {
			jsonReply(w, 403, "Reload the identity picker and try again.")
			return
		}
		if body.ID != "signed-out" && body.ID != "unlinked" {
			if _, ok := d.auth.state.user(body.ID); !ok {
				jsonReply(w, 400, "Choose an existing Anchor person.")
				return
			}
		}
		http.SetCookie(w, &http.Cookie{Name: d.cookie, Value: d.signed(body.ID), Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		jsonReply(w, 200, "Test identity selected.")
	default:
		assets := map[string]struct{ path, contentType string }{
			"/__dev/identities":     {"devtools/identities.html", "text/html; charset=utf-8"},
			"/__dev/identities.js":  {"devtools/identities.js", "text/javascript; charset=utf-8"},
			"/__dev/identities.css": {"devtools/identities.css", "text/css; charset=utf-8"},
		}
		asset, ok := assets[r.URL.Path]
		if !ok || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		data, err := identityTools.ReadFile(asset.path)
		if err != nil {
			http.Error(w, "Development asset missing.", 500)
			return
		}
		w.Header().Set("Content-Type", asset.contentType)
		fmt.Fprint(w, string(data))
	}
}
