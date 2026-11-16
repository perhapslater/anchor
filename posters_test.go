package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Posters: fetching a cover from Sonarr or Radarr, keeping it for a while, and
// serving it only to people allowed to see the Request status sheet.

var (
	jpegBytes = append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte{7}, 200)...)
	pngBytes  = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{7}, 200)...)
	webpBytes = append([]byte("RIFF\x10\x00\x00\x00WEBPVP8 "), bytes.Repeat([]byte{7}, 200)...)
)

const coverPath = `^/MediaCover/[0-9]{1,9}/poster-250\.jpg$`

// ---------- telling a picture from anything else ----------

func TestImageTypeGoesByTheBytesNotTheLabel(t *testing.T) {
	cases := []struct {
		name string
		b    []byte
		want string
	}{
		{"jpeg", jpegBytes, "image/jpeg"},
		{"png", pngBytes, "image/png"},
		{"webp", webpBytes, "image/webp"},
		{"svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), ""},
		{"html", []byte("<!doctype html><title>x</title>"), ""},
		{"gif", []byte("GIF89a......"), ""},
		{"json", []byte(`{"a":1}`), ""},
		{"empty", nil, ""},
		{"jpeg cut short", []byte{0xFF, 0xD8}, ""},
		{"png with a bad signature", []byte("\x89PNX\r\n\x1a\n...."), ""},
		{"riff that is not webp", []byte("RIFF\x10\x00\x00\x00WAVEfmt "), ""},
	}
	for _, c := range cases {
		if got := imageType(c.b); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// ---------- asking a service for a picture ----------

func coverServer(t *testing.T, h http.HandlerFunc) *upstream {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return devUpstream(t, srv.URL, 1<<10, coverPath)
}

func TestGetImageReturnsThePictureAndWorksOutItsTypeItself(t *testing.T) {
	for name, want := range map[string][]byte{"image/jpeg": jpegBytes, "image/png": pngBytes, "image/webp": webpBytes} {
		u := coverServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html") // the label is not trusted
			w.Write(want)
		})
		data, ctype, err := u.getImage(context.Background(), "/MediaCover/5/poster-250.jpg", nil, 1<<10)
		if err != nil || ctype != name || !bytes.Equal(data, want) {
			t.Errorf("%s: type %q, %d bytes, err %v", name, ctype, len(data), err)
		}
	}
}

func TestGetImageSendsTheKeyInAHeaderAndNothingElse(t *testing.T) {
	var method, key, accept, query, path, cookie string
	u := coverServer(t, func(w http.ResponseWriter, r *http.Request) {
		method, key, accept, query, path, cookie = r.Method, r.Header.Get("X-Api-Key"), r.Header.Get("Accept"), r.URL.RawQuery, r.URL.Path, r.Header.Get("Cookie")
		w.Write(jpegBytes)
	})
	if _, _, err := u.getImage(context.Background(), "/MediaCover/12/poster-250.jpg", nil, 1<<10); err != nil {
		t.Fatal(err)
	}
	if method != "GET" || key != "sekrit" || !strings.HasPrefix(accept, "image/") || query != "" || path != "/MediaCover/12/poster-250.jpg" || cookie != "" {
		t.Errorf("method %q key %q accept %q query %q path %q cookie %q", method, key, accept, query, path, cookie)
	}
}

func TestGetImageRefusesAnythingThatIsNotAPicture(t *testing.T) {
	for name, body := range map[string]string{
		"a web page": "<!doctype html><script>alert(1)</script>",
		"an svg":     `<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`,
		"json":       `{"error":"nope"}`,
		"nothing":    "",
		"a stub":     "\xFF\xD8",
	} {
		u := coverServer(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) })
		data, _, err := u.getImage(context.Background(), "/MediaCover/5/poster-250.jpg", nil, 1<<10)
		if err == nil || data != nil {
			t.Errorf("%s was accepted", name)
			continue
		}
		if strings.Contains(err.Error(), "alert") || strings.Contains(err.Error(), "nope") {
			t.Errorf("%s: the error repeats what the service sent: %v", name, err)
		}
	}
}

func TestGetImageHasASizeLimit(t *testing.T) {
	at := append([]byte{0xFF, 0xD8, 0xFF}, bytes.Repeat([]byte{1}, 297)...) // exactly 300
	over := append(append([]byte(nil), at...), 1)                           // 301
	serve := func(body []byte) *upstream {
		return coverServer(t, func(w http.ResponseWriter, r *http.Request) { w.Write(body) })
	}
	if _, _, err := serve(at).getImage(context.Background(), "/MediaCover/5/poster-250.jpg", nil, 300); err != nil {
		t.Errorf("a picture exactly at the limit: %v", err)
	}
	_, _, err := serve(over).getImage(context.Background(), "/MediaCover/5/poster-250.jpg", nil, 300)
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("a picture over the limit: %v", err)
	}
}

func TestGetImageDoesNotFollowRedirects(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		w.Write(jpegBytes)
	}))
	defer other.Close()
	u := coverServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/MediaCover/5/poster-250.jpg", http.StatusFound)
	})
	_, _, err := u.getImage(context.Background(), "/MediaCover/5/poster-250.jpg", nil, 1<<10)
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("err %v", err)
	}
	if elsewhere.Load() != 0 {
		t.Fatal("Anchor followed the redirect, taking its key along")
	}
}

func TestGetImageOnlyAsksForAddressesOnTheList(t *testing.T) {
	var hits atomic.Int32
	u := coverServer(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.Write(jpegBytes) })
	for _, p := range []string{
		"/MediaCover/5/poster.jpg", "/MediaCover/5/poster-250.jpg?x=1", "/MediaCover/5/../../api/v3/system/status",
		"/MediaCover/5/poster-250.png", "/MediaCover//poster-250.jpg", "/MediaCover/5%2f/poster-250.jpg", "/api/v3/series", "poster-250.jpg", "",
	} {
		if _, _, err := u.getImage(context.Background(), p, nil, 1<<10); err == nil {
			t.Errorf("%q was asked for", p)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("the service saw %d requests", hits.Load())
	}
}

func TestGetImageWhenTheServiceSaysNo(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusUnauthorized, http.StatusInternalServerError} {
		u := coverServer(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "secret detail", code) })
		_, _, err := u.getImage(context.Background(), "/MediaCover/5/poster-250.jpg", nil, 1<<10)
		if err == nil || !strings.Contains(err.Error(), strconv.Itoa(code)) || strings.Contains(err.Error(), "secret detail") {
			t.Errorf("%d: %v", code, err)
		}
	}
}

// ---------- the two clients ----------

// Sonarr and Radarr have two addresses for a cover: the API's, which is meant
// for an API key, and the one their own web page uses, which may want a login.
// Anchor asks the API's first.
func TestPosterClientsAskForTheCoverAtTheAPIAddressFirst(t *testing.T) {
	var path, key string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, key = r.URL.Path, r.Header.Get("X-Api-Key")
		w.Write(jpegBytes)
	}))
	defer srv.Close()
	client := newUpstreamClient(true)
	sonarr, err := newSonarr(srv.URL, "sonarr-key", client)
	if err != nil {
		t.Fatal(err)
	}
	radarr, err := newRadarr(srv.URL, "radarr-key", client)
	if err != nil {
		t.Fatal(err)
	}
	if data, ctype, err := sonarr.poster(context.Background(), 9); err != nil || ctype != "image/jpeg" || !bytes.Equal(data, jpegBytes) || path != "/api/v3/mediacover/9/poster-250.jpg" || key != "sonarr-key" {
		t.Errorf("Sonarr: %q key %q type %q err %v", path, key, ctype, err)
	}
	if _, _, err := radarr.poster(context.Background(), 31); err != nil || path != "/api/v3/mediacover/31/poster-250.jpg" || key != "radarr-key" {
		t.Errorf("Radarr: %q key %q err %v", path, key, err)
	}
	path = ""
	for _, id := range []int{0, -1, maxID + 1} {
		if _, _, err := sonarr.poster(context.Background(), id); err == nil {
			t.Errorf("Sonarr id %d was accepted", id)
		}
		if _, _, err := radarr.poster(context.Background(), id); err == nil {
			t.Errorf("Radarr id %d was accepted", id)
		}
	}
	if path != "" {
		t.Errorf("a bad id reached the service: %q", path)
	}
}

func TestACoverFallsBackToTheAddressTheWebPageUses(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	api, ui := func(w http.ResponseWriter) { http.NotFound(w, nil) }, func(w http.ResponseWriter) { w.Write(pngBytes) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.Path)
		a, u := api, ui
		mu.Unlock()
		switch r.URL.Path {
		case "/api/v3/mediacover/9/poster-250.jpg":
			a(w)
		case "/MediaCover/9/poster-250.jpg":
			u(w)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	sonarr, err := newSonarr(srv.URL, "k", newUpstreamClient(true))
	if err != nil {
		t.Fatal(err)
	}
	set := func(a, u func(http.ResponseWriter)) {
		mu.Lock()
		api, ui, asked = a, u, nil
		mu.Unlock()
	}
	got := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), asked...) }
	status := func(code int) func(http.ResponseWriter) {
		return func(w http.ResponseWriter) { http.Error(w, "no", code) }
	}
	good := func(w http.ResponseWriter) { w.Write(jpegBytes) }
	both := []string{"/api/v3/mediacover/9/poster-250.jpg", "/MediaCover/9/poster-250.jpg"}

	set(good, status(500))
	if _, ctype, err := sonarr.poster(context.Background(), 9); err != nil || ctype != "image/jpeg" || len(got()) != 1 {
		t.Errorf("the API address works: %v, asked %v", err, got())
	}
	set(status(404), good)
	if _, ctype, err := sonarr.poster(context.Background(), 9); err != nil || ctype != "image/jpeg" || !reflect.DeepEqual(got(), both) {
		t.Errorf("only the web page's address works: %v, asked %v", err, got())
	}
	set(func(w http.ResponseWriter) { w.Write([]byte("<html>log in</html>")) }, good)
	if _, _, err := sonarr.poster(context.Background(), 9); err != nil || !reflect.DeepEqual(got(), both) {
		t.Errorf("the API answers with a web page: %v, asked %v", err, got())
	}
	set(status(401), status(302))
	_, _, err = sonarr.poster(context.Background(), 9)
	if err == nil || !strings.Contains(err.Error(), "401") || !reflect.DeepEqual(got(), both) {
		t.Errorf("both fail: %v, asked %v (the first answer is the one reported)", err, got())
	}
}

