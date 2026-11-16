package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAPI serves canned JSON by path and remembers what it was asked.
type fakeAPI struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []string // "path?query" in the order asked
	keys []string // the key header of each request
}

func newFakeAPI(t *testing.T, keyHeader string, routes map[string]string) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, r.URL.RequestURI())
		f.keys = append(f.keys, r.Header.Get(keyHeader))
		f.mu.Unlock()
		body, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

var devClient = newUpstreamClient(true)

// ---------- flexInt ----------

func TestFlexIntAcceptsNumbersStringsAndJunk(t *testing.T) {
	var out struct {
		A, B, C, D, E, F flexInt
	}
	in := `{"A": 5, "B": "5", "C": null, "D": "", "E": "x", "F": 7.0}`
	if err := json.Unmarshal([]byte(in), &out); err != nil {
		t.Fatal(err)
	}
	got := []int{int(out.A), int(out.B), int(out.C), int(out.D), int(out.E), int(out.F)}
	if !reflect.DeepEqual(got, []int{5, 5, 0, 0, 0, 7}) {
		t.Fatalf("got %v", got)
	}
}

// ---------- Seerr ----------

const seerrRequests = `{"pageInfo":{"pages":1,"pageSize":60,"results":8,"page":1},"results":[
 {"id":1,"status":2,"type":"movie","createdAt":"2026-10-02T10:00:00.000Z","is4k":false,
  "media":{"id":5,"mediaType":"movie","tmdbId":693134,"tvdbId":null,"status":3},"seasons":[],
  "requestedBy":{"id":3,"displayName":"plex family"}},
 {"id":2,"status":1,"type":"tv","createdAt":"2026-10-01T09:30:00.000Z",
  "media":{"mediaType":"tv","tmdbId":95396,"tvdbId":371980,"status":2},
  "seasons":[{"id":1,"seasonNumber":2,"status":1},{"id":2,"seasonNumber":0,"status":1}]},
 {"id":3,"status":3,"type":"movie","createdAt":"2026-09-30T00:00:00.000Z","media":{"tmdbId":11,"status":1}},
 {"id":4,"status":4,"type":"movie","createdAt":"2026-09-29T00:00:00.000Z","media":{"tmdbId":12,"status":2}},
 {"id":5,"status":5,"createdAt":"2026-09-28T00:00:00.000Z","media":{"mediaType":"tv","tmdbId":13,"tvdbId":"130","status":"5"},"seasons":[{"seasonNumber":1}]},
 {"id":6,"status":2,"type":"music","media":{"tmdbId":14}},
 {"id":7,"status":2,"type":"movie","media":{"tmdbId":0}},
 {"id":8,"status":2,"type":"movie","createdAt":"not a date","media":{"tmdbId":15,"status":1}}
]}`

func TestSeerrRequests(t *testing.T) {
	api := newFakeAPI(t, "X-Api-Key", map[string]string{"/api/v1/request": seerrRequests})
	s, err := newSeerr(api.srv.URL, "k1", devClient)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.requests(context.Background(), 60)
	if err != nil {
		t.Fatal(err)
	}
	if q := api.asked(); len(q) != 1 || q[0] != "/api/v1/request?skip=0&sort=added&take=60" {
		t.Fatalf("asked %v", q)
	}
	if api.keys[0] != "k1" {
		t.Fatalf("key header %q", api.keys[0])
	}

	created := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	want := []seerrRequest{
		{Kind: "movie", State: reqApproved, MediaStatus: 3, TMDB: 693134, Created: created("2026-10-02T10:00:00Z")},
		{Kind: "show", State: reqPending, MediaStatus: 2, TMDB: 95396, TVDB: 371980, Seasons: []int{2}, Created: created("2026-10-01T09:30:00Z")},
		{Kind: "movie", State: reqDeclined, MediaStatus: 1, TMDB: 11, Created: created("2026-09-30T00:00:00Z")},
		{Kind: "movie", State: reqFailed, MediaStatus: 2, TMDB: 12, Created: created("2026-09-29T00:00:00Z")},
		// no "type": falls back to media.mediaType; completed counts as approved; numbers as strings are fine
		{Kind: "show", State: reqApproved, MediaStatus: 5, TMDB: 13, TVDB: 130, Seasons: []int{1}, Created: created("2026-09-28T00:00:00Z")},
		// a bad date is not fatal
		{Kind: "movie", State: reqApproved, MediaStatus: 1, TMDB: 15},
	}
	// music and tmdbId 0 are dropped
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got\n%+v\nwant\n%+v", got, want)
	}
}

