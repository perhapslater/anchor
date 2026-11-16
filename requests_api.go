package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The endpoints behind the "Request status" sheet:
//
//	GET  /api/requests                         the list (refreshed first if it is over 5 minutes old)
//	POST /api/requests/refresh                 Refresh now
//	GET  /api/requests/poster/{plex|tmdbmovie|tmdbtv|movie|show}/{id}   one poster for a title (see posters.go)
//
// All start at the same gate (requestsGate): Tailscale identity, a person
// listed in anchor.yaml, and access to the tile the feature is switched on
// for. Nobody else learns anything, not even that the feature exists; every
// reason for saying no gets the same 404. Every answer is no-store. What goes
// out is Anchor's own summary (see requestRow), never what the services said;
// the one exception is a poster, which is the picture itself and nothing else.

type apiRequests struct {
	Rows     []requestRow `json:"rows"`
	Coming   *comingList  `json:"coming,omitempty"` // the Coming up tab; left out if there is nothing to build it from
	Updated  string       `json:"updated"`          // when the list was last built, RFC 3339
	Age      int          `json:"age"`              // seconds since then, so a phone with a wrong clock still says the right thing
	PlexOK   bool         `json:"plexOk"`           // false: Plex couldn't be asked, so nothing says ready
	Stale    bool         `json:"stale,omitempty"`
	Message  string       `json:"message,omitempty"`
	Problems []string     `json:"problems,omitempty"` // admins only
}

func (h *handler) requestsGate(w http.ResponseWriter, r *http.Request) (*Config, visitor, bool) {
	w.Header().Set("Cache-Control", "no-store")
	cfg, _ := h.store.Get()
	v := h.identify(r)
	if selected := cfg.requestFor(r.URL.Query().Get("app")); h.requests != nil && selected != nil {
		if app := cfg.appByID(selected.App); app != nil && cfg.visibleTo(app, v.Login) {
			scoped := *cfg
			scoped.Requests = selected
			return &scoped, v, true
		}
	}
	jsonReply(w, http.StatusNotFound, "Not found.")
	return nil, v, false
}

func (h *handler) requestsList(w http.ResponseWriter, r *http.Request) {
	cfg, v, ok := h.requestsGate(w, r)
	if !ok {
		return
	}
	if !h.reqReads.allow(v.Login + "\n" + cfg.Requests.App) {
		w.Header().Set("Retry-After", "2")
		jsonReply(w, http.StatusTooManyRequests, "Please wait a moment and try again.")
		return
	}
	h.writeRequests(w, cfg, v, h.requestService(cfg).get(r.Context(), cfg))
}

// requestsPoster serves the poster of one title on the sheet:
//
//	GET /api/requests/poster/{plex|tmdbmovie|tmdbtv|movie|show}/{id}
//
// The kind and number are one of the names the list gave a title (see
// posters.go). It goes through the same gate as the list, so a person who may
// not see the sheet learns nothing, not even that this exists. It only answers
// for names on the sheet as it stands, never starts a refresh, and has its own
// limit per person (a screenful of posters arrives at once). What goes out is
// the picture and Anchor's own headers; nothing the service sent along with it.
func (h *handler) requestsPoster(w http.ResponseWriter, r *http.Request) {
	cfg, v, ok := h.requestsGate(w, r)
	if !ok {
		return
	}
	if !h.posterReads.allow(v.Login) {
		w.Header().Set("Retry-After", "1")
		jsonReply(w, http.StatusTooManyRequests, "Please wait a moment and try again.")
		return
	}
	ref, ok := parsePosterRef(r.PathValue("kind"), r.PathValue("id"))
	if !ok {
		jsonReply(w, http.StatusNotFound, "Not found.")
		return
	}
	data, ctype, ok := h.requestService(cfg).poster(r.Context(), cfg, ref)
	if !ok {
		jsonReply(w, http.StatusNotFound, "Not found.")
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", ctype)
	hd.Set("Content-Length", strconv.Itoa(len(data)))
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Security-Policy", "default-src 'none'")
	hd.Set("Cross-Origin-Resource-Policy", "same-origin")
	_, _ = w.Write(data)
}

// requestsRefresh is Refresh now. Like the "Not working?" button it only
// works from Anchor's own page (same-origin, JSON body).
func (h *handler) requestsRefresh(w http.ResponseWriter, r *http.Request) {
	cfg, v, ok := h.requestsGate(w, r)
	if !ok {
		return
	}
	if !sameOrigin(r) {
		jsonReply(w, http.StatusForbidden, "This button only works from Anchor itself.")
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		jsonReply(w, http.StatusUnsupportedMediaType, "Expected JSON.")
		return
	}
	_, _ = io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, 1024)) // nothing in the body is used
	h.writeRequests(w, cfg, v, h.requestService(cfg).force(r.Context(), cfg))
}

func (h *handler) writeRequests(w http.ResponseWriter, cfg *Config, v visitor, res requestResult) {
	service := h.requestService(cfg)
	admin := cfg.isAdmin(v.Login)
	if res.err != nil || res.snap == nil {
		msg := "Request status isn't available right now. Please try again in a minute."
		if admin && res.err != nil {
			msg += " (" + res.err.Error() + ")"
		}
		jsonReply(w, http.StatusServiceUnavailable, msg)
		return
	}

	snap := res.snap
	out := apiRequests{
		Rows:    make([]requestRow, len(snap.Rows)), // a copy: the snapshot is shared
		Coming:  snap.Coming,                        // nothing in it is for admins only, so it isn't copied
		Updated: snap.Taken.UTC().Format(time.RFC3339),
		Age:     max(0, int(service.now().Sub(snap.Taken).Seconds())),
		PlexOK:  snap.PlexOK,
		Stale:   res.stale,
	}
	copy(out.Rows, snap.Rows)
	if admin {
		// Which service's posters aren't loading is for the admin too.
		out.Problems = append(append([]string(nil), snap.Problems...), service.health.problems()...)
	} else {
		for i := range out.Rows {
			out.Rows[i].Detail = ""
		}
	}
	if res.coolDown {
		if res.stale {
			out.Message = "Couldn't refresh just now. Please try again in a minute."
		} else {
			out.Message = fmt.Sprintf("Already up to date. Checked %s.", agoText(service.now().Sub(snap.Taken)))
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

// agoText says how long ago something was, roughly.
func agoText(d time.Duration) string {
	switch {
	case d < 5*time.Second:
		return "just now"
	case d < 90*time.Second:
		return fmt.Sprintf("%d seconds ago", int(d.Seconds()))
	}
	return fmt.Sprintf("%d minutes ago", int(d.Minutes()+0.5))
}