// ---------- Seerr's copy of TMDB's poster ----------

func TestSeerrMayOnlyBeAskedForTitlesAndOneSizeOfPoster(t *testing.T) {
	seerr, err := newSeerr("http://127.0.0.1:1", "k", newUpstreamClient(true))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		"/api/v1/request", "/api/v1/movie/603", "/api/v1/tv/1399",
		"/imageproxy/tmdb/t/p/w185/8Gxv8gSFCU0XGDykEGv7zR1n2ua.jpg", "/imageproxy/tmdb/t/p/w185/abc_def-1.png",
	} {
		if !seerr.u.pathAllowed(p) {
			t.Errorf("%s should be allowed", p)
		}
	}
	for _, p := range []string{
		"/imageproxy/tmdb/t/p/w500/abc.jpg", "/imageproxy/tmdb/t/p/original/abc.jpg", "/imageproxy/tvdb/t/p/w185/abc.jpg",
		"/imageproxy/tmdb/t/p/w185/abc.svg", "/imageproxy/tmdb/t/p/w185/a/b.jpg", "/imageproxy/tmdb/t/p/w185/../x.jpg",
		"/imageproxy/tmdb/t/p/w185/.jpg", "/imageproxy/https://evil.example/x.jpg", "/imageproxy/tmdb/t/p/w185/abc.jpg?x=1",
		"/imageproxy/tmdb/t/p/w185//abc.jpg", "/imageproxy/tmdb/t/p/w185/abc.jpg/", "/api/v1/settings/main", "/api/v1/user",
		"/_next/image", "/api/v1/movie/", "/api/v1/tv/603/extra",
	} {
		if seerr.u.pathAllowed(p) {
			t.Errorf("%s should not be allowed", p)
		}
	}
}