func TestSeerrTitle(t *testing.T) {
	api := newFakeAPI(t, "X-Api-Key", map[string]string{
		"/api/v1/movie/693134": `{"id":693134,"title":"Dune: Part Two","releaseDate":"2024-02-27","credits":{"cast":[]}}`,
		"/api/v1/tv/95396":     `{"id":95396,"name":"Severance‮\n","firstAirDate":"2022-02-17"}`,
		"/api/v1/movie/9":      `{"title":"No Date","releaseDate":""}`,
		"/api/v1/movie/8":      `{"title":"Odd Date","releaseDate":"0000-00-00"}`,
	})
	s, _ := newSeerr(api.srv.URL, "k", devClient)
	ctx := context.Background()

	title, year, err := s.title(ctx, "movie", 693134)
	if err != nil || title != "Dune: Part Two" || year != 2024 {
		t.Fatalf("movie: %q %d %v", title, year, err)
	}
	title, year, err = s.title(ctx, "show", 95396)
	if err != nil || title != "Severance" || year != 2022 {
		t.Fatalf("show: %q %d %v (the title should be cleaned)", title, year, err)
	}
	if _, year, _ := s.title(ctx, "movie", 9); year != 0 {
		t.Errorf("no date should give year 0, got %d", year)
	}
	if _, year, _ := s.title(ctx, "movie", 8); year != 0 {
		t.Errorf("an odd date should give year 0, got %d", year)
	}

	before := len(api.asked())
	for _, c := range []struct {
		kind string
		id   int
	}{{"movie", 0}, {"movie", -4}, {"person", 5}, {"", 5}, {"movie", 1 << 40}} {
		if _, _, err := s.title(ctx, c.kind, c.id); err == nil {
			t.Errorf("%+v should be refused", c)
		}
	}
	if len(api.asked()) != before {
		t.Error("refused lookups still reached Seerr")
	}
}

// ---------- Plex ----------

func plexLibrary(items ...string) string {
	return `{"MediaContainer":{"size":` + strconv.Itoa(len(items)) + `,"totalSize":` + strconv.Itoa(len(items)) +
		`,"Metadata":[` + strings.Join(items, ",") + `]}}`
}

