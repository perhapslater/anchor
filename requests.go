package main

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Request status. The "Request status" sheet on the Seerr tile tells the
// family whether what they asked for is ready, coming, downloading or stuck.
//
//	Seerr   what was asked for, whether it was approved, and the titles
//	Plex    the only thing that can say "ready to watch"
//	Sonarr  how many episodes have aired, the next air date, downloads
//	Radarr  release dates, downloads
//
// This file turns what those four say into one short row per title. The
// rules are in decide and are plain functions of their input, so they can be
// tested without any network.

// reqState is where a request stands in Seerr.
type reqState int

const (
	reqPending reqState = iota + 1
	reqApproved
	reqDeclined
	reqFailed // approved, but Seerr couldn't pass it on to Radarr/Sonarr
)

// mediaAvailable is Seerr's own "available" media status.
const mediaAvailable = 5

// stuckAfter is how long a title that is out may sit with nothing in Plex,
// no file and no download before the family is told it hasn't been found.
const stuckAfter = 48 * time.Hour

// requestRow is one title as the page gets it. State is always one of a
// fixed set, so the page's wording never depends on upstream text:
// ready, partial, downloading, soon, preparing, notfound, pending, declined,
// unconfirmed.
type requestRow struct {
	Title       string `json:"title"`
	Year        int    `json:"year,omitempty"`
	Kind        string `json:"kind"` // movie or show
	State       string `json:"state"`
	Ready       int    `json:"ready,omitempty"` // shows: episodes ready to watch
	Date        string `json:"date,omitempty"`  // YYYY-MM-DD
	Downloading bool   `json:"downloading,omitempty"`
	Stuck       bool   `json:"stuck,omitempty"`
	Detail      string `json:"detail,omitempty"` // admins only
	// Posters says where the title's poster may come from, best first, as
	// short names like "plex/10" or "tmdbtv/1399" (see posters.go). The page
	// asks Anchor for the first, and the next if that one fails.
	Posters []string `json:"posters,omitempty"`
	series  int      // Sonarr's id for the show, to tell when Coming up already lists it; never sent
}

// arrFacts is what Radarr (a movie) or Sonarr (the requested seasons of a
// show) says about one title. A nil *arrFacts means "no answer".
type arrFacts struct {
	Aired, Files int       // shows: aired monitored episodes, and files for them
	HasFile      bool      // movies
	Available    bool      // movies: Radarr is allowed to look for it now
	Expected     string    // YYYY-MM-DD: movie home release, or a show's next air date
	Upcoming     bool      // Expected is still ahead of us
	Since        time.Time // when it became gettable; zero if unknown
	Downloading  bool
	QueueProblem bool // a download is failing or stalled
	Series       int  // shows: Sonarr's own id for the show
	Movie        int  // movies: Radarr's own id for the movie
}

// facts is everything decide needs about one request.
type facts struct {
	kind        string // movie or show
	req         reqState
	created     time.Time // when it was requested
	mediaStatus int       // Seerr's media status
	plexOK      bool      // false: Plex could not be asked
	inPlex      bool      // movies
	plexEps     int       // shows: episodes in Plex for the requested seasons
	arr         *arrFacts
	now         time.Time
}

// decide says where a title stands. Nothing is "ready" unless Plex said so.
func decide(f facts) requestRow {
	row := requestRow{Kind: f.kind}
	switch f.req {
	case reqDeclined:
		row.State = "declined"
		return row
	case reqPending:
		row.State = "pending"
		return row
	case reqFailed:
		row.State, row.Detail = "notfound", "Seerr could not pass this request on to Radarr or Sonarr."
		return row
	}
	if f.kind == "movie" {
		return decideMovie(f, row)
	}
	return decideShow(f, row)
}

// stuckFor reports whether the title has been gettable for longer than
// stuckAfter. With no date from Radarr or Sonarr, the request's own age is
// the clock.
func stuckFor(f facts, since time.Time) (time.Time, bool) {
	if f.created.After(since) {
		since = f.created
	}
	return since, !since.IsZero() && f.now.Sub(since) > stuckAfter
}