// The name Seerr gives is checked on its own, as well as by the address list
// after it: either one would stop these.
func TestSeerrPosterPathsMustBeAPlainFileName(t *testing.T) {
	for _, ok := range []string{"/8Gxv8gSFCU0XGDykEGv7zR1n2ua.jpg", "/abc_def-1.png", "/a.jpg"} {
		if !seerrPosterPath.MatchString(ok) {
			t.Errorf("%q should be accepted", ok)
		}
	}
	for _, bad := range []string{
		"", "/", "abc.jpg", "/../x.jpg", "//evil.example/x.jpg", "http://evil.example/x.jpg", "/a/b.jpg", "/x.svg", "/x.jpeg",
		"/x.JPG", "/abc.jpg?x=1", "/abc.jpg#f", "/ab%2fc.jpg", "/abc.jpg\n", "\n/abc.jpg", "/.jpg", "/abc.jpg/", "/ab c.jpg",
		"/" + strings.Repeat("a", 65) + ".jpg",
	} {
		if seerrPosterPath.MatchString(bad) {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestSeerrPosterLooksUpThePosterPathThenFetchesThatPicture(t *testing.T) {
	g := &gauge{}
	srv := newStackSrv(t, g, map[string]string{
		"/api/v1/movie/603":                    `{"title":"The Matrix","posterPath":"/abc123.jpg"}`,
		"/api/v1/tv/1399":                      `{"name":"Some Show","posterPath":"/tv_x-9.png"}`,
		"/api/v1/movie/700":                    `{"title":"No Poster"}`,
		"/api/v1/movie/701":                    `{"title":"Odd","posterPath":"/../../x.jpg"}`,
		"/imageproxy/tmdb/t/p/w185/abc123.jpg": string(jpegBytes),
		"/imageproxy/tmdb/t/p/w185/tv_x-9.png": string(pngBytes),
	})
	s, err := newSeerr(srv.URL, "seerr-key", newUpstreamClient(true))
	if err != nil {
		t.Fatal(err)
	}
	if data, ctype, err := s.poster(context.Background(), "movie", 603); err != nil || ctype != "image/jpeg" || !bytes.Equal(data, jpegBytes) {
		t.Errorf("movie: type %q err %v", ctype, err)
	}
	if data, ctype, err := s.poster(context.Background(), "show", 1399); err != nil || ctype != "image/png" || !bytes.Equal(data, pngBytes) {
		t.Errorf("show: type %q err %v", ctype, err)
	}
	if _, _, err := s.poster(context.Background(), "movie", 700); err == nil {
		t.Error("a title with no poster gave a picture")
	}
	if srv.count("/imageproxy") != 2 {
		t.Errorf("pictures asked for: %d, want only the 2 that exist", srv.count("/imageproxy"))
	}
	for _, bad := range []string{"/../../x.jpg", "http://evil.example/p.jpg", "//evil.example/p.jpg", "/a/b.jpg", "/x.svg", "abc.jpg",
		"/abc.jpg?x=1", "/abc.jpg#x", "/ab%2fc.jpg", "/abc.jpg\n", "/" + strings.Repeat("a", 65) + ".jpg", "/.jpg", "/abc.jpg/"} {
		srv.routes["/api/v1/movie/702"] = `{"posterPath":` + strconv.Quote(bad) + `}`
		before := srv.count("/imageproxy")
		if _, _, err := s.poster(context.Background(), "movie", 702); err == nil || srv.count("/imageproxy") != before {
			t.Errorf("posterPath %q was followed (err %v)", bad, err)
		}
	}
	before := srv.count("")
	for _, c := range []struct {
		kind string
		id   int
	}{{"movie", 0}, {"movie", -1}, {"movie", maxID + 1}, {"music", 5}, {"", 5}, {"tv", 5}} {
		if _, _, err := s.poster(context.Background(), c.kind, c.id); err == nil {
			t.Errorf("%+v was accepted", c)
		}
	}
	if srv.count("") != before {
		t.Error("a bad id or kind reached Seerr")
	}
}

// The real lists, not a copy of them: Sonarr and Radarr may be asked for a
// show's or movie's 250-pixel cover, at either of its two addresses, and the
// few JSON questions already there, and for nothing else under either or
// anywhere.
func TestSonarrAndRadarrMayOnlyBeAskedForTheFixedCover(t *testing.T) {
	client := newUpstreamClient(true)
	sonarr, err := newSonarr("http://127.0.0.1:1", "k", client)
	if err != nil {
		t.Fatal(err)
	}
	radarr, err := newRadarr("http://127.0.0.1:1", "k", client)
	if err != nil {
		t.Fatal(err)
	}
	for name, u := range map[string]*upstream{"Sonarr": sonarr.u, "Radarr": radarr.u} {
		for _, p := range []string{
			"/api/v3/mediacover/5/poster-250.jpg", "/api/v3/mediacover/999999999/poster-250.jpg",
			"/MediaCover/5/poster-250.jpg", "/MediaCover/999999999/poster-250.jpg", "/api/v3/queue", "/api/v3/calendar",
		} {
			if !u.pathAllowed(p) {
				t.Errorf("%s: %s should be allowed", name, p)
			}
		}
		for _, p := range []string{
			"/MediaCover/5/poster.jpg", "/MediaCover/5/poster-500.jpg", "/MediaCover/5/fanart-250.jpg", "/MediaCover/5/poster-250.png",
			"/MediaCover/5/poster-250.jpg/", "/MediaCover/5/poster-250.jpg?x=1", "/MediaCover/5/../5/poster-250.jpg", "/MediaCover//poster-250.jpg",
			"/MediaCover/Series/5/poster-250.jpg", "/MediaCover/1000000000/poster-250.jpg", "/MediaCover/0x5/poster-250.jpg", "/MediaCover/",
			"/mediacover/5/poster-250.jpg", "/api/v3/system/status", "/api/v3/series/5",
			"/api/v3/mediacover/5/poster.jpg", "/api/v3/mediacover/5/poster-500.jpg", "/api/v3/mediacover/5/poster-250.jpg?apikey=k",
			"/api/v3/mediacover/5/../x", "/api/v3/mediacover//poster-250.jpg", "/api/v3/mediacover/x/poster-250.jpg",
			"/api/v3/mediacover/5/poster-250.jpg/", "/api/v3/mediacover/1000000000/poster-250.jpg", "/api/v3/MediaCover/5/poster-250.jpg",
			"/api/v3/mediacover/", "/api/v3/mediacover",
		} {
			if u.pathAllowed(p) {
				t.Errorf("%s: %s should not be allowed", name, p)
			}
		}
	}
	if sonarr.u.pathAllowed("/api/v3/movie") || !radarr.u.pathAllowed("/api/v3/movie") {
		t.Error("the JSON lists changed")
	}
}

func TestParsePosterRef(t *testing.T) {
	good := map[[2]string]posterRef{
		{"movie", "5"}:         {"movie", 5},
		{"show", "999999999"}:  {"show", 999999999},
		{"movie", "1234"}:      {"movie", 1234},
		{"show", "10"}:         {"show", 10},
		{"movie", "100000000"}: {"movie", 100000000},
		{"plex", "10"}:         {"plex", 10},
		{"tmdbmovie", "603"}:   {"tmdbmovie", 603},
		{"tmdbtv", "1399"}:     {"tmdbtv", 1399},
	}
	for in, want := range good {
		if got, ok := parsePosterRef(in[0], in[1]); !ok || got != want {
			t.Errorf("%v: got %+v %v", in, got, ok)
		}
	}
	for _, in := range [][2]string{
		{"tv", "5"}, {"Movie", "5"}, {"", "5"}, {"movie ", "5"}, {"movie/../x", "5"}, {"Plex", "5"}, {"radarr", "5"}, {"sonarr", "5"}, {"tmdb", "5"}, {"seerr", "5"}, {"TMDBMovie", "5"}, {"tmdbtv ", "5"},
		{"movie", ""}, {"movie", "0"}, {"movie", "-1"}, {"movie", "+1"}, {"movie", "05"}, {"movie", "1e3"},
		{"movie", "1_0"}, {"movie", " 5"}, {"movie", "5 "}, {"movie", "5/"}, {"movie", "1000000000"},
		{"movie", "٣"}, {"movie", "5.0"}, {"movie", "0x10"},
	} {
		if got, ok := parsePosterRef(in[0], in[1]); ok {
			t.Errorf("%q was accepted as %+v", in, got)
		}
	}
}

func TestParsePosterString(t *testing.T) {
	for in, want := range map[string]posterRef{
		"plex/10": {"plex", 10}, "movie/5": {"movie", 5}, "show/9": {"show", 9},
		"tmdbmovie/603": {"tmdbmovie", 603}, "tmdbtv/1399": {"tmdbtv", 1399},
	} {
		if got, ok := parsePosterString(in); !ok || got != want {
			t.Errorf("%q: got %+v %v", in, got, ok)
		}
	}
	for _, in := range []string{
		"", "plex", "plex/", "/10", "plex/10/", "plex/010", "plex//10", "a/b/c", "plex/10 ", " plex/10", "PLEX/10",
		"plex/10?x=1", "plex/-10", "plex/0", "tmdb/5", "plex/1000000000", "plex/10\n", "plex\\10",
	} {
		if got, ok := parsePosterString(in); ok {
			t.Errorf("%q was accepted as %+v", in, got)
		}
	}
}

// A title lists everywhere its poster may come from, best first, and the page
// tries them in that order: Plex's own copy if the title is in Plex, then
// Seerr's copy of TMDB's picture by its TMDB id, then Sonarr's or Radarr's
// small cover by their own number. A number of 0 (or one that couldn't be real)
// means that place doesn't have it.
func TestPosterRefsListWhereAPosterCanComeFromBestFirst(t *testing.T) {
	for _, c := range []struct {
		kind              string
		plex, tmdb, arrID int
		want              []string
	}{
		{"movie", 10, 603, 31, []string{"plex/10", "tmdbmovie/603", "movie/31"}},
		{"show", 20, 1399, 9, []string{"plex/20", "tmdbtv/1399", "show/9"}},
		{"movie", 0, 603, 0, []string{"tmdbmovie/603"}},
		{"movie", 0, 603, 31, []string{"tmdbmovie/603", "movie/31"}},
		{"show", 0, 0, 9, []string{"show/9"}},
		{"show", 20, 0, 0, []string{"plex/20"}},
		{"movie", 0, 0, 0, nil},
		{"movie", -1, 0, maxID + 1, nil},
		{"show", maxID + 1, -5, 0, nil},
		{"music", 1, 1, 1, nil},
		{"", 1, 1, 1, nil},
	} {
		if got := posterRefs(c.kind, c.plex, c.tmdb, c.arrID); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%+v: got %v", c, got)
		}
	}
}

// A movie or show that is in Plex gets its poster from Plex. Plex scales it to
// the same 250 pixels, and Anchor asks for exactly that: a fixed path, a query
// it builds itself from the rating key, the token in a header.
func TestPlexMayOnlyBeAskedForItsListsAndAMoviesPoster(t *testing.T) {
	plex, err := newPlex("http://127.0.0.1:1", "k", newUpstreamClient(true))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		"/library/sections", "/library/sections/1/all", "/library/metadata/20/children",
		"/photo/:/transcode", "/library/metadata/10/thumb", "/library/metadata/999999999/thumb",
	} {
		if !plex.u.pathAllowed(p) {
			t.Errorf("%s should be allowed", p)
		}
	}
	for _, p := range []string{
		"/photo/:/transcode/", "/photo/:/other", "/photo/:/transcode?url=x", "/library/metadata/10/thumb/1699999999",
		"/library/metadata/10/art", "/library/metadata/10", "/library/metadata/x/thumb", "/library/metadata/1000000000/thumb",
		"/library/metadata//thumb", "/library/metadata/10/../11/thumb", "/library/sections/1/refresh", "/status/sessions",
		"/accounts", "/:/prefs", "/library/metadata/10/thumb/", "/Library/metadata/10/thumb",
	} {
		if plex.u.pathAllowed(p) {
			t.Errorf("%s should not be allowed", p)
		}
	}
}

func TestPlexPosterAsksPlexForAScaledPictureByRatingKey(t *testing.T) {
	var mu sync.Mutex
	var path, token, raw string
	var query url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		path, token, raw, query = r.URL.Path, r.Header.Get("X-Plex-Token"), r.URL.RawQuery, r.URL.Query()
		mu.Unlock()
		w.Write(jpegBytes)
	}))
	defer srv.Close()
	p, err := newPlex(srv.URL, "plex-token", newUpstreamClient(true))
	if err != nil {
		t.Fatal(err)
	}
	data, ctype, err := p.poster(context.Background(), 10)
	if err != nil || ctype != "image/jpeg" || !bytes.Equal(data, jpegBytes) {
		t.Fatalf("type %q, %d bytes, err %v", ctype, len(data), err)
	}
	if path != "/photo/:/transcode" || token != "plex-token" || query.Get("url") != "/library/metadata/10/thumb" ||
		query.Get("width") != "250" || query.Get("height") != "375" {
		t.Errorf("path %q token %q query %v", path, token, query)
	}
	if strings.Contains(raw, "plex-token") || strings.Contains(raw, "X-Plex-Token") {
		t.Errorf("the token is in the address: %s", raw)
	}
	path = ""
	for _, id := range []int{0, -1, maxID + 1} {
		if _, _, err := p.poster(context.Background(), id); err == nil {
			t.Errorf("rating key %d was accepted", id)
		}
	}
	if path != "" {
		t.Errorf("a bad rating key reached Plex: %q", path)
	}
}

func TestPlexPosterFallsBackToTheOriginalWhenPlexCannotScaleIt(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	scaling, original := http.StatusInternalServerError, jpegBytes
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.Path)
		code, body := scaling, original
		mu.Unlock()
		switch r.URL.Path {
		case "/photo/:/transcode":
			if code != http.StatusOK {
				http.Error(w, "no", code)
				return
			}
			w.Write(pngBytes)
		case "/library/metadata/10/thumb":
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p, _ := newPlex(srv.URL, "t", newUpstreamClient(true))
	run := func() ([]byte, string, error, []string) {
		mu.Lock()
		asked = nil
		mu.Unlock()
		d, c, err := p.poster(context.Background(), 10)
		mu.Lock()
		defer mu.Unlock()
		return d, c, err, append([]string(nil), asked...)
	}

	if d, c, err, got := run(); err != nil || c != "image/jpeg" || !bytes.Equal(d, jpegBytes) || !reflect.DeepEqual(got, []string{"/photo/:/transcode", "/library/metadata/10/thumb"}) {
		t.Errorf("scaling fails, original fine: type %q err %v asked %v", c, err, got)
	}
	mu.Lock()
	scaling = http.StatusOK
	mu.Unlock()
	if d, c, err, got := run(); err != nil || c != "image/png" || !bytes.Equal(d, pngBytes) || len(got) != 1 {
		t.Errorf("scaling works: type %q err %v asked %v (%d bytes)", c, err, got, len(d))
	}
	mu.Lock()
	scaling, original = http.StatusInternalServerError, append([]byte{0xFF, 0xD8, 0xFF}, bytes.Repeat([]byte{1}, posterMax)...)
	mu.Unlock()
	if _, _, err, _ := run(); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("both fail (the original is over the limit): %v", err)
	}
	mu.Lock()
	original = []byte("<html>not a picture</html>")
	mu.Unlock()
	if _, _, err, _ := run(); err == nil {
		t.Error("a web page was accepted as the original")
	}
}

// ---------- keeping pictures ----------

func newTestCache() (*posterCache, *clock) {
	ck := &clock{t: t0}
	c := newPosterCache()
	c.now = ck.Now
	return c, ck
}

func key(n int) posterKey { return posterKey{sig: "s", ref: posterRef{"movie", n}} }

func okFetch(calls *atomic.Int32, body []byte) func(context.Context) ([]byte, string, error) {
	return func(context.Context) ([]byte, string, error) {
		calls.Add(1)
		return body, imageType(body), nil
	}
}

