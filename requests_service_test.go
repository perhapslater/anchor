package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------- a pretend Seerr, Plex, Sonarr and Radarr ----------

// gauge counts how many calls are in flight across all the fake servers.
type gauge struct {
	mu       sync.Mutex
	cur, max int
}

func (g *gauge) enter() {
	g.mu.Lock()
	g.cur++
	if g.cur > g.max {
		g.max = g.cur
	}
	g.mu.Unlock()
}

func (g *gauge) leave() { g.mu.Lock(); g.cur--; g.mu.Unlock() }

type stackSrv struct {
	*httptest.Server
	mu     sync.Mutex
	hits   map[string]int // request URI -> times asked
	routes map[string]string
	fail   int // when set, every request gets this status
	delay  time.Duration
}

func (s *stackSrv) setFail(code int) { s.mu.Lock(); s.fail = code; s.mu.Unlock() }

func (s *stackSrv) count(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for uri, c := range s.hits {
		if strings.HasPrefix(uri, prefix) {
			n += c
		}
	}
	return n
}

func newStackSrv(t *testing.T, g *gauge, routes map[string]string) *stackSrv {
	t.Helper()
	s := &stackSrv{hits: map[string]int{}, routes: routes}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.enter()
		defer g.leave()
		s.mu.Lock()
		s.hits[r.URL.RequestURI()]++
		fail, delay := s.fail, s.delay
		s.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		if fail != 0 {
			http.Error(w, "broken", fail)
			return
		}
		body, ok := s.routes[r.URL.RequestURI()]
		if !ok {
			body, ok = s.routes[r.URL.Path]
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

type stack struct {
	seerr, plex, sonarr, radarr *stackSrv
	gauge                       *gauge
}

const hostileTitle = "<script>alert(1)</script>\U0000202eEvil"

func newStack(t *testing.T) *stack {
	t.Helper()
	g := &gauge{}
	st := &stack{gauge: g}
	st.seerr = newStackSrv(t, g, map[string]string{
		"/api/v1/request": `{"results":[
 {"status":2,"type":"movie","createdAt":"2026-10-03T08:00:00.000Z","media":{"tmdbId":100,"status":5}},
 {"status":2,"type":"tv","createdAt":"2026-10-02T08:00:00.000Z","media":{"tmdbId":200,"tvdbId":2000,"status":3},"seasons":[{"seasonNumber":2}]},
 {"status":2,"type":"movie","createdAt":"2026-10-01T08:00:00.000Z","media":{"tmdbId":101,"status":2}},
 {"status":1,"type":"movie","createdAt":"2026-09-30T08:00:00.000Z","media":{"tmdbId":102,"status":1}},
 {"status":2,"type":"movie","createdAt":"2026-09-29T08:00:00.000Z","media":{"tmdbId":100,"status":5}},
 {"status":2,"type":"tv","createdAt":"2026-09-28T08:00:00.000Z","media":{"tmdbId":201,"tvdbId":2001,"status":2},"seasons":[{"seasonNumber":1}]},
 {"status":2,"type":"movie","createdAt":"2026-09-27T08:00:00.000Z","media":{"tmdbId":103,"status":3}}]}`,
		"/api/v1/movie/100": `{"title":"Ready Movie","releaseDate":"2024-01-01"}`,
		"/api/v1/tv/200":    `{"name":"Slow Show","firstAirDate":"2022-02-02"}`,
		"/api/v1/movie/101": `{"title":"Future Movie","releaseDate":"2026-12-01"}`,
		"/api/v1/movie/102": `{"title":"Pending Movie","releaseDate":"2027-01-01"}`,
		"/api/v1/tv/201":    `{"name":"Mystery Show","firstAirDate":"2025-05-05"}`,
		"/api/v1/movie/103": `{"title":"` + hostileTitle + `","releaseDate":"2020-03-03"}`,
	})
	st.plex = newStackSrv(t, g, map[string]string{
		"/library/sections":             `{"MediaContainer":{"Directory":[{"key":"1","type":"movie"},{"key":"2","type":"show"}]}}`,
		"/library/sections/1/all":       plexLibrary(`{"ratingKey":"10","Guid":[{"id":"tmdb://100"}]}`),
		"/library/sections/2/all":       plexLibrary(`{"ratingKey":"20","Guid":[{"id":"tvdb://2000"}]}`),
		"/library/metadata/20/children": `{"MediaContainer":{"Metadata":[{"index":1,"leafCount":10},{"index":2,"leafCount":3}]}}`,
	})
	st.sonarr = newStackSrv(t, g, map[string]string{
		"/api/v3/series?tvdbId=2000": `[{"id":9,"seasons":[
			{"seasonNumber":1,"statistics":{"episodeFileCount":10,"episodeCount":10}},
			{"seasonNumber":2,"statistics":{"episodeFileCount":3,"episodeCount":8,"previousAiring":"2026-10-01T01:00:00Z"}}]}]`,
		"/api/v3/series?tvdbId=2001": `[]`,
		"/api/v3/queue":              `{"records":[{"seriesId":9,"status":"downloading","trackedDownloadStatus":"ok"}]}`,
		"/api/v3/calendar": `[
			{"seriesId":9,"seasonNumber":2,"episodeNumber":5,"airDateUtc":"2026-10-08T12:00:00Z","monitored":true,"series":{"title":"Slow Show","year":2022,"tvdbId":2000,"tmdbId":200}},
			{"seriesId":9,"seasonNumber":2,"episodeNumber":6,"airDateUtc":"2026-10-15T12:00:00Z","monitored":true,"series":{"title":"Slow Show","year":2022,"tvdbId":2000,"tmdbId":200}},
			{"seriesId":11,"seasonNumber":1,"episodeNumber":1,"airDateUtc":"2026-10-20T12:00:00Z","monitored":true,"series":{"title":"Brand New","year":2026}}]`,
	})
	st.radarr = newStackSrv(t, g, map[string]string{
		"/api/v3/movie?tmdbId=100": `[]`, // Radarr answers an empty list for a movie it doesn't have
		"/api/v3/movie?tmdbId=101": `[{"id":31,"isAvailable":false,"minimumAvailability":"released","digitalRelease":"2026-11-12T00:00:00Z"}]`,
		"/api/v3/movie?tmdbId=103": `[{"id":33,"isAvailable":true,"minimumAvailability":"released","digitalRelease":"2026-09-01T00:00:00Z"}]`,
		"/api/v3/queue":            `{"records":[{"movieId":33,"status":"downloading","trackedDownloadStatus":"ok"}]}`,
		// Radarr's own range would leave the last one out; the fake doesn't,
		// so this also checks that Anchor keeps to its six months.
		"/api/v3/calendar": `[
			{"id":40,"title":"Cinema Movie","year":2026,"monitored":true,"minimumAvailability":"inCinemas","inCinemas":"2026-10-20T00:00:00Z"},
			{"id":31,"title":"Future Movie","year":2026,"tmdbId":101,"monitored":true,"minimumAvailability":"released","digitalRelease":"2026-11-12T00:00:00Z"},
			{"id":41,"title":"Way Off","year":2027,"monitored":true,"minimumAvailability":"released","digitalRelease":"2027-06-01T00:00:00Z"}]`,
	})
	return st
}

func (st *stack) block() string {
	return "requests:\n  app: seerr\n  seerr: " + st.seerr.URL + "\n  plex: " + st.plex.URL +
		"\n  sonarr: " + st.sonarr.URL + "\n  radarr: " + st.radarr.URL + "\n"
}

var stackKeys = requestKeys{Seerr: "seerr-key", Plex: "plex-token", Sonarr: "sonarr-key", Radarr: "radarr-key"}

func storeFor(t *testing.T, cfg string) *ConfigStore {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "anchor.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := NewConfigStore(filepath.Join(dir, "anchor.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, e := store.Get(); e != "" {
		t.Fatalf("config error: %s", e)
	}
	return store
}

func newStackService(t *testing.T, st *stack, keys requestKeys) (*requestStatus, *Config) {
	t.Helper()
	store := storeFor(t, requestsBase+st.block())
	cfg, _ := store.Get()
	rs := newRequestStatus(store, keys, true)
	rs.now = func() time.Time { return t0 }
	return rs, cfg
}

// ---------- pulling everything together ----------

func TestPullBuildsTheRows(t *testing.T) {
	st := newStack(t)
	rs, cfg := newStackService(t, st, stackKeys)
	snap, err := rs.pull(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.PlexOK || len(snap.Problems) != 0 {
		t.Fatalf("plexOK=%v problems=%v", snap.PlexOK, snap.Problems)
	}
	var got []string
	for _, r := range snap.Rows {
		got = append(got, strings.Join([]string{r.Title, strconv.Itoa(r.Year), r.Kind, r.State, strconv.Itoa(r.Ready), r.Date, strconv.FormatBool(r.Downloading)}, "|"))
	}
	want := []string{
		"Ready Movie|2024|movie|ready|0||false",
		"Slow Show|2022|show|partial|3||true", // season 2 only: 3 of 8 aired are in Plex
		"Future Movie|2026|movie|soon|0|2026-11-12|false",
		"Pending Movie|2027|movie|pending|0||false",
		"Mystery Show|2025|show|preparing|0||false", // Sonarr doesn't know it yet
		"<script>alert(1)</script>Evil|2020|movie|downloading|0||false",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestPullAsksOnlyWhatItNeeds(t *testing.T) {
	st := newStack(t)
	rs, cfg := newStackService(t, st, stackKeys)
	if _, err := rs.pull(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	// A movie already in Plex, and one still waiting for approval, never
	// reach Radarr. (A movie in Plex gets its poster from Plex, too.)
	for _, id := range []string{"100", "102"} {
		if n := st.radarr.count("/api/v3/movie?tmdbId=" + id); n != 0 {
			t.Errorf("Radarr was asked about movie %s %d times", id, n)
		}
	}
	// Each title's name is looked up once, even though 100 was requested twice.
	if n := st.seerr.count("/api/v1/movie/100"); n != 1 {
		t.Errorf("movie 100 looked up %d times", n)
	}
	// The queue is read once per service, not once per title.
	if st.sonarr.count("/api/v3/queue") != 1 || st.radarr.count("/api/v3/queue") != 1 {
		t.Error("the download queues should each be read once")
	}
	// Each request used the right key.
	for _, srv := range []*stackSrv{st.seerr, st.plex, st.sonarr, st.radarr} {
		if srv.count("") == 0 {
			t.Errorf("a service was never asked")
		}
	}
}

func TestPullCachesTitles(t *testing.T) {
	st := newStack(t)
	rs, cfg := newStackService(t, st, stackKeys)
	for i := 0; i < 3; i++ {
		if _, err := rs.pull(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	if n := st.seerr.count("/api/v1/movie/") + st.seerr.count("/api/v1/tv/"); n != 6 {
		t.Fatalf("want 6 title lookups in total across 3 pulls, got %d", n)
	}
	// A week later they're asked again.
	rs.now = func() time.Time { return t0.Add(8 * 24 * time.Hour) }
	if _, err := rs.pull(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if n := st.seerr.count("/api/v1/movie/") + st.seerr.count("/api/v1/tv/"); n != 12 {
		t.Fatalf("after a week want 12 lookups, got %d", n)
	}
}

func TestTitleCacheIsBounded(t *testing.T) {
	c := newTitleCache()
	for i := 0; i < 2000; i++ {
		c.put("movie:"+strconv.Itoa(i), titleEntry{title: "x"}, t0.Add(time.Duration(i)*time.Second))
	}
	if n := c.size(); n > titleCacheMax {
		t.Fatalf("cache grew to %d (limit %d)", n, titleCacheMax)
	}
	// The newest are the ones kept.
	if _, ok := c.get("movie:1999", t0.Add(2000*time.Second)); !ok {
		t.Error("the newest entry was dropped")
	}
	if _, ok := c.get("movie:0", t0.Add(2000*time.Second)); ok {
		t.Error("the oldest entry should have been dropped")
	}
}

func TestPullCallsAtMostFourAtATime(t *testing.T) {
	st := newStack(t)
	for _, s := range []*stackSrv{st.seerr, st.plex, st.sonarr, st.radarr} {
		s.delay = 25 * time.Millisecond
	}
	rs, cfg := newStackService(t, st, stackKeys)
	if _, err := rs.pull(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	st.gauge.mu.Lock()
	max := st.gauge.max
	st.gauge.mu.Unlock()
	if max > parallelCalls {
		t.Fatalf("%d calls at once, limit is %d", max, parallelCalls)
	}
	if max < 2 {
		t.Fatalf("expected some parallelism, saw %d", max)
	}
}

func TestPullWhenPlexIsDown(t *testing.T) {
	st := newStack(t)
	st.plex.setFail(500)
	rs, cfg := newStackService(t, st, stackKeys)
	snap, err := rs.pull(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if snap.PlexOK {
		t.Fatal("PlexOK should be false")
	}
	if len(snap.Problems) != 1 || !strings.Contains(snap.Problems[0], "Plex") {
		t.Fatalf("problems: %v", snap.Problems)
	}
	for _, r := range snap.Rows {
		if r.State == "ready" || r.State == "partial" {
			t.Errorf("%q is %q while Plex can't be asked", r.Title, r.State)
		}
	}
	if snap.Rows[0].State != "unconfirmed" { // Seerr says it's available
		t.Errorf("first row: %+v", snap.Rows[0])
	}
}

func TestPullWhenAShowsSeasonsCantBeRead(t *testing.T) {
	// Plex answers the library but not the show's seasons: that show must not
	// be called ready or partial on a guess.
	st := newStack(t)
	delete(st.plex.routes, "/library/metadata/20/children")
	rs, cfg := newStackService(t, st, stackKeys)
	snap, err := rs.pull(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s := snap.Rows[1].State; s == "ready" || s == "partial" {
		t.Fatalf("Slow Show is %q without Plex's season counts", s)
	}
	if len(snap.Problems) != 1 || !strings.Contains(snap.Problems[0], "Plex") {
		t.Fatalf("problems: %v", snap.Problems)
	}
	if !snap.PlexOK {
		t.Error("the rest of Plex works")
	}
}

func TestPullWhenSonarrIsDown(t *testing.T) {
	st := newStack(t)
	st.sonarr.setFail(401)
	rs, cfg := newStackService(t, st, stackKeys)
	snap, err := rs.pull(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Problems) != 1 || !strings.Contains(snap.Problems[0], "Sonarr") {
		t.Fatalf("problems: %v", snap.Problems)
	}
	// Without Sonarr the show can only say what Plex has: 3 episodes, no extras.
	r := snap.Rows[1]
	if r.State != "partial" || r.Ready != 3 || r.Downloading || r.Date != "" {
		t.Fatalf("show without Sonarr: %+v", r)
	}
	// Movies are unaffected.
	if snap.Rows[2].State != "soon" || snap.Rows[2].Date != "2026-11-12" {
		t.Fatalf("movie: %+v", snap.Rows[2])
	}
	if n := st.sonarr.count("/api/v3/series"); n != 0 {
		t.Errorf("Sonarr was asked about shows %d times after its queue failed", n)
	}
}

func TestPullWhenRadarrIsDown(t *testing.T) {
	st := newStack(t)
	st.radarr.setFail(500)
	rs, cfg := newStackService(t, st, stackKeys)
	snap, err := rs.pull(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Problems) != 1 || !strings.Contains(snap.Problems[0], "Radarr") {
		t.Fatalf("problems: %v", snap.Problems)
	}
	if r := snap.Rows[2]; r.State != "preparing" || r.Date != "" {
		t.Fatalf("movie without Radarr: %+v", r)
	}
}

func TestPullFailsWhenSeerrIsDown(t *testing.T) {
	st := newStack(t)
	st.seerr.setFail(500)
	rs, cfg := newStackService(t, st, stackKeys)
	if _, err := rs.pull(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "Seerr") {
		t.Fatalf("got %v", err)
	}
}

func TestPullNamesTheMissingKey(t *testing.T) {
	for _, c := range []struct {
		name   string
		keys   requestKeys
		fails  bool
		wantIn string
	}{
		{"seerr key", requestKeys{Plex: "p", Sonarr: "s", Radarr: "r"}, true, "ANCHOR_SEERR_KEY"},
		{"plex token", requestKeys{Seerr: "k", Sonarr: "s", Radarr: "r"}, false, "ANCHOR_PLEX_TOKEN"},
		{"sonarr key", requestKeys{Seerr: "k", Plex: "p", Radarr: "r"}, false, "ANCHOR_SONARR_KEY"},
		{"radarr key", requestKeys{Seerr: "k", Plex: "p", Sonarr: "s"}, false, "ANCHOR_RADARR_KEY"},
	} {
		st := newStack(t)
		rs, cfg := newStackService(t, st, c.keys)
		snap, err := rs.pull(context.Background(), cfg)
		if c.fails {
			if err == nil || !strings.Contains(err.Error(), c.wantIn) {
				t.Errorf("%s: got %v", c.name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if len(snap.Problems) != 1 || !strings.Contains(snap.Problems[0], c.wantIn) {
			t.Errorf("%s: problems %v", c.name, snap.Problems)
		}
	}
}

func TestPullLeavesOutServicesNotInTheConfig(t *testing.T) {
	st := newStack(t)
	block := "requests:\n  app: seerr\n  seerr: " + st.seerr.URL + "\n  plex: " + st.plex.URL + "\n"
	store := storeFor(t, requestsBase+block)
	cfg, _ := store.Get()
	rs := newRequestStatus(store, requestKeys{Seerr: "k", Plex: "p"}, true)
	rs.now = func() time.Time { return t0 }
	snap, err := rs.pull(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Problems) != 0 {
		t.Fatalf("Sonarr and Radarr are optional: %v", snap.Problems)
	}
	if st.sonarr.count("") != 0 || st.radarr.count("") != 0 {
		t.Fatal("services that aren't in anchor.yaml were called")
	}
}

// ---------- Coming up ----------

// serviceFor builds a service from a requests: block and keys.
func serviceFor(t *testing.T, block string, keys requestKeys) (*requestStatus, *Config) {
	t.Helper()
	store := storeFor(t, requestsBase+block)
	cfg, _ := store.Get()
	rs := newRequestStatus(store, keys, true)
	rs.now = func() time.Time { return t0 }
	return rs, cfg
}

func comingLines(c *comingList) []string {
	var out []string
	for _, r := range c.Rows {
		out = append(out, strings.Join([]string{r.Title, strconv.Itoa(r.Year), r.Kind, r.Date, r.Note, strconv.Itoa(r.Season), strconv.Itoa(r.Count)}, "|"))
	}
	return out
}

func TestPullBuildsComingUp(t *testing.T) {
	st := newStack(t)
	rs, cfg := newStackService(t, st, stackKeys)
	snap, err := rs.pull(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Coming == nil || len(snap.Coming.Missing) != 0 || len(snap.Problems) != 0 {
		t.Fatalf("coming %+v, problems %v", snap.Coming, snap.Problems)
	}
	want := []string{
		"Slow Show|2022|show|2026-10-08|episode|0|0",
		"Brand New|2026|show|2026-10-20|premiere|1|0",
		"Cinema Movie|2026|movie|2026-10-20|cinemas|0|0",
		"Future Movie|2026|movie|2026-11-12|home|0|0",
		// Way Off is in June, past the six months, so it isn't here.
	}
	if got := comingLines(snap.Coming); !reflect.DeepEqual(got, want) {
		t.Fatalf("coming up:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// One calendar call to each service, however many rows.
	if st.sonarr.count("/api/v3/calendar") != 1 || st.radarr.count("/api/v3/calendar") != 1 {
		t.Errorf("calendar calls: sonarr %d, radarr %d", st.sonarr.count("/api/v3/calendar"), st.radarr.count("/api/v3/calendar"))
	}
	// The Requests tab is exactly what it was.
	if len(snap.Rows) != 6 || snap.Rows[2].Date != "2026-11-12" {
		t.Errorf("rows changed: %+v", snap.Rows)
	}
}

// quietSlowShow makes Slow Show (requested season 2, 3 of 8 episodes in Plex)
// a show with nothing downloading and a next episode at the given time.
func quietSlowShow(st *stack, nextAiring string) {
	st.sonarr.routes["/api/v3/queue"] = `{"records":[]}`
	st.sonarr.routes["/api/v3/series?tvdbId=2000"] = `[{"id":9,"seasons":[
		{"seasonNumber":1,"statistics":{"episodeFileCount":10,"episodeCount":10}},
		{"seasonNumber":2,"statistics":{"episodeFileCount":3,"episodeCount":8,"previousAiring":"2026-10-01T01:00:00Z","nextAiring":"` + nextAiring + `"}}]}]`
}

func TestPullDoesNotRepeatTheNextEpisodeOnRequests(t *testing.T) {
	const calendarFirst = "2026-10-08T12:00:00Z" // when the fake calendar has Slow Show's next episode
	local := func(iso string) string { return parseTime(iso).In(time.Local).Format("2006-01-02") }
	for _, c := range []struct {
		name     string
		next     string // what Sonarr's show page says
		prepare  func(st *stack)
		wantDate string // what Requests still says
	}{
		{"same episode on both tabs", calendarFirst, nil, ""},
		{"the calendar couldn't be read", calendarFirst, func(st *stack) { delete(st.sonarr.routes, "/api/v3/calendar") }, local(calendarFirst)},
		{"the calendar has a different day", "2026-10-09T12:00:00Z", nil, local("2026-10-09T12:00:00Z")},
		{"too far off for the calendar", "2026-12-01T12:00:00Z", func(st *stack) { st.sonarr.routes["/api/v3/calendar"] = `[]` }, local("2026-12-01T12:00:00Z")},
	} {
		st := newStack(t)
		quietSlowShow(st, c.next)
		if c.prepare != nil {
			c.prepare(st)
		}
		rs, cfg := newStackService(t, st, stackKeys)
		snap, err := rs.pull(context.Background(), cfg)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if r := snap.Rows[1]; r.Title != "Slow Show" || r.State != "partial" || r.Ready != 3 || r.Date != c.wantDate {
			t.Errorf("%s: Requests row %+v, want date %q", c.name, r, c.wantDate)
		}
	}
	// Whatever Requests says, Coming up still lists the show.
	st := newStack(t)
	quietSlowShow(st, calendarFirst)
	rs, cfg := newStackService(t, st, stackKeys)
	snap, _ := rs.pull(context.Background(), cfg)
	if got := comingLines(snap.Coming); !strings.HasPrefix(got[0], "Slow Show|2022|show|"+local(calendarFirst)) {
		t.Fatalf("Coming up: %v", got)
	}
}

func TestPullWhenOnlyTheCalendarFails(t *testing.T) {
	st := newStack(t)
	delete(st.sonarr.routes, "/api/v3/calendar") // the queue still works; the calendar answers 404
	rs, cfg := newStackService(t, st, stackKeys)
	snap, err := rs.pull(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Problems) != 1 || !strings.Contains(snap.Problems[0], "Sonarr") {
		t.Fatalf("problems: %v", snap.Problems)
	}
	if !reflect.DeepEqual(snap.Coming.Missing, []string{"shows"}) {
		t.Fatalf("missing %v", snap.Coming.Missing)
	}
	if got := comingLines(snap.Coming); !reflect.DeepEqual(got, []string{"Cinema Movie|2026|movie|2026-10-20|cinemas|0|0", "Future Movie|2026|movie|2026-11-12|home|0|0"}) {
		t.Fatalf("the movies half should still be there: %v", got)
	}
	// And the Requests tab doesn't notice.
	if r := snap.Rows[1]; r.State != "partial" || !r.Downloading {
		t.Fatalf("show row: %+v", r)
	}
}

func TestPullDoesNotAskForTheCalendarOfAServiceThatIsDown(t *testing.T) {
	st := newStack(t)
	st.sonarr.setFail(500)
	st.radarr.setFail(500)
	rs, cfg := newStackService(t, st, stackKeys)
	snap, err := rs.pull(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if st.sonarr.count("/api/v3/calendar") != 0 || st.radarr.count("/api/v3/calendar") != 0 {
		t.Error("a service whose queue just failed was asked for its calendar too")
	}
	if !reflect.DeepEqual(snap.Coming.Missing, []string{"shows", "movies"}) || len(snap.Coming.Rows) != 0 {
		t.Fatalf("coming %+v", snap.Coming)
	}
	if snap.Coming.Rows == nil {
		t.Error("rows should be an empty list, not null")
	}
}

func TestPullComingUpNeedsTheKeyToo(t *testing.T) {
	st := newStack(t)
	rs, cfg := newStackService(t, st, requestKeys{Seerr: "k", Plex: "p", Radarr: "r"}) // no Sonarr key
	snap, err := rs.pull(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snap.Coming.Missing, []string{"shows"}) || st.sonarr.count("") != 0 {
		t.Fatalf("coming %+v, Sonarr asked %d times", snap.Coming, st.sonarr.count(""))
	}
}

func TestComingUpWithOnlyOneOfTheTwoConfigured(t *testing.T) {
	st := newStack(t)
	block := "requests:\n  app: seerr\n  seerr: " + st.seerr.URL + "\n  plex: " + st.plex.URL + "\n  radarr: " + st.radarr.URL + "\n"
	rs, cfg := serviceFor(t, block, requestKeys{Seerr: "k", Plex: "p", Radarr: "r"})
	snap, err := rs.pull(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Coming == nil || len(snap.Coming.Missing) != 0 {
		t.Fatalf("a service that isn't configured isn't missing: %+v", snap.Coming)
	}
	if got := comingLines(snap.Coming); !reflect.DeepEqual(got, []string{"Cinema Movie|2026|movie|2026-10-20|cinemas|0|0", "Future Movie|2026|movie|2026-11-12|home|0|0"}) {
		t.Fatalf("got %v", got)
	}
	if st.sonarr.count("") != 0 {
		t.Error("Sonarr isn't in anchor.yaml but was called")
	}
}

func TestNoComingUpWithoutSonarrOrRadarr(t *testing.T) {
	st := newStack(t)
	block := "requests:\n  app: seerr\n  seerr: " + st.seerr.URL + "\n  plex: " + st.plex.URL + "\n"
	rs, cfg := serviceFor(t, block, requestKeys{Seerr: "k", Plex: "p"})
	snap, err := rs.pull(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Coming != nil {
		t.Fatalf("there is nothing to build it from: %+v", snap.Coming)
	}
}

func TestPullKeepsAtMostFortyUniqueTitles(t *testing.T) {
	var results []string
	routes := map[string]string{}
	for i := 1; i <= 70; i++ {
		results = append(results, `{"status":2,"type":"movie","createdAt":"2026-10-01T00:00:00Z","media":{"tmdbId":`+strconv.Itoa(i)+`,"status":1}}`)
		routes["/api/v1/movie/"+strconv.Itoa(i)] = `{"title":"Movie ` + strconv.Itoa(i) + `"}`
	}
	routes["/api/v1/request"] = `{"results":[` + strings.Join(results, ",") + `]}`
	g := &gauge{}
	seerr := newStackSrv(t, g, routes)
	plex := newStackSrv(t, g, map[string]string{
		"/library/sections":       `{"MediaContainer":{"Directory":[{"key":"1","type":"movie"}]}}`,
		"/library/sections/1/all": plexLibrary(),
	})
	store := storeFor(t, requestsBase+"requests: {app: seerr, seerr: '"+seerr.URL+"', plex: '"+plex.URL+"'}\n")
	cfg, _ := store.Get()
	rs := newRequestStatus(store, requestKeys{Seerr: "k", Plex: "p"}, true)
	rs.now = func() time.Time { return t0 }
	snap, err := rs.pull(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Rows) != maxRows {
		t.Fatalf("%d rows, want %d", len(snap.Rows), maxRows)
	}
	if snap.Rows[0].Title != "Movie 1" || snap.Rows[maxRows-1].Title != "Movie 40" {
		t.Fatalf("kept %q..%q", snap.Rows[0].Title, snap.Rows[maxRows-1].Title)
	}
	if n := seerr.count("/api/v1/movie/"); n != maxRows {
		t.Fatalf("looked up %d titles, want %d", n, maxRows)
	}
	if got := seerr.count("/api/v1/request?"); got != 1 || !strings.Contains(firstKey(seerr.hits), "take=60") {
		t.Fatalf("list calls %d: %v", got, seerr.hits)
	}
}

func firstKey(m map[string]int) string {
	for k := range m {
		if strings.HasPrefix(k, "/api/v1/request") {
			return k
		}
	}
	return ""
}

func TestMergeRequests(t *testing.T) {
	newer, older := t0, t0.Add(-time.Hour)
	in := []seerrRequest{
		{Kind: "show", State: reqPending, TMDB: 5, TVDB: 0, Seasons: []int{3}, Created: newer},
		{Kind: "movie", State: reqApproved, TMDB: 5, Created: newer},
		{Kind: "show", State: reqApproved, TMDB: 5, TVDB: 50, Seasons: []int{1, 3}, Created: older},
		{Kind: "movie", State: reqDeclined, TMDB: 6, Created: older},
		{Kind: "movie", State: reqPending, TMDB: 6, Created: older},
	}
	got := mergeRequests(in)
	if len(got) != 3 {
		t.Fatalf("got %d: %+v", len(got), got)
	}
	show := got[0]
	if show.Kind != "show" || show.TVDB != 50 || !reflect.DeepEqual(show.Seasons, []int{1, 3}) ||
		show.State != reqApproved || !show.Created.Equal(newer) {
		t.Errorf("merged show: %+v", show)
	}
	if got[1].Kind != "movie" || got[1].TMDB != 5 {
		t.Errorf("a movie and a show with the same TMDB id are different titles: %+v", got[1])
	}
	if got[2].State != reqPending {
		t.Errorf("pending beats declined: %+v", got[2])
	}
}

// ---------- the shared snapshot ----------

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type fakeBuild struct {
	calls atomic.Int32
	fail  atomic.Bool
	hold  chan struct{} // when set, builds wait for it to close
}

func (f *fakeBuild) build(ctx context.Context, cfg *Config) (*snapshot, error) {
	f.calls.Add(1)
	if f.hold != nil {
		<-f.hold
	}
	if f.fail.Load() {
		return nil, errors.New("Seerr could not be reached: nope")
	}
	return &snapshot{Rows: []requestRow{{Title: "Built #" + strconv.Itoa(int(f.calls.Load())), Kind: "movie", State: "ready"}}, PlexOK: true}, nil
}

func newFakeService(t *testing.T) (*requestStatus, *fakeBuild, *clock, *Config) {
	t.Helper()
	store := storeFor(t, requestsBase+"requests: {app: seerr, seerr: 'http://s:1', plex: 'http://p:1'}\n")
	cfg, _ := store.Get()
	ck := &clock{t: t0}
	fb := &fakeBuild{}
	rs := newRequestStatus(store, requestKeys{}, false)
	rs.now, rs.build = ck.Now, fb.build
	rs.forced.now = ck.Now
	return rs, fb, ck, cfg
}

func TestReadsAreServedFromTheSnapshotFor5Minutes(t *testing.T) {
	rs, fb, ck, cfg := newFakeService(t)
	ctx := context.Background()
	r := rs.get(ctx, cfg)
	if r.err != nil || r.stale || r.coolDown || r.snap == nil || !r.snap.Taken.Equal(t0) {
		t.Fatalf("first read: %+v", r)
	}
	ck.advance(4*time.Minute + 59*time.Second)
	rs.get(ctx, cfg)
	if fb.calls.Load() != 1 {
		t.Fatalf("rebuilt inside 5 minutes (%d builds)", fb.calls.Load())
	}
	ck.advance(2 * time.Second)
	r = rs.get(ctx, cfg)
	if fb.calls.Load() != 2 || r.snap.Rows[0].Title != "Built #2" {
		t.Fatalf("after 5 minutes: %d builds, %+v", fb.calls.Load(), r.snap.Rows)
	}
}

func TestManyReadersShareOneBuild(t *testing.T) {
	rs, fb, _, cfg := newFakeService(t)
	fb.hold = make(chan struct{})
	var wg sync.WaitGroup
	results := make([]requestResult, 20)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = rs.get(context.Background(), cfg)
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(fb.hold)
	wg.Wait()
	if n := fb.calls.Load(); n != 1 {
		t.Fatalf("%d builds for 20 readers", n)
	}
	for i, r := range results {
		if r.err != nil || r.snap == nil || r.snap.Rows[0].Title != "Built #1" {
			t.Fatalf("reader %d: %+v", i, r)
		}
	}
}

func TestACancelledReaderStopsWaitingButTheBuildFinishes(t *testing.T) {
	rs, fb, _, cfg := newFakeService(t)
	fb.hold = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan requestResult, 1)
	go func() { done <- rs.get(ctx, cfg) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case r := <-done:
		if r.err == nil {
			t.Fatal("a cancelled reader should get an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the cancelled reader kept waiting")
	}
	close(fb.hold)
	r := rs.get(context.Background(), cfg)
	if r.err != nil || r.snap == nil || fb.calls.Load() != 1 {
		t.Fatalf("the build should have finished for others: %+v, %d builds", r, fb.calls.Load())
	}
}

func TestRefreshNowIsLimited(t *testing.T) {
	rs, fb, ck, cfg := newFakeService(t)
	ctx := context.Background()
	rs.get(ctx, cfg) // 1 build

	ck.advance(10 * time.Second)
	r := rs.force(ctx, cfg)
	if !r.coolDown || r.snap == nil || fb.calls.Load() != 1 {
		t.Fatalf("10s after a build: %+v, %d builds", r, fb.calls.Load())
	}
	ck.advance(25 * time.Second) // 35s after the build
	r = rs.force(ctx, cfg)
	if r.coolDown || fb.calls.Load() != 2 || r.snap.Rows[0].Title != "Built #2" {
		t.Fatalf("35s after a build: %+v, %d builds", r, fb.calls.Load())
	}
	// Someone else's read just rebuilt it: a tap right after is "already up to date".
	ck.advance(6 * time.Minute)
	rs.get(ctx, cfg)
	ck.advance(5 * time.Second)
	if r = rs.force(ctx, cfg); !r.coolDown {
		t.Fatalf("a tap right after a read's rebuild should be a cool-down: %+v", r)
	}
}

func TestRefreshNowHasAnHourlyCap(t *testing.T) {
	rs, fb, ck, cfg := newFakeService(t)
	ctx := context.Background()
	rs.get(ctx, cfg)
	for i := 0; i < forcedPerHour; i++ {
		ck.advance(31 * time.Second)
		if r := rs.force(ctx, cfg); r.coolDown {
			t.Fatalf("forced refresh %d was refused", i+1)
		}
	}
	builds := fb.calls.Load()
	ck.advance(31 * time.Second)
	if r := rs.force(ctx, cfg); !r.coolDown {
		t.Fatal("the hourly cap did not hold")
	}
	if fb.calls.Load() != builds {
		t.Fatal("a refused refresh still built")
	}
	// An hour on, it works again.
	ck.advance(time.Hour)
	if r := rs.force(ctx, cfg); r.coolDown {
		t.Fatalf("still refused an hour later: %+v", r)
	}
}

func TestAFailedBuildKeepsTheOldSnapshotAndBacksOff(t *testing.T) {
	rs, fb, ck, cfg := newFakeService(t)
	ctx := context.Background()

	// Never worked: an error, and no hammering.
	fb.fail.Store(true)
	if r := rs.get(ctx, cfg); r.err == nil || r.snap != nil {
		t.Fatalf("first failure: %+v", r)
	}
	ck.advance(30 * time.Second)
	rs.get(ctx, cfg)
	if fb.calls.Load() != 1 {
		t.Fatalf("retried inside the back-off (%d builds)", fb.calls.Load())
	}
	ck.advance(31 * time.Second)
	fb.fail.Store(false)
	if r := rs.get(ctx, cfg); r.err != nil || r.snap == nil || r.stale || fb.calls.Load() != 2 {
		t.Fatalf("recovery: %+v, %d builds", r, fb.calls.Load())
	}

	// Worked, then broke: the old rows stay, marked stale.
	ck.advance(6 * time.Minute)
	fb.fail.Store(true)
	r := rs.get(ctx, cfg)
	if r.err != nil || !r.stale || r.snap == nil || r.snap.Rows[0].Title != "Built #2" || fb.calls.Load() != 3 {
		t.Fatalf("after a failure: %+v, %d builds", r, fb.calls.Load())
	}
	// No new attempt for 60 seconds, from a read or from Refresh now.
	ck.advance(40 * time.Second)
	rs.get(ctx, cfg)
	if r = rs.force(ctx, cfg); !r.coolDown || !r.stale {
		t.Fatalf("forced refresh inside the back-off: %+v", r)
	}
	if fb.calls.Load() != 3 {
		t.Fatalf("tried again inside the back-off (%d builds)", fb.calls.Load())
	}
	ck.advance(25 * time.Second)
	fb.fail.Store(false)
	if r = rs.get(ctx, cfg); r.stale || fb.calls.Load() != 4 || r.snap.Rows[0].Title != "Built #4" {
		t.Fatalf("after the back-off: %+v, %d builds", r, fb.calls.Load())
	}
}

func TestChangingTheSettingsDropsTheOldSnapshot(t *testing.T) {
	rs, fb, _, cfg := newFakeService(t)
	ctx := context.Background()
	rs.get(ctx, cfg)

	moved := *cfg
	r2 := *cfg.Requests
	r2.Plex = "http://elsewhere:32400"
	moved.Requests = &r2
	r := rs.get(ctx, &moved)
	if fb.calls.Load() != 2 || r.snap == nil || r.snap.sig != r2.sig() {
		t.Fatalf("new settings should be used at once: %d builds, %+v", fb.calls.Load(), r)
	}

	// And if the new settings fail, the old ones' rows are not shown.
	r3 := r2
	r3.Seerr = "http://another:5055"
	moved2 := moved
	moved2.Requests = &r3
	fb.fail.Store(true)
	if r := rs.get(ctx, &moved2); r.snap != nil || r.err == nil {
		t.Fatalf("rows from other settings were served: %+v", r)
	}
}

func TestAPanicWhileBuildingIsAnErrorNotACrash(t *testing.T) {
	rs, _, _, cfg := newFakeService(t)
	rs.build = func(context.Context, *Config) (*snapshot, error) { panic("surprise") }
	r := rs.get(context.Background(), cfg)
	if r.err == nil || r.snap != nil {
		t.Fatalf("got %+v", r)
	}
}

func TestTheWorkerPoolSurvivesAPanicingJob(t *testing.T) {
	var done atomic.Int32
	panicked := parallelDo(10, 4, func(i int) {
		if i == 3 {
			panic("surprise")
		}
		done.Add(1)
	})
	if done.Load() != 9 {
		t.Fatalf("%d of 9 other jobs ran", done.Load())
	}
	if !reflect.DeepEqual(panicked, []int{3}) {
		t.Fatalf("panicked jobs: %v", panicked)
	}
}

func TestMissingKeysNamesOnlyWhatIsNeeded(t *testing.T) {
	rc := &Requests{Seerr: "http://s", Plex: "http://p"}
	if got := missingKeys(rc, requestKeys{}); !reflect.DeepEqual(got, []string{"ANCHOR_SEERR_KEY", "ANCHOR_PLEX_TOKEN"}) {
		t.Errorf("seerr and plex only: %v", got)
	}
	rc.Sonarr, rc.Radarr = "http://so", "http://ra"
	if got := missingKeys(rc, requestKeys{}); len(got) != 4 {
		t.Errorf("all four: %v", got)
	}
	if got := missingKeys(rc, stackKeys); got != nil {
		t.Errorf("nothing missing: %v", got)
	}
	rc.Sonarr, rc.Radarr = "", ""
	if got := missingKeys(rc, requestKeys{Seerr: "k", Plex: "p"}); got != nil {
		t.Errorf("Sonarr and Radarr keys aren't needed when they aren't configured: %v", got)
	}
}
