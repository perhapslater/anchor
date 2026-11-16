package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// report handles the "Not working?" button: POST /api/report {"app": "<id>"}.
//
// Reports trigger notifications, so the endpoint is guarded:
//   - only enabled Anchor accounts, for apps they can see
//   - only from Anchor's own page (same-origin checks + JSON body, which a
//     cross-site form can't send without a CORS preflight Anchor never allows)
//   - one report per person per app every 10 minutes, 30 per hour in total
func (h *handler) report(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		jsonReply(w, http.StatusForbidden, "This button only works from Anchor itself.")
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		jsonReply(w, http.StatusUnsupportedMediaType, "Expected JSON.")
		return
	}
	cfg, _ := h.store.Get()
	v := h.identify(r)
	person, known := cfg.person(v.Login)
	if !known {
		jsonReply(w, http.StatusForbidden, "Anchor doesn't know who you are.")
		return
	}
	if !cfg.reportsEnabled() || h.notifier == nil {
		jsonReply(w, http.StatusNotFound, "Reporting isn't turned on.")
		return
	}

	var body struct {
		App string `json:"app"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		jsonReply(w, http.StatusBadRequest, "Bad request.")
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		jsonReply(w, http.StatusBadRequest, "Bad request.")
		return
	}
	// A body may arrive after logout, expiry, or an account/configuration edit.
	// Check the current session before producing an external side effect.
	if h.auth != nil {
		p := h.store.persistent
		p.mu.Lock()
		err := p.checkActorLocked(authFrom(r), false)
		cfg, _ = h.store.Get()
		p.mu.Unlock()
		if err != nil {
			jsonReply(w, http.StatusForbidden, "Your session changed. Sign in again.")
			return
		}
		person, known = cfg.person(v.Login)
		if !known || !cfg.reportsEnabled() {
			jsonReply(w, http.StatusNotFound, "Reporting isn't turned on.")
			return
		}
	}
	app := cfg.appByID(body.App)
	if app == nil || !cfg.visibleTo(app, v.Login) {
		// Same answer whether the app doesn't exist or isn't theirs.
		jsonReply(w, http.StatusNotFound, "That app isn't on your Anchor.")
		return
	}

	if !h.reports.allow(v.Login + "|" + app.ID) {
		jsonReply(w, http.StatusTooManyRequests,
			fmt.Sprintf("A report was attempted recently or Anchor's report limit was reached. If you need help, please text %s.", cfg.Contact))
		return
	}

	who := person.Name
	if who == "" {
		who = v.Login
	}
	st := h.checker.Get(app.ID)
	check := map[string]string{"up": "working", "down": "not responding", "unknown": "unknown"}[st.State]
	msg := Notification{
		Kind:     "report",
		Title:    fmt.Sprintf("%s says %s isn't working", who, app.Name),
		Message:  fmt.Sprintf("Reported from Anchor by %s (%s).\nAnchor's own check says: %s.", who, v.Login, check),
		Priority: 4,
		Tags:     []string{"raising_hand"},
		Click:    app.URL,
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	if err := h.notifier.Send(ctx, msg); err != nil {
		var partial *partialDeliveryError
		if errors.As(err, &partial) && partial.delivered > 0 {
			jsonReply(w, http.StatusOK, "Your report was delivered to at least one notification destination. An administrator can review the other destination's delivery status.")
			return
		}
		log.Printf("report from %s about %s could not be sent: %v", v.Login, app.ID, err)
		jsonReply(w, http.StatusBadGateway,
			fmt.Sprintf("Sorry, that didn't go through. Please text %s instead.", cfg.Contact))
		return
	}
	log.Printf("report: %s says %s isn't working", v.Login, app.ID)
	jsonReply(w, http.StatusOK, fmt.Sprintf("Thanks! Your report was sent to %s.", cfg.Contact))
}

// sameOrigin accepts only requests made by Anchor's own page.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" || len(r.Header.Values("Origin")) != 1 {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.User != nil || u.Opaque != "" || u.Host == "" ||
		u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(origin, "#") ||
		(u.Scheme != "https" && u.Scheme != "http") || strings.HasSuffix(u.Host, ":") {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if external, ok := r.Context().Value(externalSchemeKey{}).(string); ok {
		scheme = external
	}
	return u.Scheme == scheme && strings.EqualFold(originAuthority(u), originAuthority(&url.URL{Scheme: scheme, Host: r.Host}))
}

type externalSchemeKey struct{}

// Serve terminates HTTPS before forwarding to Anchor. The deployment mode,
// never visitor-supplied Forwarded headers, supplies the external scheme.
func externalScheme(scheme string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), externalSchemeKey{}, scheme)))
	})
}

func originAuthority(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return strings.ToLower(u.Hostname()) + ":" + port
}

func jsonReply(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": code == http.StatusOK, "message": message})
}