func TestPosterCacheKeepsAPictureForADay(t *testing.T) {
	c, ck := newTestCache()
	var calls atomic.Int32
	for i := 0; i < 3; i++ {
		data, ctype, ok := c.get(context.Background(), key(1), okFetch(&calls, jpegBytes))
		if !ok || ctype != "image/jpeg" || !bytes.Equal(data, jpegBytes) {
			t.Fatalf("get %d: ok=%v type=%q", i, ok, ctype)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("fetched %d times", calls.Load())
	}
	ck.advance(posterTTL - time.Minute)
	c.get(context.Background(), key(1), okFetch(&calls, jpegBytes))
	if calls.Load() != 1 {
		t.Fatalf("fetched again after %v", posterTTL-time.Minute)
	}
	ck.advance(2 * time.Minute)
	c.get(context.Background(), key(1), okFetch(&calls, jpegBytes))
	if calls.Load() != 2 {
		t.Fatalf("a day later it should be fetched again; calls %d", calls.Load())
	}
}

func TestPosterCacheRemembersAMissForTenMinutes(t *testing.T) {
	c, ck := newTestCache()
	var calls atomic.Int32
	failing := func(context.Context) ([]byte, string, error) {
		calls.Add(1)
		return nil, "", errors.New("Radarr answered 404")
	}
	for i := 0; i < 3; i++ {
		if _, _, ok := c.get(context.Background(), key(1), failing); ok {
			t.Fatal("a miss was served")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("asked %d times within the ten minutes", calls.Load())
	}
	ck.advance(posterMissTTL + time.Second)
	if data, _, ok := c.get(context.Background(), key(1), okFetch(&calls, jpegBytes)); !ok || len(data) == 0 {
		t.Fatal("after ten minutes it should try again, and the picture should come")
	}
	if calls.Load() != 2 {
		t.Fatalf("calls %d", calls.Load())
	}
}

func TestPosterCacheLetsCallersShareOneFetch(t *testing.T) {
	c, _ := newTestCache()
	var calls atomic.Int32
	release := make(chan struct{})
	fetch := func(context.Context) ([]byte, string, error) {
		calls.Add(1)
		<-release
		return jpegBytes, "image/jpeg", nil
	}
	var wg sync.WaitGroup
	got := make([]bool, 20)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, got[i] = c.get(context.Background(), key(1), fetch)
		}()
	}
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond) // let the others queue up behind it
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("%d fetches for one picture", calls.Load())
	}
	for i, ok := range got {
		if !ok {
			t.Errorf("caller %d got nothing", i)
		}
	}
}

func TestPosterCacheFetchesAtMostFourAtATime(t *testing.T) {
	c, _ := newTestCache()
	var cur, most atomic.Int32
	fetch := func(context.Context) ([]byte, string, error) {
		n := cur.Add(1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
		cur.Add(-1)
		return jpegBytes, "image/jpeg", nil
	}
	var wg sync.WaitGroup
	for i := 1; i <= 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.get(context.Background(), key(i), fetch)
		}()
	}
	wg.Wait()
	if most.Load() > posterFetches || most.Load() < 2 {
		t.Fatalf("up to %d fetches at once; want 2 to %d", most.Load(), posterFetches)
	}
}

func TestPosterCacheIsBounded(t *testing.T) {
	c, ck := newTestCache()
	var calls atomic.Int32
	for i := 1; i <= posterEntries+30; i++ {
		c.get(context.Background(), key(i), okFetch(&calls, jpegBytes))
		ck.advance(time.Second)
	}
	if n, _ := c.stats(); n > posterEntries {
		t.Fatalf("%d entries, limit %d", n, posterEntries)
	}
	before := calls.Load()
	c.get(context.Background(), key(posterEntries+30), okFetch(&calls, jpegBytes)) // the newest is still there
	if calls.Load() != before {
		t.Error("the newest picture was thrown away")
	}
	c.get(context.Background(), key(1), okFetch(&calls, jpegBytes)) // the oldest is not
	if calls.Load() != before+1 {
		t.Error("the oldest picture was kept")
	}

	big := append([]byte{0xFF, 0xD8, 0xFF}, bytes.Repeat([]byte{1}, 400<<10)...)
	c2, ck2 := newTestCache()
	for i := 1; i <= 40; i++ {
		c2.get(context.Background(), key(i), okFetch(&calls, big))
		ck2.advance(time.Second)
	}
	if _, size := c2.stats(); size > posterBytes {
		t.Fatalf("%d bytes held, limit %d", size, posterBytes)
	}
}

func TestAPhoneThatLeavesDoesNotStopTheFetch(t *testing.T) {
	c, _ := newTestCache()
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	var fetchErr atomic.Value
	var hadDeadline atomic.Bool
	fetch := func(ctx context.Context) ([]byte, string, error) {
		calls.Add(1)
		_, d := ctx.Deadline()
		hadDeadline.Store(d)
		close(started)
		<-release
		if err := ctx.Err(); err != nil {
			fetchErr.Store(err)
		}
		return jpegBytes, "image/jpeg", nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool)
	go func() {
		_, _, ok := c.get(ctx, key(1), fetch)
		done <- ok
	}()
	<-started
	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("a caller that left still got a picture")
		}
	case <-time.After(time.Second):
		t.Fatal("the caller kept waiting after leaving")
	}
	close(release)
	deadline := time.Now().Add(time.Second)
	for {
		if _, _, ok := c.get(context.Background(), key(1), func(context.Context) ([]byte, string, error) {
			t.Error("fetched a second time")
			return nil, "", errors.New("x")
		}); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the fetch did not finish for the next caller")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if v := fetchErr.Load(); v != nil {
		t.Errorf("the fetch was cancelled along with the caller: %v", v)
	}
	if !hadDeadline.Load() {
		t.Error("the fetch has no time limit")
	}
	if calls.Load() != 1 {
		t.Errorf("%d fetches", calls.Load())
	}
}

func TestPosterCacheSurvivesAPanickingFetch(t *testing.T) {
	c, ck := newTestCache()
	if _, _, ok := c.get(context.Background(), key(1), func(context.Context) ([]byte, string, error) { panic("surprise") }); ok {
		t.Fatal("a panic gave a picture")
	}
	ck.advance(posterMissTTL + time.Second)
	var calls atomic.Int32
	if _, _, ok := c.get(context.Background(), key(1), okFetch(&calls, jpegBytes)); !ok {
		t.Fatal("the cache did not recover")
	}
}

func TestPosterCacheKeepsDifferentSettingsApart(t *testing.T) {
	c, _ := newTestCache()
	var calls atomic.Int32
	a := posterKey{sig: "old addresses", ref: posterRef{"show", 9}}
	b := posterKey{sig: "new addresses", ref: posterRef{"show", 9}}
	c.get(context.Background(), a, okFetch(&calls, jpegBytes))
	c.get(context.Background(), b, okFetch(&calls, pngBytes))
	if calls.Load() != 2 {
		t.Fatalf("a picture from the old address was reused: %d fetches", calls.Load())
	}
}

// ---------- limits for one person ----------

func TestBucketLimiterAllowsABurstThenSlowsDown(t *testing.T) {
	ck := &clock{t: t0}
	l := newBucketLimiter(5, time.Second)
	l.now = ck.Now
	for i := 0; i < 5; i++ {
		if !l.allow("mom") {
			t.Fatalf("request %d of the burst was refused", i+1)
		}
	}
	if l.allow("mom") {
		t.Fatal("a sixth in a row was allowed")
	}
	if !l.allow("dad") {
		t.Fatal("one person's burst used up another's")
	}
	ck.advance(2500 * time.Millisecond)
	if !l.allow("mom") || !l.allow("mom") || l.allow("mom") {
		t.Fatal("after 2.5s exactly two more should be allowed")
	}
	ck.advance(time.Hour)
	for i := 0; i < 5; i++ {
		if !l.allow("mom") {
			t.Fatalf("after an hour the burst should be back (%d)", i+1)
		}
	}
	if l.allow("mom") {
		t.Fatal("waiting longer than a full bucket should not save up more than the burst")
	}
}

// ---------- which pictures may be asked for ----------

func TestListedPostersAreExactlyThoseOnTheSheet(t *testing.T) {
	snap := &snapshot{
		Rows: []requestRow{
			{Title: "A", Kind: "show", Posters: []string{"plex/20", "tmdbtv/1399", "show/9"}},
			{Title: "B", Kind: "movie", Posters: []string{"movie/30"}},
			{Title: "No posters", Kind: "movie"},
			// Nothing a service could say gets onto the list unless it is well formed.
			{Title: "Junk", Kind: "movie", Posters: []string{"", "plex", "plex/0", "plex/-4", "plex/010", "music/5", "plex/10/x", "movie/1000000000", "../etc/passwd", "movie/7 "}},
		},
		Coming: &comingList{Rows: []comingRow{
			{Title: "C", Kind: "movie", Posters: []string{"tmdbmovie/603", "movie/40"}},
			{Title: "D", Kind: "show", Posters: []string{"show/9"}}, // the same one twice is fine
			{Title: "E", Kind: "show"},
		}},
	}
	want := map[posterRef]bool{
		{"plex", 20}: true, {"tmdbtv", 1399}: true, {"show", 9}: true, {"movie", 30}: true,
		{"tmdbmovie", 603}: true, {"movie", 40}: true,
	}
	if got := listedPosters(snap); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := listedPosters(&snapshot{}); len(got) != 0 {
		t.Fatalf("an empty snapshot lists %v", got)
	}
}

// ---------- ids on the rows ----------

