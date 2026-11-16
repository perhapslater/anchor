package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"
)

// Home-screen shortcuts. A shortcut on a phone's home screen points at
// /open/<app>, a Anchor address, instead of at the app. Anchor checks who's
// asking and how the app is doing, then sends them on. That's what lets a
// shortcut keep the app's login link, follow saved URL changes, say
// "not working right now" instead of showing a browser error, and stop working
// for someone who has lost access.
//
//	/open/<app>          sends the person to the app (or says why it can't)
//	/open/<app>?add=1    the guide for adding the shortcut to the home screen

//go:embed templates/open.html
var openTemplateText string

var openTemplate = template.Must(template.New("open").Parse(openTemplateText))

// openPage is what the guide and the "not working" page show.
type openPage struct {
	Guide   bool   // true: how to add the shortcut; false: the app isn't working
	Name    string // the app's name, which phones use as the icon's label
	Icon    string // the home-screen icon, also shown as a preview
	Message string // why the app isn't working
}

// appLocked reports whether the app can't be opened right now: it's in
// maintenance, or Anchor's checks say it's down.
func appLocked(a *App, st Status) bool {
	return a.Maintenance != "" || st.State == "down"
}

// homeIconURL is the PNG a phone uses for this app's home-screen icon. Anchor
// doesn't fetch icons given as an https:// address, so those apps get
// Anchor's own icon.
func homeIconURL(a *App) string {
	if strings.HasPrefix(a.Icon, "http://") || strings.HasPrefix(a.Icon, "https://") {
		return "/brand/apple-touch-icon.png"
	}
	return "/icons/" + a.Icon + "/home.png"
}

func (h *handler) open(w http.ResponseWriter, r *http.Request) {
	cfg, _ := h.store.Get()
	v := h.identify(r)
	app := cfg.appByID(r.PathValue("id"))
	if app == nil || !cfg.visibleTo(app, v.Login) {
		// Same answer whether the app doesn't exist or isn't theirs.
		toHome(w, r)
		return
	}

	if r.URL.Query().Get("add") != "" {
		if !app.homeScreenOn() {
			toHome(w, r)
			return
		}
		renderOpen(w, openPage{Guide: true, Name: app.Name, Icon: homeIconURL(app)})
		return
	}

	// Locked apps show the same words as their tile. Admins can still open them.
	if appLocked(app, h.checker.Get(app.ID)) && !cfg.isAdmin(v.Login) {
		msg := app.Maintenance
		if msg == "" {
			msg = "Not working right now."
			if cfg.Notify.alertsOn() && app.alertsOn() {
				msg = fmt.Sprintf("Not working right now. Alerts to %s are enabled.", cfg.Contact)
			}
		}
		renderOpen(w, openPage{Name: app.Name, Icon: homeIconURL(app), Message: msg})
		return
	}

	// Only ever the saved application address, never anything from the request.
	if u, err := url.Parse(app.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		toHome(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, app.URL, http.StatusFound)
}

func renderOpen(w http.ResponseWriter, p openPage) {
	var buf bytes.Buffer
	if err := openTemplate.Execute(&buf, p); err != nil {
		log.Printf("open page: %v", err)
		http.Error(w, "Something went wrong.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

func toHome(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, "/", http.StatusFound)
}
