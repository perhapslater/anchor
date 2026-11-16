package main

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var errTailscaleAddress = errors.New("Enter the full HTTPS Tailscale Serve address, such as https://anchor.example.ts.net:8443, without a path or sign-in details.")

type tailscaleTestReport struct {
	State     string       `json:"state"`
	Title     string       `json:"title"`
	Detail    string       `json:"detail"`
	Identity  string       `json:"identity,omitempty"`
	Name      string       `json:"name,omitempty"`
	Groups    []string     `json:"groups,omitempty"`
	Sections  []apiSection `json:"sections,omitempty"`
	CheckedAt time.Time    `json:"checked_at"`
	Address   string       `json:"address,omitempty"`
}

type tailscaleTestRun struct {
	owner, address string
	revision       int64
	expires        time.Time
	report         *tailscaleTestReport
}

type tailscaleTestRuns struct {
	sync.Mutex
	runs map[string]*tailscaleTestRun
}

func tailscaleServeOrigin(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" || u.User != nil || !strings.HasSuffix(strings.ToLower(u.Hostname()), ".ts.net") ||
		u.Opaque != "" || strings.HasSuffix(u.Host, ":") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errTailscaleAddress
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return nil, errTailscaleAddress
		}
	}
	u.Path, u.RawPath = "", ""
	return u, nil
}

// Only the administrator can start a run or read its result. The short-lived
// token lets the test tab submit its own identity without sharing cookies,
// opening CORS, granting access, or sending identity data to a browser opener.
func (h *handler) startTailscaleTest(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	var body struct{ Address string }
	if !readJSON(w, r, &body) {
		return
	}
	if !h.requireAdmin(w, r) {
		return
	}
	u, err := tailscaleServeOrigin(body.Address)
	if err != nil {
		jsonReply(w, 400, errTailscaleAddress.Error())
		return
	}
	revision, err := h.store.persistent.revision()
	if err != nil {
		jsonReply(w, 500, "Could not read saved settings.")
		return
	}
	tests := h.tailscaleTests
	tests.Lock()
	defer tests.Unlock()
	for key, run := range tests.runs {
		if time.Now().After(run.expires) {
			delete(tests.runs, key)
		}
	}
	if len(tests.runs) >= 64 {
		jsonReply(w, 429, "Too many sign-in tests. Wait a few minutes and try again.")
		return
	}
	token := randomToken()
	tests.runs[token] = &tailscaleTestRun{owner: authFrom(r).user.ID, address: u.String(), revision: revision, expires: time.Now().Add(5 * time.Minute)}
	writeJSON(w, map[string]string{"ticket": token, "url": u.String() + "/tailscale-test.html#ticket=" + token})
}

func (h *handler) tailscaleTestResult(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	revision, err := h.store.persistent.revision()
	if err != nil {
		jsonReply(w, 500, "Could not read saved settings.")
		return
	}
	tests := h.tailscaleTests
	tests.Lock()
	defer tests.Unlock()
	run := tests.runs[r.URL.Query().Get("ticket")]
	if run == nil || run.owner != authFrom(r).user.ID || time.Now().After(run.expires) {
		jsonReply(w, 410, "This sign-in test has expired. Start a new test.")
		return
	}
	if run.revision != revision {
		writeJSON(w, map[string]any{"report": map[string]string{"state": "stale", "title": "Settings changed", "detail": "Run the test again to check the latest accounts, groups and applications."}})
		return
	}
	writeJSON(w, map[string]any{"report": run.report})
}

func (h *handler) tailscaleTest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	out := tailscaleTestReport{State: "missing", Title: "Tailscale sign-in wasn't detected", Detail: "Connect this device to Tailscale, then open Anchor using its full HTTPS Tailscale Serve address. If you are already connected, check Step 1: Connect Anchor to Tailscale.", CheckedAt: time.Now()}
	if h.auth == nil || h.auth.mode != "tailscale" {
		out.State, out.Title, out.Detail = "mode", "Automatic Tailscale sign-in is not enabled", "Complete Step 1: Connect Anchor to Tailscale, including Anchor's deployment mode, then test its HTTPS Serve address."
	} else if w.Header().Get("X-Anchor-Dev-Identities") == "1" {
		out.State, out.Title, out.Detail = "simulated", "Development simulation", "Use a real Tailscale-connected device and HTTPS Serve address to verify automatic sign-in."
	} else if identity := h.auth.observedTailscaleLogin(r); identity != "" {
		// This endpoint reveals only the requesting device's trusted identity;
		// unlinked/paused people never receive other account or application data.
		out.Identity = identity
		who := authFrom(r)
		if who.method != "tailscale" || !who.user.Enabled {
			out.State, out.Title, out.Detail = "unlinked", "Tailscale identity received; account needs attention", "This email is not linked to an enabled Anchor account. Ask your administrator to check Step 2: Link people to their Anchor accounts."
		} else {
			cfg, cfgErr := h.store.Get()
			if cfgErr != "" {
				jsonReply(w, 503, "Anchor could not load its application settings. Check Administration and try again.")
				return
			}
			out.State, out.Title, out.Detail = "verified", "Automatic sign-in verified", "Anchor recognized this account through Tailscale. These are the applications available to this account in Anchor."
			out.Name, out.Groups = who.user.Name, who.user.Groups
			out.Sections = h.sectionsFor(cfg, who.user.ID, false, false)
		}
	}
	if token := r.URL.Query().Get("ticket"); token != "" {
		tests := h.tailscaleTests
		tests.Lock()
		run := tests.runs[token]
		if run == nil || time.Now().After(run.expires) {
			tests.Unlock()
			jsonReply(w, 410, "This sign-in test has expired. Return to setup and start a new test.")
			return
		}
		target, _ := url.Parse(run.address)
		if originAuthority(target) != originAuthority(&url.URL{Scheme: "https", Host: r.Host}) {
			tests.Unlock()
			jsonReply(w, 400, "Open this test using the HTTPS Tailscale address entered in setup.")
			return
		}
		out.Address = run.address
		if run.report == nil {
			run.report = &out
		}
		tests.Unlock()
	}
	writeJSON(w, out)
}