func TestPullListsWhereEveryRowsPosterCanComeFrom(t *testing.T) {
	st := newStack(t)
	rs, cfg := newStackService(t, st, stackKeys)
	snap, err := rs.pull(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	for _, r := range snap.Rows {
		rows = append(rows, r.Title+"|"+strings.Join(r.Posters, ","))
	}
	wantRows := []string{
		"Ready Movie|plex/10,tmdbmovie/100",   // in Plex: its own copy first; Radarr is never asked about it
		"Slow Show|plex/20,tmdbtv/200,show/9", // a show in Plex too, with Sonarr's number for it as a last resort
		"Future Movie|tmdbmovie/101,movie/31", // not in Plex yet: Seerr, then Radarr
		"Pending Movie|tmdbmovie/102",         // waiting for approval: Seerr has its poster even so
		"Mystery Show|tmdbtv/201",             // Sonarr doesn't know it yet
		"<script>alert(1)</script>Evil|tmdbmovie/103,movie/33",
	}
	if !reflect.DeepEqual(rows, wantRows) {
		t.Fatalf("rows:\n%s\nwant:\n%s", strings.Join(rows, "\n"), strings.Join(wantRows, "\n"))
	}
	var coming []string
	for _, r := range snap.Coming.Rows {
		coming = append(coming, r.Title+"|"+strings.Join(r.Posters, ","))
	}
	wantComing := []string{
		"Slow Show|plex/20,tmdbtv/200,show/9", // the calendar says which TVDB and TMDB show it is, and Plex has it
		"Brand New|show/11",                   // a premiere the calendar knows nothing more about
		"Cinema Movie|movie/40",
		"Future Movie|tmdbmovie/101,movie/31",
	}
	if !reflect.DeepEqual(coming, wantComing) {
		t.Fatalf("coming up:\n%s\nwant:\n%s", strings.Join(coming, "\n"), strings.Join(wantComing, "\n"))
	}
}

func TestPullDoesNotListPostersFromServicesThatAreNotThere(t *testing.T) {
	st := newStack(t)
	// No Plex token, no Sonarr, no Radarr: Seerr's copy is all there is.
	store := storeFor(t, requestsBase+"requests:\n  app: seerr\n  seerr: "+st.seerr.URL+"\n  plex: "+st.plex.URL+"\n")
	cfg, _ := store.Get()
	rs := newRequestStatus(store, requestKeys{Seerr: "seerr-key"}, true)
	rs.now = func() time.Time { return t0 }
	snap, err := rs.pull(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range snap.Rows {
		for _, p := range r.Posters {
			if !strings.HasPrefix(p, "tmdb") {
				t.Errorf("%s: %s is from a service that isn't set up", r.Title, p)
			}
		}
	}
	if snap.Coming != nil {
		t.Errorf("Coming up without Sonarr or Radarr: %+v", snap.Coming)
	}
}

// ---------- the ids a Coming up row's poster is found by ----------

func TestCalendarsKeepTheIdsAPosterIsFoundBy(t *testing.T) {
	g := &gauge{}
	sonarrSrv := newStackSrv(t, g, map[string]string{"/api/v3/calendar": `[
		{"seriesId":9,"seasonNumber":1,"episodeNumber":1,"airDateUtc":"2026-10-08T12:00:00Z","series":{"title":"Numbers","year":2026,"tvdbId":2000,"tmdbId":200}},
		{"seriesId":10,"seasonNumber":1,"episodeNumber":1,"airDateUtc":"2026-10-09T12:00:00Z","series":{"title":"Strings","year":2026,"tvdbId":"2001","tmdbId":"201"}},
		{"seriesId":11,"seasonNumber":1,"episodeNumber":1,"airDateUtc":"2026-10-10T12:00:00Z","series":{"title":"Nothing","year":2026}},
		{"seriesId":12,"seasonNumber":1,"episodeNumber":1,"airDateUtc":"2026-10-11T12:00:00Z","series":{"title":"Junk","year":2026,"tvdbId":-4,"tmdbId":"x"}}]`})
	radarrSrv := newStackSrv(t, g, map[string]string{"/api/v3/calendar": `[
		{"id":31,"title":"With","year":2026,"tmdbId":101,"minimumAvailability":"released","digitalRelease":"2026-11-12T00:00:00Z"},
		{"id":32,"title":"Without","year":2026,"minimumAvailability":"released","digitalRelease":"2026-11-13T00:00:00Z"}]`})
	client := newUpstreamClient(true)
	sonarr, _ := newSonarr(sonarrSrv.URL, "k", client)
	radarr, _ := newRadarr(radarrSrv.URL, "k", client)
	eps, err := sonarr.calendar(context.Background(), t0, t0.AddDate(0, 6, 0))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range eps {
		got = append(got, e.Title+"|"+strconv.Itoa(e.TVDB)+"|"+strconv.Itoa(e.TMDB))
	}
	if want := []string{"Numbers|2000|200", "Strings|2001|201", "Nothing|0|0", "Junk|0|0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("episodes: %v, want %v", got, want)
	}
	movies, err := radarr.calendar(context.Background(), t0, t0.AddDate(0, 6, 0))
	if err != nil {
		t.Fatal(err)
	}
	got = nil
	for _, m := range movies {
		got = append(got, m.Title+"|"+strconv.Itoa(m.TMDB))
	}
	if want := []string{"With|101", "Without|0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("movies: %v, want %v", got, want)
	}
}

func TestComingRowsKeepTheIdsTheirPostersAreFoundBy(t *testing.T) {
	show := ep(9, "Numbers", 1, 2, "2026-10-08T12:00:00Z")
	show.TVDB, show.TMDB = 2000, 200
	film := movie(31, "With", "2026-11-12", "home")
	film.TMDB = 101
	rows := coming([]calEpisode{show}, []calMovie{film})
	if len(rows) != 2 {
		t.Fatalf("rows: %+v", rows)
	}
	if r := rows[0]; r.arrID != 9 || r.tvdb != 2000 || r.tmdb != 200 {
		t.Errorf("show: %+v", r)
	}
	if r := rows[1]; r.arrID != 31 || r.tvdb != 0 || r.tmdb != 101 {
		t.Errorf("movie: %+v", r)
	}
}

func TestComingPostersListWhereEachRowsPosterCanComeFrom(t *testing.T) {
	idx := &plexIndex{
		movieKeys: map[int]string{101: "10", 102: "x", 103: "0"},
		showsTVDB: map[int]string{2000: "20"},
		showsTMDB: map[int]string{300: "30"},
	}
	rows := []comingRow{
		{Title: "In Plex by TVDB", Kind: "show", arrID: 9, tvdb: 2000, tmdb: 200},
		{Title: "In Plex by TMDB", Kind: "show", arrID: 8, tmdb: 300},
		{Title: "Not in Plex", Kind: "show", arrID: 7, tvdb: 2999, tmdb: 299},
		{Title: "Sonarr only", Kind: "show", arrID: 6},
		{Title: "Movie in Plex", Kind: "movie", arrID: 31, tmdb: 101},
		{Title: "Movie, odd Plex number", Kind: "movie", arrID: 32, tmdb: 102},
		{Title: "Movie, zero Plex number", Kind: "movie", arrID: 33, tmdb: 103},
		{Title: "Movie, not in Plex", Kind: "movie", arrID: 34, tmdb: 104},
		{Title: "No ids at all", Kind: "movie"},
	}
	comingPosters(rows, idx)
	want := map[string]string{
		"In Plex by TVDB":         "plex/20,tmdbtv/200,show/9",
		"In Plex by TMDB":         "plex/30,tmdbtv/300,show/8",
		"Not in Plex":             "tmdbtv/299,show/7",
		"Sonarr only":             "show/6",
		"Movie in Plex":           "plex/10,tmdbmovie/101,movie/31",
		"Movie, odd Plex number":  "tmdbmovie/102,movie/32",
		"Movie, zero Plex number": "tmdbmovie/103,movie/33",
		"Movie, not in Plex":      "tmdbmovie/104,movie/34",
		"No ids at all":           "",
	}
	for _, r := range rows {
		if got := strings.Join(r.Posters, ","); got != want[r.Title] {
			t.Errorf("%s: %q, want %q", r.Title, got, want[r.Title])
		}
	}
	// With Plex unreadable there is no index, and nothing from Plex is listed.
	rows = []comingRow{{Title: "Show", Kind: "show", arrID: 9, tvdb: 2000, tmdb: 200}}
	comingPosters(rows, nil)
	if got := strings.Join(rows[0].Posters, ","); got != "tmdbtv/200,show/9" {
		t.Errorf("without Plex: %q", got)
	}
}

// ---------- the service: only what is on the sheet, from the right place ----------

func listSnapshot(rs *requestStatus, cfg *Config, refs ...posterRef) {
	snap := &snapshot{sig: cfg.Requests.sig(), Taken: rs.now(), posters: map[posterRef]bool{}}
	for _, r := range refs {
		snap.posters[r] = true
	}
	rs.snap = snap
}

func TestPosterIsOnlyServedForTitlesOnTheSheet(t *testing.T) {
	st := newStack(t)
	st.sonarr.routes["/api/v3/mediacover/9/poster-250.jpg"] = string(jpegBytes)
	st.sonarr.routes["/api/v3/mediacover/10/poster-250.jpg"] = string(jpegBytes)
	rs, cfg := newStackService(t, st, stackKeys)
	listSnapshot(rs, cfg, posterRef{"show", 9})
	if _, _, ok := rs.poster(context.Background(), cfg, posterRef{"show", 9}); !ok {
		t.Fatal("a listed picture was refused")
	}
	for _, ref := range []posterRef{{"show", 10}, {"movie", 9}, {"movie", 31}} {
		if _, _, ok := rs.poster(context.Background(), cfg, ref); ok {
			t.Errorf("%+v isn't on the sheet but was served", ref)
		}
	}
	if n := st.sonarr.count("/api/v3/mediacover/10") + st.radarr.count("/api/v3/mediacover/"); n != 0 {
		t.Errorf("a picture that isn't on the sheet was asked for (%d)", n)
	}
}

func TestPosterComesFromSonarrForShowsAndRadarrForMovies(t *testing.T) {
	st := newStack(t)
	st.sonarr.routes["/api/v3/mediacover/9/poster-250.jpg"] = string(jpegBytes)
	st.radarr.routes["/api/v3/mediacover/31/poster-250.jpg"] = string(pngBytes)
	rs, cfg := newStackService(t, st, stackKeys)
	listSnapshot(rs, cfg, posterRef{"show", 9}, posterRef{"movie", 31})
	if _, ctype, ok := rs.poster(context.Background(), cfg, posterRef{"show", 9}); !ok || ctype != "image/jpeg" {
		t.Errorf("show: %v %q", ok, ctype)
	}
	if _, ctype, ok := rs.poster(context.Background(), cfg, posterRef{"movie", 31}); !ok || ctype != "image/png" {
		t.Errorf("movie: %v %q", ok, ctype)
	}
	if st.sonarr.count("/api/v3/mediacover/9/") != 1 || st.radarr.count("/api/v3/mediacover/31/") != 1 {
		t.Error("each should have been asked once, at its own service")
	}
}

func TestPosterIsKeptSoTheServiceIsNotAskedAgain(t *testing.T) {
	st := newStack(t)
	st.sonarr.routes["/api/v3/mediacover/9/poster-250.jpg"] = string(jpegBytes)
	rs, cfg := newStackService(t, st, stackKeys)
	listSnapshot(rs, cfg, posterRef{"show", 9})
	for i := 0; i < 5; i++ {
		if _, _, ok := rs.poster(context.Background(), cfg, posterRef{"show", 9}); !ok {
			t.Fatal("refused")
		}
	}
	if n := st.sonarr.count("/api/v3/mediacover/9/"); n != 1 {
		t.Fatalf("Sonarr was asked %d times", n)
	}
}

func TestPosterIsNotServedWhenTheServiceIsOffOrKeyless(t *testing.T) {
	st := newStack(t)
	st.sonarr.routes["/api/v3/mediacover/9/poster-250.jpg"] = string(jpegBytes)
	rs, cfg := newStackService(t, st, requestKeys{Seerr: "k", Plex: "k"}) // no Sonarr key
	listSnapshot(rs, cfg, posterRef{"show", 9})
	if _, _, ok := rs.poster(context.Background(), cfg, posterRef{"show", 9}); ok {
		t.Error("served without a key")
	}
	if st.sonarr.count("/api/v3/mediacover/") != 0 {
		t.Error("Sonarr was asked without a key")
	}
}

func TestNoPosterWithoutAFreshEnoughSnapshotForTheseSettings(t *testing.T) {
	st := newStack(t)
	st.sonarr.routes["/api/v3/mediacover/9/poster-250.jpg"] = string(jpegBytes)
	rs, cfg := newStackService(t, st, stackKeys)
	if _, _, ok := rs.poster(context.Background(), cfg, posterRef{"show", 9}); ok {
		t.Fatal("served before there was any snapshot")
	}
	listSnapshot(rs, cfg, posterRef{"show", 9})
	changed := *cfg
	rc := *cfg.Requests
	rc.Sonarr = "http://127.0.0.1:1"
	changed.Requests = &rc
	if _, _, ok := rs.poster(context.Background(), &changed, posterRef{"show", 9}); ok {
		t.Fatal("a snapshot built from other settings was used")
	}
	if st.sonarr.count("/api/v3/mediacover/") != 0 {
		t.Error("Sonarr was asked")
	}
}

func TestPosterForAMovieInPlexComesFromPlexAndNeverFromRadarr(t *testing.T) {
	st := newStack(t)
	st.plex.routes["/photo/:/transcode"] = string(jpegBytes)
	rs, cfg := newStackService(t, st, stackKeys)
	listSnapshot(rs, cfg, posterRef{"plex", 10})
	for i := 0; i < 3; i++ {
		if data, ctype, ok := rs.poster(context.Background(), cfg, posterRef{"plex", 10}); !ok || ctype != "image/jpeg" || !bytes.Equal(data, jpegBytes) {
			t.Fatalf("ask %d: ok=%v type=%q", i+1, ok, ctype)
		}
	}
	if n := st.plex.count("/photo/:/transcode"); n != 1 {
		t.Errorf("Plex was asked %d times for the poster", n)
	}
	if st.radarr.count("") != 0 {
		t.Error("Radarr was asked about a movie that has its poster in Plex")
	}
}

func TestPosterFromPlexNeedsPlexsTokenAndAddress(t *testing.T) {
	st := newStack(t)
	st.plex.routes["/photo/:/transcode"] = string(jpegBytes)
	rs, cfg := newStackService(t, st, requestKeys{Seerr: "k", Sonarr: "k", Radarr: "k"}) // no Plex token
	listSnapshot(rs, cfg, posterRef{"plex", 10})
	if _, _, ok := rs.poster(context.Background(), cfg, posterRef{"plex", 10}); ok {
		t.Error("served without a Plex token")
	}
	if st.plex.count("") != 0 {
		t.Error("Plex was asked without a token")
	}
}

func TestPosterForATitleComesFromSeerrByItsTMDBId(t *testing.T) {
	st := newStack(t)
	st.seerr.routes["/api/v1/movie/100"] = `{"title":"Ready Movie","releaseDate":"2024-01-01","posterPath":"/abc123.jpg"}`
	st.seerr.routes["/api/v1/tv/200"] = `{"name":"Slow Show","firstAirDate":"2022-02-02","posterPath":"/tv200.png"}`
	st.seerr.routes["/imageproxy/tmdb/t/p/w185/abc123.jpg"] = string(jpegBytes)
	st.seerr.routes["/imageproxy/tmdb/t/p/w185/tv200.png"] = string(pngBytes)
	rs, cfg := newStackService(t, st, stackKeys)
	listSnapshot(rs, cfg, posterRef{"tmdbmovie", 100}, posterRef{"tmdbtv", 200})
	for i := 0; i < 3; i++ {
		if _, ctype, ok := rs.poster(context.Background(), cfg, posterRef{"tmdbmovie", 100}); !ok || ctype != "image/jpeg" {
			t.Fatalf("movie, ask %d: ok=%v type=%q", i+1, ok, ctype)
		}
		if _, ctype, ok := rs.poster(context.Background(), cfg, posterRef{"tmdbtv", 200}); !ok || ctype != "image/png" {
			t.Fatalf("show, ask %d: ok=%v type=%q", i+1, ok, ctype)
		}
	}
	if n := st.seerr.count("/imageproxy/"); n != 2 {
		t.Errorf("Seerr was asked for %d pictures; each should be fetched once", n)
	}
	if st.sonarr.count("")+st.radarr.count("")+st.plex.count("/photo/") != 0 {
		t.Error("another service was asked for a picture Seerr had")
	}
}

func TestPosterFromSeerrNeedsSeerrsKey(t *testing.T) {
	st := newStack(t)
	st.seerr.routes["/api/v1/movie/100"] = `{"posterPath":"/abc123.jpg"}`
	st.seerr.routes["/imageproxy/tmdb/t/p/w185/abc123.jpg"] = string(jpegBytes)
	rs, cfg := newStackService(t, st, requestKeys{Plex: "k"}) // no Seerr key
	listSnapshot(rs, cfg, posterRef{"tmdbmovie", 100})
	if _, _, ok := rs.poster(context.Background(), cfg, posterRef{"tmdbmovie", 100}); ok {
		t.Error("served without a Seerr key")
	}
	if st.seerr.count("") != 0 {
		t.Error("Seerr was asked without a key")
	}
}

// ---------- telling the admin why posters aren't loading ----------

func TestPosterHealthReportsAServiceOnlyIfNoneOfItsPostersHaveEverWorked(t *testing.T) {
	h := newPosterHealth()
	if len(h.problems()) != 0 {
		t.Fatalf("a fresh one reports %v", h.problems())
	}
	h.note("movie", errors.New("Radarr answered 401 Unauthorized"))
	h.note("show", errors.New("Sonarr answered 404 Not Found"))
	h.note("show", nil) // one worked, so the 404 was just a title with no cover
	h.note("plex", errors.New("Plex answered 500 Internal Server Error"))
	h.note("tmdbtv", errors.New("Seerr: no poster for it\nsecond line"))
	want := []string{
		"Posters from Plex aren't loading: Plex answered 500 Internal Server Error",
		"Posters from Radarr aren't loading: Radarr answered 401 Unauthorized",
		"Posters from Seerr aren't loading: Seerr: no poster for it second line",
	}
	if got := h.problems(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	h.note("movie", nil)
	h.note("tmdbmovie", nil) // the other Seerr kind counts for Seerr too
	if got := h.problems(); len(got) != 1 || !strings.Contains(got[0], "Plex") {
		t.Fatalf("after Radarr and Seerr recovered: %v", got)
	}
	h.note("music", errors.New("x")) // not one of ours
	if len(h.problems()) != 1 {
		t.Fatalf("an unknown kind was reported: %v", h.problems())
	}
}

// ---------- the endpoint ----------

// posterAPI is the real handler with the poster route, a service whose list is
// faked, and a pretend Sonarr, Radarr, Plex and Seerr that serve pictures.
type posterAPI struct {
	h                           http.Handler
	rs                          *requestStatus
	ck                          *clock
	builds                      *atomic.Int32
	sonarr, radarr, plex, seerr *coverSrv
}

type coverSrv struct {
	*httptest.Server
	hits atomic.Int32
	mu   sync.Mutex
	keys []string
	body []byte
	hdr  http.Header
	code int
	json string // if set, anything under /api/ answers with this (Seerr's title lookup)
}

func newCoverSrv(t *testing.T) *coverSrv {
	t.Helper()
	c := &coverSrv{body: jpegBytes, code: http.StatusOK}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.hits.Add(1)
		c.mu.Lock()
		key := r.Header.Get("X-Api-Key")
		if key == "" {
			key = r.Header.Get("X-Plex-Token")
		}
		c.keys = append(c.keys, key)
		body, hdr, code, js := c.body, c.hdr, c.code, c.json
		c.mu.Unlock()
		if js != "" && strings.HasPrefix(r.URL.Path, "/api/") {
			w.Write([]byte(js))
			return
		}
		for k, v := range hdr {
			w.Header()[k] = v
		}
		w.WriteHeader(code)
		w.Write(body)
	}))
	t.Cleanup(c.Close)
	return c
}

