package main

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// ---------- config ----------

const requestsBase = `
people:
  mom@example.com: {name: Mom, groups: [family]}
apps:
  - {name: Seerr, url: https://seerr.test/, access: [family], check: "off"}
`

func TestRequestsConfigParses(t *testing.T) {
	cfg, err := parseConfig([]byte(requestsBase + `
requests:
  app: seerr
  seerr: http://192.168.1.10:5055/
  plex: http://192.168.1.10:32400
  sonarr: http://192.168.1.10:8989
  radarr: https://radarr.example.ts.net/prefix
`))
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Requests
	if r == nil {
		t.Fatal("requests block was dropped")
	}
	want := Requests{
		App:    "seerr",
		Seerr:  "http://192.168.1.10:5055", // trailing slash trimmed
		Plex:   "http://192.168.1.10:32400",
		Sonarr: "http://192.168.1.10:8989",
		Radarr: "https://radarr.example.ts.net/prefix",
	}
	if *r != want {
		t.Fatalf("got %+v, want %+v", *r, want)
	}
}

func TestRequestsConfigIsOptional(t *testing.T) {
	cfg, err := parseConfig([]byte(requestsBase))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Requests != nil {
		t.Fatalf("no block should mean the feature is off: %+v", cfg.Requests)
	}
	// Sonarr and Radarr are optional.
	cfg, err = parseConfig([]byte(requestsBase + "requests: {app: seerr, seerr: 'http://s:5055', plex: 'http://p:32400'}\n"))
	if err != nil || cfg.Requests == nil {
		t.Fatalf("seerr + plex alone should be fine: %v", err)
	}
}

func TestRequestsConfigIsStrict(t *testing.T) {
	good := "app: seerr, seerr: 'http://s:5055', plex: 'http://p:32400'"
	bad := map[string]string{
		"missing app":        "{seerr: 'http://s:5055', plex: 'http://p:32400'}",
		"app not in file":    "{app: nope, seerr: 'http://s:5055', plex: 'http://p:32400'}",
		"missing seerr":      "{app: seerr, plex: 'http://p:32400'}",
		"missing plex":       "{app: seerr, seerr: 'http://s:5055'}",
		"typo in a key":      "{" + good + ", sonar: 'http://x:8989'}",
		"ftp scheme":         "{app: seerr, seerr: 'ftp://s:5055', plex: 'http://p:32400'}",
		"no host":            "{app: seerr, seerr: 'http://', plex: 'http://p:32400'}",
		"not a url":          "{app: seerr, seerr: 'not a url', plex: 'http://p:32400'}",
		"credentials in url": "{app: seerr, seerr: 'http://u:p@s:5055', plex: 'http://p:32400'}",
		"query in url":       "{app: seerr, seerr: 'http://s:5055/?k=v', plex: 'http://p:32400'}",
		"fragment in url":    "{app: seerr, seerr: 'http://s:5055/#f', plex: 'http://p:32400'}",
		"bad sonarr":         "{" + good + ", sonarr: 'gopher://x'}",
		"bad radarr":         "{" + good + ", radarr: 'http://'}",
	}
	for name, block := range bad {
		_, err := parseConfig([]byte(requestsBase + "requests: " + block + "\n"))
		if err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	// The error should say what to fix.
	_, err := parseConfig([]byte(requestsBase + "requests: {app: nope, seerr: 'http://s:5055', plex: 'http://p:32400'}\n"))
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("error should name the bad app id, got %v", err)
	}
}