func decideMovie(f facts, row requestRow) requestRow {
	a := f.arr
	if f.plexOK && f.inPlex {
		row.State = "ready"
		return row
	}
	if !f.plexOK && (f.mediaStatus == mediaAvailable || (a != nil && a.HasFile)) {
		row.State = "unconfirmed"
		return row
	}
	if a == nil {
		row.State = "preparing"
		return row
	}
	switch {
	case a.Downloading:
		row.State = "downloading"
	case !a.Available:
		row.State = "soon"
		if a.Upcoming {
			row.Date = a.Expected
		}
	case a.HasFile:
		row.State, row.Detail = "preparing", "Downloaded; waiting for Plex to add it."
	case a.QueueProblem:
		row.State, row.Detail = "notfound", "Radarr's download is failing or stalled."
	default:
		if since, stuck := stuckFor(f, a.Since); stuck {
			row.State = "notfound"
			row.Detail = fmt.Sprintf("Radarr: nothing found since %s; no file, not in the download queue.", since.Format("2006-01-02"))
		} else {
			row.State = "preparing"
		}
	}
	return row
}

func decideShow(f facts, row requestRow) requestRow {
	a := f.arr
	plexEps := 0
	if f.plexOK {
		plexEps = f.plexEps
	}

	if a == nil { // Sonarr has no answer: only Seerr and Plex
		switch {
		case !f.plexOK && f.mediaStatus == mediaAvailable:
			row.State = "unconfirmed"
		case f.plexOK && plexEps > 0 && f.mediaStatus == mediaAvailable:
			row.State = "ready"
		case f.plexOK && plexEps > 0:
			row.State, row.Ready = "partial", plexEps
		default:
			row.State = "preparing"
		}
		return row
	}

	missing := a.Files < a.Aired
	since, stuck := stuckFor(f, a.Since)
	stuck = stuck && missing
	failing := a.QueueProblem

	complete := a.Aired > 0 && plexEps >= a.Aired
	if complete && !a.Upcoming {
		row.State = "ready"
		return row
	}
	if complete || plexEps > 0 {
		row.State, row.Ready = "partial", plexEps
		switch {
		case a.Downloading:
			row.Downloading = true
		case failing || stuck:
			row.Stuck = true
			row.Detail = stuckShowDetail(a, since)
		case a.Upcoming:
			row.Date = a.Expected
		}
		return row
	}

	// Nothing in Plex for the requested seasons (or Plex couldn't be asked).
	switch {
	case !f.plexOK && a.Aired > 0 && !missing && !a.Upcoming:
		row.State = "unconfirmed"
	case a.Aired == 0:
		row.State = "soon"
		if a.Upcoming {
			row.Date = a.Expected
		}
	case a.Downloading:
		row.State = "downloading"
	case failing || stuck:
		row.State, row.Detail = "notfound", stuckShowDetail(a, since)
	default:
		row.State = "preparing"
	}
	return row
}

func stuckShowDetail(a *arrFacts, since time.Time) string {
	if a.QueueProblem {
		return "Sonarr's download is failing or stalled."
	}
	return fmt.Sprintf("Sonarr: %d of %d aired episodes still missing since %s; none in the download queue.",
		a.Aired-a.Files, a.Aired, since.Format("2006-01-02"))
}

// cleanText makes a piece of text from outside (a title from Seerr) safe to
// show: no control, invisible or direction-changing characters, runs of
// white space collapsed, and at most max characters. It does not escape
// anything; the page shows it as plain text and JSON encoding escapes it.
func cleanText(s string, max int) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case r == utf8.RuneError:
			// an invalid byte
		case unicode.IsSpace(r):
			space = b.Len() > 0
		case !unicode.IsGraphic(r):
			// control, format (zero-width, bidi) and unassigned characters
		default:
			if space {
				b.WriteByte(' ')
				space = false
			}
			b.WriteRune(r)
		}
	}
	out := b.String()
	if rs := []rune(out); len(rs) > max {
		out = strings.TrimSpace(string(rs[:max]))
	}
	return out
}