func (c *coverSrv) serve(body []byte, hdr http.Header) {
	c.mu.Lock()
	c.body, c.hdr = body, hdr
	c.mu.Unlock()
}

func (c *coverSrv) setCode(code int) {
	c.mu.Lock()
	c.code = code
	c.mu.Unlock()
}

func newPosterAPI(t *testing.T, burst int) *posterAPI {
	t.Helper()
	pa := &posterAPI{ck: &clock{t: t0}, builds: &atomic.Int32{}, sonarr: newCoverSrv(t), radarr: newCoverSrv(t), plex: newCoverSrv(t), seerr: newCoverSrv(t)}
	pa.seerr.json = `{"title":"Something","posterPath":"/abc123.jpg"}`
	store := storeFor(t, `
title: Test
contact: Alex
people:
  admin@example.com: {name: Admin, groups: [admin, family]}
  mom@example.com:   {name: Mom, groups: [family]}
  dad@example.com:   {name: Dad, groups: [nope]}
apps:
  - {name: Seerr, url: https://seerr.test/, access: [family], check: "off"}
  - {name: Other, url: https://other.test/, access: [nope], check: "off"}
requests:
  app: seerr
  seerr: `+pa.seerr.URL+`
  plex: `+pa.plex.URL+`
  sonarr: `+pa.sonarr.URL+`
  radarr: `+pa.radarr.URL+`
`)
	rs := newRequestStatus(store, requestKeys{Seerr: "seerr-key", Plex: "plex-token", Sonarr: "sonarr-key", Radarr: "radarr-key"}, true)
	rs.now = pa.ck.Now
	rs.forced.now = pa.ck.Now
	rs.build = func(ctx context.Context, c *Config) (*snapshot, error) {
		pa.builds.Add(1)
		return &snapshot{
			PlexOK: true,
			Rows: []requestRow{
				{Title: "Slow Show", Kind: "show", State: "partial", Posters: []string{"show/9"}},
				{Title: "Dune", Kind: "movie", State: "ready", Posters: []string{"plex/10", "tmdbmovie/603"}},
				{Title: "Soon", Kind: "movie", State: "soon", Posters: []string{"movie/30"}},
				{Title: "Waiting", Kind: "movie", State: "pending", Posters: []string{"tmdbmovie/1001"}},
				{Title: "Nothing", Kind: "movie", State: "pending"},
			},
			Coming: &comingList{Rows: []comingRow{{Title: "Cinema Movie", Kind: "movie", Date: "2026-10-20", Note: "cinemas", Posters: []string{"movie/40"}}}},
		}, nil
	}
	reads := newRateLimiter(time.Millisecond, 3600)
	reads.now = pa.ck.Now
	posters := newBucketLimiter(burst, time.Second)
	posters.now = pa.ck.Now
	h := &handler{store: store, checker: NewChecker(store), requests: rs, reqReads: reads, posterReads: posters}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/requests", h.requestsList)
	mux.HandleFunc("GET /api/requests/poster/{kind}/{id}", h.requestsPoster)
	pa.h, pa.rs = blockFunnel(securityHeaders(mux)), rs
	return pa
}