func TestRequestsAppIsTheTilesID(t *testing.T) {
	// The id defaults to the slug of the name, so "Seerr" is "seerr" and
	// "Plex Requests" would be "plex-requests".
	cfg, err := parseConfig([]byte(`
people:
  mom@example.com: {name: Mom, groups: [family]}
apps:
  - {name: Plex Requests, url: https://r.test/, access: [family], check: "off"}
requests: {app: plex-requests, seerr: 'http://s:5055', plex: 'http://p:32400'}
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Requests.App != "plex-requests" {
		t.Fatalf("app = %q", cfg.Requests.App)
	}
}

func TestRequestsSigChangesWithAnyField(t *testing.T) {
	base := Requests{App: "a", Seerr: "http://s", Plex: "http://p", Sonarr: "http://so", Radarr: "http://ra"}
	seen := map[string]bool{base.sig(): true}
	for _, r := range []Requests{
		{App: "b", Seerr: "http://s", Plex: "http://p", Sonarr: "http://so", Radarr: "http://ra"},
		{App: "a", Seerr: "http://x", Plex: "http://p", Sonarr: "http://so", Radarr: "http://ra"},
		{App: "a", Seerr: "http://s", Plex: "http://x", Sonarr: "http://so", Radarr: "http://ra"},
		{App: "a", Seerr: "http://s", Plex: "http://p", Sonarr: "http://x", Radarr: "http://ra"},
		{App: "a", Seerr: "http://s", Plex: "http://p", Sonarr: "http://so", Radarr: "http://x"},
	} {
		if seen[r.sig()] {
			t.Errorf("sig did not change for %+v", r)
		}
		seen[r.sig()] = true
	}
}

// ---------- cleaning text from outside ----------

func TestCleanText(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"plain":           {"Dune: Part Two", "Dune: Part Two"},
		"accents":         {"Amélie", "Amélie"},
		"japanese":        {"千と千尋の神隠し", "千と千尋の神隠し"},
		"control chars":   {"A\x00B\x07C\x1bD\x7fE", "ABCDE"},
		"newlines":        {"Line one\nLine two\r\n\tthree", "Line one Line two three"},
		"line separators": {"a\U00002028b\U00002029c", "a b c"},
		"bidi overrides":  {"abc\U0000202edef\U0000202c \U00002066x\U00002069", "abcdef x"},
		"zero width":      {"a\U0000200bb\U0000200dc\U0000feffd\U0000200e", "abcd"},
		"tag characters":  {"a\U000e0041b", "ab"},
		"collapses":       {"  lots   of    space  ", "lots of space"},
		"invalid utf8":    {"ok\xffgo\xc3", "okgo"},
		"markup is text":  {"<img src=x onerror=alert(1)>", "<img src=x onerror=alert(1)>"},
		"empty":           {"", ""},
		"only junk":       {"\x00\U0000202e \n", ""},
	}
	for name, c := range cases {
		if got := cleanText(c.in, 120); got != c.want {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
}

func TestCleanTextCutsByCharactersNotBytes(t *testing.T) {
	long := strings.Repeat("千", 500)
	got := cleanText(long, 120)
	if n := len([]rune(got)); n != 120 {
		t.Fatalf("got %d characters, want 120", n)
	}
	if !utf8.ValidString(got) {
		t.Fatal("cut in the middle of a character")
	}
	// A cut right after a space doesn't leave a trailing space.
	if got := cleanText("abcd efgh", 5); got != "abcd" {
		t.Fatalf("got %q", got)
	}
}

// ---------- deciding what to say ----------

var t0 = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func hoursAgo(h int) time.Time { return t0.Add(-time.Duration(h) * time.Hour) }

func movieFacts(arr *arrFacts) facts {
	return facts{kind: "movie", req: reqApproved, created: hoursAgo(100), plexOK: true, arr: arr, now: t0}
}

func showFacts(plexEps int, arr *arrFacts) facts {
	return facts{kind: "show", req: reqApproved, created: hoursAgo(100), plexOK: true, plexEps: plexEps, arr: arr, now: t0}
}

type want struct {
	state       string
	ready       int
	date        string
	downloading bool
	stuck       bool
}

func check(t *testing.T, name string, f facts, w want) {
	t.Helper()
	got := decide(f)
	if got.State != w.state || got.Ready != w.ready || got.Date != w.date || got.Downloading != w.downloading || got.Stuck != w.stuck {
		t.Errorf("%s: got %+v, want %+v", name, got, w)
	}
	if got.Kind != f.kind {
		t.Errorf("%s: kind %q, want %q", name, got.Kind, f.kind)
	}
}

func TestDecideSeerrStates(t *testing.T) {
	for _, kind := range []string{"movie", "show"} {
		f := facts{kind: kind, plexOK: true, inPlex: true, plexEps: 9, now: t0, created: hoursAgo(1)}
		f.req = reqDeclined
		check(t, kind+" declined", f, want{state: "declined"})
		f.req = reqPending
		check(t, kind+" pending", f, want{state: "pending"})
		f.req = reqFailed
		check(t, kind+" failed", f, want{state: "notfound"})
		if d := decide(f).Detail; !strings.Contains(d, "Seerr") {
			t.Errorf("%s failed: detail %q should point at Seerr", kind, d)
		}
	}
}

func TestDecideMovies(t *testing.T) {
	const soon = "2026-11-12"
	avail := func(mod func(*arrFacts)) *arrFacts {
		a := &arrFacts{Available: true}
		if mod != nil {
			mod(a)
		}
		return a
	}

	f := movieFacts(avail(nil))
	f.inPlex = true
	check(t, "in Plex", f, want{state: "ready"})

	f = movieFacts(nil)
	f.inPlex = true
	check(t, "in Plex, no Radarr", f, want{state: "ready"})

	// Plex can't be asked: never "ready".
	f = movieFacts(avail(nil))
	f.plexOK, f.inPlex, f.mediaStatus = false, true, mediaAvailable
	check(t, "Plex down, Seerr says available", f, want{state: "unconfirmed"})
	f = movieFacts(avail(func(a *arrFacts) { a.HasFile = true }))
	f.plexOK = false
	check(t, "Plex down, Radarr has the file", f, want{state: "unconfirmed"})
	f = movieFacts(avail(func(a *arrFacts) { a.Downloading = true }))
	f.plexOK = false
	check(t, "Plex down, downloading", f, want{state: "downloading"})

	check(t, "downloading", movieFacts(avail(func(a *arrFacts) { a.Downloading = true })), want{state: "downloading"})

	check(t, "not out yet, date known",
		movieFacts(&arrFacts{Available: false, Upcoming: true, Expected: soon}), want{state: "soon", date: soon})
	check(t, "not out yet, no date",
		movieFacts(&arrFacts{Available: false}), want{state: "soon"})
	check(t, "not available but the date has passed: no past date shown",
		movieFacts(&arrFacts{Available: false, Expected: "2026-09-01"}), want{state: "soon"})

	check(t, "downloaded, waiting for Plex",
		movieFacts(avail(func(a *arrFacts) { a.HasFile = true; a.Since = hoursAgo(500) })), want{state: "preparing"})

	check(t, "out 49 hours, nothing found",
		movieFacts(avail(func(a *arrFacts) { a.Since = hoursAgo(49) })), want{state: "notfound"})
	check(t, "out 47 hours: still looking",
		movieFacts(avail(func(a *arrFacts) { a.Since = hoursAgo(47) })), want{state: "preparing"})
	check(t, "a failing download",
		movieFacts(avail(func(a *arrFacts) { a.QueueProblem = true; a.Since = hoursAgo(1) })), want{state: "notfound"})

	// Radarr will search right away ("announced"); with no date the request's
	// own age is the clock.
	f = movieFacts(avail(nil))
	f.created = hoursAgo(100)
	check(t, "no date, requested long ago", f, want{state: "notfound"})
	f.created = hoursAgo(10)
	check(t, "no date, requested today", f, want{state: "preparing"})

	check(t, "no Radarr data", movieFacts(nil), want{state: "preparing"})
	if d := decide(movieFacts(avail(func(a *arrFacts) { a.Since = hoursAgo(49) }))).Detail; d == "" {
		t.Error("a stuck title should explain itself to the admin")
	}
}

func TestDecideShows(t *testing.T) {
	const next = "2026-10-12"

	check(t, "everything aired is in Plex",
		showFacts(8, &arrFacts{Aired: 8, Files: 8}), want{state: "ready"})
	check(t, "all aired in Plex, more coming",
		showFacts(8, &arrFacts{Aired: 8, Files: 8, Upcoming: true, Expected: next}), want{state: "partial", ready: 8, date: next})
	check(t, "some in Plex, rest downloading",
		showFacts(3, &arrFacts{Aired: 8, Files: 3, Downloading: true}), want{state: "partial", ready: 3, downloading: true})
	check(t, "some in Plex, rest not found",
		showFacts(3, &arrFacts{Aired: 8, Files: 3, Since: hoursAgo(100)}), want{state: "partial", ready: 3, stuck: true})
	check(t, "some in Plex, rest failing",
		showFacts(3, &arrFacts{Aired: 8, Files: 3, QueueProblem: true, Since: hoursAgo(1)}), want{state: "partial", ready: 3, stuck: true})
	check(t, "some in Plex, a new episode aired a few hours ago",
		showFacts(3, &arrFacts{Aired: 4, Files: 3, Since: hoursAgo(10), Upcoming: true, Expected: next}),
		want{state: "partial", ready: 3, date: next})
	check(t, "Sonarr has the files, Plex hasn't caught up",
		showFacts(3, &arrFacts{Aired: 8, Files: 8, Since: hoursAgo(100)}), want{state: "partial", ready: 3})

	check(t, "nothing aired yet, date known",
		showFacts(0, &arrFacts{Upcoming: true, Expected: "2026-11-05"}), want{state: "soon", date: "2026-11-05"})
	check(t, "nothing aired yet, no date",
		showFacts(0, &arrFacts{}), want{state: "soon"})

	// The family asks for season 2 of a show whose season 1 is already in
	// Plex. plexEps counts only the requested seasons, so this is never ready.
	check(t, "season 2 aired, not in Plex",
		showFacts(0, &arrFacts{Aired: 10, Files: 0, Since: hoursAgo(10)}), want{state: "preparing"})
	check(t, "season 2 downloading",
		showFacts(0, &arrFacts{Aired: 10, Downloading: true}), want{state: "downloading"})
	check(t, "season 2 missing for days",
		showFacts(0, &arrFacts{Aired: 10, Since: hoursAgo(100)}), want{state: "notfound"})
	check(t, "season 2 download failing",
		showFacts(0, &arrFacts{Aired: 10, QueueProblem: true}), want{state: "notfound"})
	check(t, "downloaded, waiting for Plex",
		showFacts(0, &arrFacts{Aired: 10, Files: 10, Since: hoursAgo(100)}), want{state: "preparing"})

	// Plex can't be asked.
	f := showFacts(8, &arrFacts{Aired: 8, Files: 8})
	f.plexOK = false
	check(t, "Plex down, Sonarr has it all", f, want{state: "unconfirmed"})
	f = showFacts(8, &arrFacts{Aired: 8, Files: 3, Since: hoursAgo(100)})
	f.plexOK = false
	check(t, "Plex down: the Plex count is not trusted", f, want{state: "notfound"})

	// No Sonarr data: only Seerr and Plex.
	f = showFacts(5, nil)
	f.mediaStatus = mediaAvailable
	check(t, "no Sonarr, Seerr says available", f, want{state: "ready"})
	f.mediaStatus = 3
	check(t, "no Sonarr, Seerr says processing", f, want{state: "partial", ready: 5})
	check(t, "no Sonarr, nothing in Plex", showFacts(0, nil), want{state: "preparing"})
	f = showFacts(0, nil)
	f.plexOK, f.mediaStatus = false, mediaAvailable
	check(t, "no Sonarr, Plex down, Seerr says available", f, want{state: "unconfirmed"})
	f.mediaStatus = 3
	check(t, "no Sonarr, Plex down", f, want{state: "preparing"})
}

func TestNothingIsReadyWhilePlexIsDown(t *testing.T) {
	// Every combination of Seerr and arr facts, with Plex down, must avoid
	// "ready" and "partial".
	for _, kind := range []string{"movie", "show"} {
		for _, media := range []int{0, 3, 4, mediaAvailable} {
			for _, a := range []*arrFacts{
				nil, {}, {Available: true}, {Available: true, HasFile: true},
				{Aired: 8, Files: 8}, {Aired: 8, Files: 8, Upcoming: true, Expected: "2026-10-12"}, {Aired: 8, Files: 3},
			} {
				f := facts{kind: kind, req: reqApproved, created: hoursAgo(1), plexOK: false, inPlex: true, plexEps: 99, mediaStatus: media, arr: a, now: t0}
				if got := decide(f); got.State == "ready" || got.State == "partial" {
					t.Errorf("%s media=%d arr=%+v: said %q with Plex down", kind, media, a, got.State)
				}
			}
		}
	}
}