func TestPlexIndex(t *testing.T) {
	api := newFakeAPI(t, "X-Plex-Token", map[string]string{
		"/library/sections": `{"MediaContainer":{"Directory":[
			{"key":"1","type":"movie","title":"Movies"},
			{"key":"2","type":"show","title":"TV"},
			{"key":"3","type":"artist","title":"Music"},
			{"key":"x/../9","type":"movie","title":"Evil"}]}}`,
		"/library/sections/1/all": plexLibrary(
			`{"ratingKey":"10","guid":"plex://movie/abc","Guid":[{"id":"imdb://tt1"},{"id":"tmdb://693134"},{"id":"tvdb://5"}]}`,
			`{"ratingKey":"11","guid":"com.plexapp.agents.themoviedb://603?lang=en"}`,
			`{"ratingKey":"12","guid":"com.plexapp.agents.imdb://tt0000001?lang=en"}`,
			`{"ratingKey":"13","Guid":[{"id":"tmdb://notanumber"},{"id":"tmdb://0"}]}`),
		"/library/sections/2/all": plexLibrary(
			`{"ratingKey":"20","Guid":[{"id":"tvdb://371980"},{"id":"tmdb://95396"}],"leafCount":10}`,
			`{"ratingKey":"21","guid":"com.plexapp.agents.thetvdb://121361?lang=en"}`,
			`{"ratingKey":"22","Guid":[{"id":"tmdb://777"}]}`),
	})
	p, err := newPlex(api.srv.URL, "tok", devClient)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := p.index(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !idx.movies[693134] || !idx.movies[603] || len(idx.movies) != 2 {
		t.Errorf("movies: %v", idx.movies)
	}
	// Plex's own number for each movie, which is what its poster is fetched by.
	if idx.movieKeys[693134] != "10" || idx.movieKeys[603] != "11" || len(idx.movieKeys) != 2 {
		t.Errorf("movie rating keys: %v", idx.movieKeys)
	}
	if idx.showsTVDB[371980] != "20" || idx.showsTVDB[121361] != "21" || len(idx.showsTVDB) != 2 {
		t.Errorf("shows by tvdb: %v", idx.showsTVDB)
	}
	if idx.showsTMDB[95396] != "20" || idx.showsTMDB[777] != "22" || len(idx.showsTMDB) != 2 {
		t.Errorf("shows by tmdb: %v", idx.showsTMDB)
	}
	for _, q := range api.asked() {
		if strings.Contains(q, "/3/") || strings.Contains(q, "..") {
			t.Errorf("asked for a section it should have skipped: %s", q)
		}
		if strings.Contains(q, "tok") {
			t.Errorf("token in the URL: %s", q)
		}
	}
	for _, k := range api.keys {
		if k != "tok" {
			t.Errorf("token header %q", k)
		}
	}
}

func TestPlexIndexPages(t *testing.T) {
	var mu sync.Mutex
	var starts []string
	items := make([]string, 5)
	for i := range items {
		items[i] = `{"ratingKey":"` + strconv.Itoa(100+i) + `","Guid":[{"id":"tmdb://` + strconv.Itoa(1000+i) + `"}]}`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/sections":
			w.Write([]byte(`{"MediaContainer":{"Directory":[{"key":"1","type":"movie"}]}}`))
		case "/library/sections/1/all":
			q := r.URL.Query()
			start, _ := strconv.Atoi(q.Get("X-Plex-Container-Start"))
			size, _ := strconv.Atoi(q.Get("X-Plex-Container-Size"))
			mu.Lock()
			starts = append(starts, q.Get("X-Plex-Container-Start")+"+"+q.Get("X-Plex-Container-Size"))
			mu.Unlock()
			end := min(start+size, len(items))
			w.Write([]byte(`{"MediaContainer":{"totalSize":5,"Metadata":[` + strings.Join(items[start:end], ",") + `]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p, _ := newPlex(srv.URL, "tok", devClient)
	p.pageSize = 2
	idx, err := p.index(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.movies) != 5 {
		t.Fatalf("read %d of 5 items: %v (pages %v)", len(idx.movies), idx.movies, starts)
	}
	if !reflect.DeepEqual(starts, []string{"0+2", "2+2", "4+2"}) {
		t.Fatalf("pages asked: %v", starts)
	}
}

func TestPlexIndexStopsAtTheLastPage(t *testing.T) {
	// A server that never says it is done must not keep Anchor busy forever.
	var n int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/library/sections" {
			w.Write([]byte(`{"MediaContainer":{"Directory":[{"key":"1","type":"movie"}]}}`))
			return
		}
		mu.Lock()
		n++
		mu.Unlock()
		w.Write([]byte(`{"MediaContainer":{"totalSize":99999999,"Metadata":[{"ratingKey":"1","Guid":[{"id":"tmdb://1"}]},{"ratingKey":"2","Guid":[{"id":"tmdb://2"}]}]}}`))
	}))
	defer srv.Close()
	p, _ := newPlex(srv.URL, "tok", devClient)
	p.pageSize = 2
	if _, err := p.index(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n > maxPlexPages {
		t.Fatalf("asked for %d pages, limit is %d", n, maxPlexPages)
	}
}

func TestPlexSeasons(t *testing.T) {
	api := newFakeAPI(t, "X-Plex-Token", map[string]string{
		"/library/metadata/20/children": `{"MediaContainer":{"Metadata":[
			{"index":0,"leafCount":2,"type":"season"},
			{"index":1,"leafCount":10,"type":"season"},
			{"index":2,"leafCount":"4","type":"season"}]}}`,
	})
	p, _ := newPlex(api.srv.URL, "tok", devClient)
	got, err := p.seasons(context.Background(), "20")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, map[int]int{0: 2, 1: 10, 2: 4}) {
		t.Fatalf("got %v", got)
	}
	before := len(api.asked())
	for _, key := range []string{"", "abc", "20/../..", "20?x=1", "1234567890123"} {
		if _, err := p.seasons(context.Background(), key); err == nil {
			t.Errorf("rating key %q should be refused", key)
		}
	}
	if len(api.asked()) != before {
		t.Error("refused keys still reached Plex")
	}
}

// ---------- Sonarr ----------

var (
	now     = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	eastern = time.FixedZone("EST", -5*3600)
	sonarrQ = `{"page":1,"pageSize":500,"totalRecords":4,"records":[
		{"seriesId":9,"status":"downloading","trackedDownloadStatus":"ok","trackedDownloadState":"downloading"},
		{"seriesId":9,"status":"queued","trackedDownloadStatus":"ok"},
		{"seriesId":4,"status":"downloading","trackedDownloadStatus":"warning","trackedDownloadState":"downloading"},
		{"seriesId":5,"status":"completed","trackedDownloadStatus":"ok","trackedDownloadState":"importBlocked"},
		{"seriesId":6,"status":"queued","trackedDownloadStatus":"ok"},
		{"seriesId":7,"status":"failed"},
		{"seriesId":8,"status":"paused"}]}`
	sonarrSeries = `[{"id":9,"title":"Severance",
		"seasons":[
		 {"seasonNumber":0,"monitored":false,"statistics":{"episodeFileCount":0,"episodeCount":0}},
		 {"seasonNumber":1,"monitored":true,"statistics":{"episodeFileCount":8,"episodeCount":8,"previousAiring":"2025-05-01T01:00:00Z"}},
		 {"seasonNumber":2,"monitored":true,"statistics":{"episodeFileCount":2,"episodeCount":4,"previousAiring":"2026-10-05T01:00:00Z","nextAiring":"2026-10-12T01:00:00Z"}},
		 {"seasonNumber":3,"monitored":true,"statistics":{"episodeFileCount":0,"episodeCount":0,"nextAiring":"2027-01-10T01:00:00Z"}}]}]`
)

func TestSonarrQueue(t *testing.T) {
	api := newFakeAPI(t, "X-Api-Key", map[string]string{"/api/v3/queue": sonarrQ})
	s, err := newSonarr(api.srv.URL, "k", devClient)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.queue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if q := api.asked(); len(q) != 1 || q[0] != "/api/v3/queue?page=1&pageSize=500" {
		t.Fatalf("asked %v", q)
	}
	want := map[int]queueState{
		9: {Downloading: true},
		4: {Problem: true}, // warning: not counted as downloading
		5: {Problem: true}, // downloaded but the import is blocked
		7: {Problem: true},
		// 6 queued and 8 paused are waiting, neither downloading nor failing
		6: {},
		8: {},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestSonarrFacts(t *testing.T) {
	api := newFakeAPI(t, "X-Api-Key", map[string]string{"/api/v3/series": sonarrSeries})
	s, _ := newSonarr(api.srv.URL, "k", devClient)
	ctx := context.Background()
	queue := map[int]queueState{9: {Downloading: true}}

	got, err := s.facts(ctx, 371980, []int{2}, queue, eastern, now)
	if err != nil {
		t.Fatal(err)
	}
	if q := api.asked(); len(q) != 1 || q[0] != "/api/v3/series?tvdbId=371980" {
		t.Fatalf("asked %v", q)
	}
	prev, _ := time.Parse(time.RFC3339, "2026-10-05T01:00:00Z")
	want := &arrFacts{
		Aired: 4, Files: 2, Available: true, Downloading: true, Upcoming: true, Since: prev,
		Expected: "2026-10-11", // 01:00 UTC on the 12th is 8pm on the 11th in New York
		Series:   9,            // Sonarr's own id for the show, so Requests and Coming up can tell it's the same one
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("season 2:\n got %+v\nwant %+v", got, want)
	}

	// No seasons named: every season above 0 counts, and the earliest next date wins.
	got, _ = s.facts(ctx, 371980, nil, nil, eastern, now)
	if got.Aired != 12 || got.Files != 10 || got.Expected != "2026-10-11" || !got.Upcoming || got.Downloading {
		t.Fatalf("all seasons: %+v", got)
	}

	// Season 3 only: nothing aired yet, a date ahead.
	got, _ = s.facts(ctx, 371980, []int{3}, nil, eastern, now)
	if got.Aired != 0 || got.Available || got.Expected != "2027-01-09" || !got.Upcoming || !got.Since.IsZero() {
		t.Fatalf("season 3: %+v", got)
	}

	// A season that doesn't exist: nothing to report, but not an error.
	got, _ = s.facts(ctx, 371980, []int{9}, nil, eastern, now)
	if got == nil || got.Aired != 0 || got.Upcoming {
		t.Fatalf("unknown season: %+v", got)
	}
}

func TestSonarrFactsIgnoreAStaleNextDate(t *testing.T) {
	api := newFakeAPI(t, "X-Api-Key", map[string]string{"/api/v3/series": `[{"id":1,"seasons":[
		{"seasonNumber":1,"statistics":{"episodeFileCount":1,"episodeCount":1,"nextAiring":"2020-01-01T00:00:00Z"}}]}]`})
	s, _ := newSonarr(api.srv.URL, "k", devClient)
	got, _ := s.facts(context.Background(), 1, nil, nil, time.UTC, now)
	if got.Upcoming || got.Expected != "" {
		t.Fatalf("a date in the past is not upcoming: %+v", got)
	}
}

func TestSonarrFactsFallBackToSeriesTotals(t *testing.T) {
	api := newFakeAPI(t, "X-Api-Key", map[string]string{"/api/v3/series": `[{"id":3,
		"nextAiring":"2026-10-20T00:00:00Z","previousAiring":"2026-10-01T00:00:00Z",
		"statistics":{"episodeFileCount":5,"episodeCount":6}}]`})
	s, _ := newSonarr(api.srv.URL, "k", devClient)
	got, err := s.facts(context.Background(), 5, []int{1}, nil, time.UTC, now)
	if err != nil || got == nil || got.Aired != 6 || got.Files != 5 || got.Expected != "2026-10-20" || !got.Upcoming {
		t.Fatalf("got %+v %v", got, err)
	}
}

func TestSonarrFactsNilWhenNotFound(t *testing.T) {
	api := newFakeAPI(t, "X-Api-Key", map[string]string{"/api/v3/series": `[]`})
	s, _ := newSonarr(api.srv.URL, "k", devClient)
	got, err := s.facts(context.Background(), 5, nil, nil, time.UTC, now)
	if err != nil || got != nil {
		t.Fatalf("got %+v %v", got, err)
	}
	if _, err := s.facts(context.Background(), 0, nil, nil, time.UTC, now); err == nil {
		t.Error("tvdb id 0 should be refused")
	}
	if len(api.asked()) != 1 {
		t.Errorf("a refused id still reached Sonarr: %v", api.asked())
	}
}

// ---------- Radarr ----------

func radarrMovie(fields string) string { return `[{"id":3,"title":"X",` + fields + `}]` }

func radarrFacts(t *testing.T, body string, queue map[int]queueState) *arrFacts {
	t.Helper()
	api := newFakeAPI(t, "X-Api-Key", map[string]string{"/api/v3/movie": body})
	r, err := newRadarr(api.srv.URL, "k", devClient)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.facts(context.Background(), 693134, queue, eastern, now)
	if err != nil {
		t.Fatal(err)
	}
	if q := api.asked(); len(q) != 1 || q[0] != "/api/v3/movie?tmdbId=693134" {
		t.Fatalf("asked %v", q)
	}
	return got
}

func TestRadarrFactsNotOutYet(t *testing.T) {
	got := radarrFacts(t, radarrMovie(`"hasFile":false,"isAvailable":false,"minimumAvailability":"released",
		"inCinemas":"2026-09-01T00:00:00Z","digitalRelease":"2026-11-12T00:00:00Z","physicalRelease":"2026-12-01T00:00:00Z"`), nil)
	// Calendar dates are shown as given: the 12th, not the evening of the 11th.
	want := &arrFacts{Available: false, Expected: "2026-11-12", Upcoming: true, Movie: 3} // 3 is Radarr's own id for it
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestRadarrFactsUsesTheDateRadarrActsOn(t *testing.T) {
	// "In Cinemas": Radarr starts looking at the theatrical date.
	got := radarrFacts(t, radarrMovie(`"isAvailable":false,"minimumAvailability":"inCinemas",
		"inCinemas":"2026-10-20T00:00:00Z","digitalRelease":"2026-12-12T00:00:00Z"`), nil)
	if got.Expected != "2026-10-20" || !got.Upcoming {
		t.Errorf("inCinemas: %+v", got)
	}
	// "In Cinemas" with no theatrical date falls back to the home release.
	got = radarrFacts(t, radarrMovie(`"isAvailable":false,"minimumAvailability":"inCinemas","physicalRelease":"2026-12-01T00:00:00Z"`), nil)
	if got.Expected != "2026-12-01" {
		t.Errorf("inCinemas fallback: %+v", got)
	}
	// "Announced": Radarr looks right away, so there is no date to wait for.
	got = radarrFacts(t, radarrMovie(`"isAvailable":true,"minimumAvailability":"announced","inCinemas":"2027-01-01T00:00:00Z"`), nil)
	if got.Expected != "" || got.Upcoming || !got.Available {
		t.Errorf("announced: %+v", got)
	}
	// "Released" with the earlier of digital and physical.
	got = radarrFacts(t, radarrMovie(`"isAvailable":false,"minimumAvailability":"released",
		"digitalRelease":"2026-12-12T00:00:00Z","physicalRelease":"2026-11-30T00:00:00Z"`), nil)
	if got.Expected != "2026-11-30" {
		t.Errorf("earliest home release: %+v", got)
	}
	// Only a theatrical date and "Released": no date to promise.
	got = radarrFacts(t, radarrMovie(`"isAvailable":false,"minimumAvailability":"released","inCinemas":"2026-10-20T00:00:00Z"`), nil)
	if got.Expected != "" || got.Available {
		t.Errorf("theatrical only: %+v", got)
	}
}

func TestRadarrFactsOutAlready(t *testing.T) {
	got := radarrFacts(t, radarrMovie(`"hasFile":true,"isAvailable":true,"minimumAvailability":"released","digitalRelease":"2026-09-01T00:00:00Z"`),
		map[int]queueState{3: {Problem: true}})
	since, _ := time.Parse(time.RFC3339, "2026-09-01T00:00:00Z")
	want := &arrFacts{HasFile: true, Available: true, Expected: "2026-09-01", Since: since, QueueProblem: true, Movie: 3}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	got = radarrFacts(t, radarrMovie(`"isAvailable":true,"minimumAvailability":"released","digitalRelease":"2026-09-01T00:00:00Z"`),
		map[int]queueState{3: {Downloading: true}})
	if !got.Downloading || got.HasFile {
		t.Errorf("downloading: %+v", got)
	}
}

func TestRadarrFactsWithoutIsAvailable(t *testing.T) {
	// If this Radarr doesn't say, work it out from the date.
	got := radarrFacts(t, radarrMovie(`"minimumAvailability":"released","digitalRelease":"2026-09-01T00:00:00Z"`), nil)
	if !got.Available {
		t.Errorf("a past home release should be available: %+v", got)
	}
	got = radarrFacts(t, radarrMovie(`"minimumAvailability":"released","digitalRelease":"2026-11-01T00:00:00Z"`), nil)
	if got.Available {
		t.Errorf("a future home release should not be: %+v", got)
	}
	got = radarrFacts(t, radarrMovie(`"minimumAvailability":"announced"`), nil)
	if !got.Available {
		t.Errorf("announced is available at once: %+v", got)
	}
}

func TestRadarrFactsAllowsABadDate(t *testing.T) {
	got := radarrFacts(t, radarrMovie(`"isAvailable":false,"minimumAvailability":"released","digitalRelease":"soon-ish"`), nil)
	if got == nil || got.Expected != "" || got.Available {
		t.Errorf("got %+v", got)
	}
}

func TestRadarrNilWhenNotFoundAndQueue(t *testing.T) {
	api := newFakeAPI(t, "X-Api-Key", map[string]string{
		"/api/v3/movie": `[]`,
		"/api/v3/queue": `{"records":[{"movieId":3,"status":"downloading","trackedDownloadStatus":"ok"},{"movieId":4,"status":"failed"}]}`,
	})
	r, _ := newRadarr(api.srv.URL, "k", devClient)
	got, err := r.facts(context.Background(), 5, nil, time.UTC, now)
	if err != nil || got != nil {
		t.Fatalf("got %+v %v", got, err)
	}
	if _, err := r.facts(context.Background(), -1, nil, time.UTC, now); err == nil {
		t.Error("a bad tmdb id should be refused")
	}
	q, err := r.queue(context.Background())
	if err != nil || !reflect.DeepEqual(q, map[int]queueState{3: {Downloading: true}, 4: {Problem: true}}) {
		t.Fatalf("queue %v %v", q, err)
	}
}

func TestSourcesNeedAnAddressAndKey(t *testing.T) {
	for name, build := range map[string]func(string, string) error{
		"seerr":  func(b, k string) error { _, err := newSeerr(b, k, devClient); return err },
		"plex":   func(b, k string) error { _, err := newPlex(b, k, devClient); return err },
		"sonarr": func(b, k string) error { _, err := newSonarr(b, k, devClient); return err },
		"radarr": func(b, k string) error { _, err := newRadarr(b, k, devClient); return err },
	} {
		if build("http://192.168.1.2:80", "k") != nil {
			t.Errorf("%s: a good address and key should work", name)
		}
		if build("http://192.168.1.2:80", "") == nil {
			t.Errorf("%s: a missing key should fail", name)
		}
		if build("file:///etc/passwd", "k") == nil {
			t.Errorf("%s: a bad address should fail", name)
		}
	}
}