func (pa *posterAPI) get(path string, opts ...reqOpt) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = loopback
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	pa.h.ServeHTTP(rec, req)
	return rec
}

// open is what the page does first: load the list, which is what makes the
// pictures on it askable.
func (pa *posterAPI) open(t *testing.T) {
	t.Helper()
	pa.ck.advance(3 * time.Second)
	if rec := pa.get("/api/requests", as("mom@example.com")); rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
}

func TestPosterEndpointChecksWhoIsAskingAndTheirAccess(t *testing.T) {
	pa := newPosterAPI(t, 100)
	pa.open(t)
	denied := map[string][]reqOpt{
		"nobody":                      nil,
		"a stranger":                  {as("stranger@x.com")},
		"known, but no Seerr access":  {as("dad@example.com")},
		"a forged login from the LAN": {as("mom@example.com"), from("192.168.1.9:5000")},
		"a forged admin from the LAN": {as("admin@example.com"), from("[2001:db8::1]:5000")},
	}
	var first string
	for who, opts := range denied {
		rec := pa.get("/api/requests/poster/show/9", opts...)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: got %d", who, rec.Code)
		}
		if first == "" {
			first = rec.Body.String()
		} else if rec.Body.String() != first {
			t.Errorf("%s: a different answer from the others", who)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control %q", who, rec.Header().Get("Cache-Control"))
		}
	}
	if pa.sonarr.hits.Load() != 0 {
		t.Fatal("somebody who may not look made Anchor ask Sonarr")
	}
	for _, who := range []string{"mom@example.com", "admin@example.com"} {
		if rec := pa.get("/api/requests/poster/show/9", as(who)); rec.Code != http.StatusOK {
			t.Errorf("%s: got %d", who, rec.Code)
		}
	}
	// "Not on the sheet" says the same thing as "not allowed".
	if rec := pa.get("/api/requests/poster/show/10", as("mom@example.com")); rec.Code != http.StatusNotFound || rec.Body.String() != first {
		t.Errorf("an unlisted picture: %d %q", rec.Code, rec.Body.String())
	}
}

func TestPosterEndpointServesTitlesFromBothTabsAndNothingElse(t *testing.T) {
	pa := newPosterAPI(t, 100)
	pa.open(t)
	for path, want := range map[string]int{
		"/api/requests/poster/show/9":   http.StatusOK, // a Requests row, from Sonarr
		"/api/requests/poster/plex/10":  http.StatusOK, // a Requests row for a movie in Plex, from Plex
		"/api/requests/poster/movie/30": http.StatusOK, // a Requests row for a movie not in Plex yet, from Radarr
		"/api/requests/poster/movie/40": http.StatusOK, // a Coming up row, from Radarr
		"/api/requests/poster/movie/9":  http.StatusNotFound,
		"/api/requests/poster/show/30":  http.StatusNotFound,
		"/api/requests/poster/show/1":   http.StatusNotFound, // somebody else's title in Sonarr
		"/api/requests/poster/movie/10": http.StatusNotFound, // 10 is a Plex number, not Radarr's
		"/api/requests/poster/plex/30":  http.StatusNotFound, // and 30 is a Radarr number, not Plex's
		"/api/requests/poster/plex/9":   http.StatusNotFound,

		"/api/requests/poster/tmdbmovie/603":  http.StatusOK,       // Seerr's copy, for the movie in Plex (a second choice)
		"/api/requests/poster/tmdbmovie/1001": http.StatusOK,       // Seerr's copy, for a request waiting for approval
		"/api/requests/poster/tmdbtv/603":     http.StatusNotFound, // 603 is listed as a movie, not as a show
		"/api/requests/poster/tmdbmovie/9":    http.StatusNotFound,
	} {
		if rec := pa.get(path, as("mom@example.com")); rec.Code != want {
			t.Errorf("%s: got %d, want %d", path, rec.Code, want)
		}
	}
	// Seerr is asked twice for each of its two: the title, then the picture.
	if pa.sonarr.hits.Load() != 1 || pa.radarr.hits.Load() != 2 || pa.plex.hits.Load() != 1 || pa.seerr.hits.Load() != 4 {
		t.Errorf("Sonarr %d, Radarr %d, Plex %d, Seerr %d requests", pa.sonarr.hits.Load(), pa.radarr.hits.Load(), pa.plex.hits.Load(), pa.seerr.hits.Load())
	}
}

func TestPosterEndpointRefusesOddAddressesWithoutAskingAnyone(t *testing.T) {
	pa := newPosterAPI(t, 1000)
	pa.open(t)
	for _, p := range []string{
		"/api/requests/poster/tv/9", "/api/requests/poster/Show/9", "/api/requests/poster/show/0", "/api/requests/poster/show/-1",
		"/api/requests/poster/show/09", "/api/requests/poster/show/1e3", "/api/requests/poster/show/9999999999",
		"/api/requests/poster/show/9/extra", "/api/requests/poster/show/", "/api/requests/poster/show/9%2f..%2f..",
		"/api/requests/poster/show/9?id=10", "/api/requests/poster/../requests", "/api/requests/poster//9",
		"/api/requests/poster/http:%2f%2fevil.example%2fx/9", "/api/requests/poster/show/%39",
	} {
		rec := pa.get(p, as("mom@example.com"))
		if p == "/api/requests/poster/show/9?id=10" || p == "/api/requests/poster/show/%39" {
			// The query is ignored, and %39 is simply "9": these are the listed picture.
			if rec.Code != http.StatusOK {
				t.Errorf("%s: got %d", p, rec.Code)
			}
			continue
		}
		// Paths that need tidying ("..", "//") are sent to the tidy address by
		// the router; none of them gets anywhere near a service.
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusTemporaryRedirect && rec.Code != http.StatusMovedPermanently {
			t.Errorf("%s: got %d", p, rec.Code)
		}
	}
	if got := pa.sonarr.hits.Load(); got > 1 {
		t.Fatalf("Sonarr was asked %d times; only the listed picture should ever be", got)
	}
	if pa.radarr.hits.Load() != 0 {
		t.Fatal("Radarr was asked")
	}
}

func TestPosterEndpointAnswersInAnchorsOwnWords(t *testing.T) {
	pa := newPosterAPI(t, 100)
	pa.sonarr.serve(jpegBytes, http.Header{
		"Content-Type":        {"text/html"},
		"Set-Cookie":          {"session=upstream-secret"},
		"Content-Disposition": {"attachment; filename=evil.html"},
		"X-Evil":              {"yes"},
		"Location":            {"http://evil.example/"},
		"Cache-Control":       {"public, max-age=31536000"},
	})
	pa.open(t)
	rec := pa.get("/api/requests/poster/show/9", as("mom@example.com"))
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), jpegBytes) {
		t.Fatalf("got %d, %d bytes", rec.Code, rec.Body.Len())
	}
	want := map[string]string{
		"Content-Type":                 "image/jpeg",
		"Cache-Control":                "no-store",
		"X-Content-Type-Options":       "nosniff",
		"Content-Security-Policy":      "default-src 'none'",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Content-Length":               strconv.Itoa(len(jpegBytes)),
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s: %q, want %q", k, got, v)
		}
	}
	for _, k := range []string{"Set-Cookie", "Content-Disposition", "X-Evil", "Location"} {
		if v := rec.Header().Get(k); v != "" {
			t.Errorf("the service's %s reached the phone: %q", k, v)
		}
	}
}

func TestPosterEndpointSendsEachServiceItsOwnKey(t *testing.T) {
	pa := newPosterAPI(t, 100)
	pa.open(t)
	pa.get("/api/requests/poster/show/9", as("mom@example.com"))
	pa.get("/api/requests/poster/movie/30", as("mom@example.com"))
	pa.get("/api/requests/poster/plex/10", as("mom@example.com"))
	pa.get("/api/requests/poster/tmdbmovie/603", as("mom@example.com"))
	if !reflect.DeepEqual(pa.sonarr.keys, []string{"sonarr-key"}) || !reflect.DeepEqual(pa.radarr.keys, []string{"radarr-key"}) ||
		!reflect.DeepEqual(pa.plex.keys, []string{"plex-token"}) || !reflect.DeepEqual(pa.seerr.keys, []string{"seerr-key", "seerr-key"}) {
		t.Fatalf("Sonarr got %v, Radarr got %v, Plex got %v, Seerr got %v", pa.sonarr.keys, pa.radarr.keys, pa.plex.keys, pa.seerr.keys)
	}
}

func TestPosterEndpointNeverStartsARefresh(t *testing.T) {
	pa := newPosterAPI(t, 100)
	if rec := pa.get("/api/requests/poster/show/9", as("mom@example.com")); rec.Code != http.StatusNotFound {
		t.Fatalf("got %d before anyone had opened the sheet", rec.Code)
	}
	pa.open(t)
	before := pa.builds.Load()
	pa.ck.advance(time.Hour) // the list is long out of date
	for i := 0; i < 3; i++ {
		pa.get("/api/requests/poster/show/9", as("mom@example.com"))
	}
	if pa.builds.Load() != before {
		t.Fatalf("asking for a picture rebuilt the list (%d -> %d)", before, pa.builds.Load())
	}
}

func TestPosterEndpointDoesNotServeWhatIsNotAPictureAndRemembersIt(t *testing.T) {
	pa := newPosterAPI(t, 100)
	pa.sonarr.serve([]byte("<script>alert(1)</script>"), nil)
	pa.open(t)
	for i := 0; i < 3; i++ {
		rec := pa.get("/api/requests/poster/show/9", as("mom@example.com"))
		if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "alert") {
			t.Fatalf("got %d %q", rec.Code, rec.Body.String())
		}
	}
	// Both of its addresses are tried once, and then the answer is remembered.
	if pa.sonarr.hits.Load() != 2 {
		t.Fatalf("Sonarr was asked %d times for a picture it can't give", pa.sonarr.hits.Load())
	}
}

func TestPosterEndpointIsLimitedPerPerson(t *testing.T) {
	pa := newPosterAPI(t, 3)
	pa.open(t)
	for i := 0; i < 3; i++ {
		if rec := pa.get("/api/requests/poster/show/9", as("mom@example.com")); rec.Code != http.StatusOK {
			t.Fatalf("request %d: %d", i+1, rec.Code)
		}
	}
	rec := pa.get("/api/requests/poster/show/9", as("mom@example.com"))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("got %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if rec := pa.get("/api/requests/poster/show/9", as("admin@example.com")); rec.Code != http.StatusOK {
		t.Fatalf("one person's use limited another: %d", rec.Code)
	}
	pa.ck.advance(2 * time.Second)
	if rec := pa.get("/api/requests/poster/show/9", as("mom@example.com")); rec.Code != http.StatusOK {
		t.Fatalf("still limited after waiting: %d", rec.Code)
	}
}

func TestPosterEndpointIsNotThereWhenTheFeatureIsOff(t *testing.T) {
	store := storeFor(t, apiConfigOff)
	rs := newRequestStatus(store, requestKeys{}, false)
	h := &handler{store: store, checker: NewChecker(store), requests: rs, reqReads: newRateLimiter(time.Millisecond, 3600), posterReads: newBucketLimiter(10, time.Second)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/requests/poster/{kind}/{id}", h.requestsPoster)
	req := httptest.NewRequest(http.MethodGet, "/api/requests/poster/show/9", nil)
	req.RemoteAddr = loopback
	req.Header.Set("Tailscale-User-Login", "mom@example.com")
	rec := httptest.NewRecorder()
	blockFunnel(securityHeaders(mux)).ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestTheListTellsThePageWhichTitlesHavePosters(t *testing.T) {
	pa := newPosterAPI(t, 100)
	pa.ck.advance(3 * time.Second)
	rec := pa.get("/api/requests", as("mom@example.com"))
	out := decodeRequests(t, rec)
	var got []string
	for _, r := range out.Rows {
		got = append(got, r.Title+"|"+strings.Join(r.Posters, ","))
	}
	want := []string{"Slow Show|show/9", "Dune|plex/10,tmdbmovie/603", "Soon|movie/30", "Waiting|tmdbmovie/1001", "Nothing|"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows: %v", got)
	}
	if out.Coming == nil || !reflect.DeepEqual(out.Coming.Rows[0].Posters, []string{"movie/40"}) {
		t.Fatalf("coming: %+v", out.Coming)
	}
	for _, leak := range []string{"MediaCover", "mediacover", "imageproxy", "posterPath", pa.sonarr.URL, pa.seerr.URL, pa.plex.URL, "transcode"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Fatalf("%q reached the page", leak)
		}
	}
	// Nothing but the short names: a kind, a slash, a number.
	for _, r := range out.Rows {
		for _, p := range r.Posters {
			if _, ok := parsePosterString(p); !ok {
				t.Errorf("%s: %q isn't a plain poster name", r.Title, p)
			}
		}
	}
}

// When none of a service's posters ever loads, the admin (and only the admin)
// is told why, at the bottom of the sheet; a few missing ones aren't news.
func TestAdminsSeeWhyPostersAreNotLoadingAndTheFamilyDoesNot(t *testing.T) {
	pa := newPosterAPI(t, 100)
	pa.sonarr.setCode(http.StatusUnauthorized)
	pa.open(t)
	if rec := pa.get("/api/requests/poster/show/9", as("mom@example.com")); rec.Code != http.StatusNotFound {
		t.Fatalf("got %d", rec.Code)
	}
	problems := func(who string) []string {
		pa.ck.advance(3 * time.Second)
		return decodeRequests(t, pa.get("/api/requests", as(who))).Problems
	}
	got := problems("admin@example.com")
	if len(got) != 1 || !strings.Contains(got[0], "Sonarr") || !strings.Contains(got[0], "401") {
		t.Fatalf("admin sees %v", got)
	}
	if got := problems("mom@example.com"); len(got) != 0 {
		t.Fatalf("the family sees %v", got)
	}
	// Once one works, it is just a title that has no cover.
	pa.sonarr.setCode(http.StatusOK)
	pa.ck.advance(11 * time.Minute) // past the time a miss is remembered
	if rec := pa.get("/api/requests/poster/show/9", as("mom@example.com")); rec.Code != http.StatusOK {
		t.Fatalf("after the fix: %d", rec.Code)
	}
	if got := problems("admin@example.com"); len(got) != 0 {
		t.Fatalf("admin still sees %v", got)
	}
}
