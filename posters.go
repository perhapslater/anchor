package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Posters. Rows on the Request status sheet can show a title's poster. A title
// can have several places its poster might come from, and the page tries them
// in this order until one works:
//
//	Plex    its own copy, scaled by Plex to 250 pixels wide, if the title is in Plex
//	Seerr   a copy of TMDB's picture, found by the title's TMDB id
//	Sonarr  the small cover (also 250 wide) Sonarr or Radarr keep for every
//	Radarr  title they track, found by their own id for it
//
// Anchor fetches the picture and hands it to the family's phones. Phones can't
// go to those apps themselves: they are on the home network behind a key.
//
// What keeps this narrow:
//
//   - a phone never says where a picture comes from. It names one on a title
//     that is on the sheet (a kind and a number, like plex/10), and Anchor works
//     out the address itself
//   - only the names on the sheet as it is now can be asked for, so nobody can
//     walk through the whole library by trying numbers
//   - Anchor asks for a few fixed address patterns, through the same guarded
//     client as everything else (see upstream.go), and passes on only the
//     picture, never anything the service sent with it
//   - Anchor keeps what it fetched for a day, in memory only and capped, so
//     opening the sheet doesn't mean asking the apps again
//   - fetches are limited in number and time, and shared between phones

const (
	posterMax     = 512 << 10        // the largest picture accepted from a service
	posterTTL     = 24 * time.Hour   // how long a fetched picture is kept
	posterMissTTL = 10 * time.Minute // how long "no picture" is remembered
	posterEntries = 120              // pictures (and misses) kept at most
	posterBytes   = 8 << 20          // and no more than this much memory
	posterFetches = 4                // fetches from the services at once
	posterTimeout = 10 * time.Second // one fetch, including waiting its turn
)

// The only picture addresses Anchor asks for. Sonarr and Radarr have two for a
// cover: the API's, meant for an API key, and the one their own web page uses,
// which may want a login instead. 250 is the width they keep ready-made, so it
// is small. Seerr's is its copy of TMDB's 185-pixel poster.
const (
	coverAPIPattern   = `^/api/v3/mediacover/[0-9]{1,9}/poster-250\.jpg$`
	coverUIPattern    = `^/MediaCover/[0-9]{1,9}/poster-250\.jpg$`
	seerrImagePattern = `^/imageproxy/tmdb/t/p/w185/[A-Za-z0-9_-]{1,64}\.(jpg|png)$`
)

// fetchCover asks Sonarr or Radarr for the cover of one of its own titles, at
// the API's address and, if that fails, at the web page's.
func fetchCover(ctx context.Context, u *upstream, id int) ([]byte, string, error) {
	if !validID(id) {
		return nil, "", fmt.Errorf("%s: bad id", u.name)
	}
	n := strconv.Itoa(id)
	data, ctype, err := u.getImage(ctx, "/api/v3/mediacover/"+n+"/poster-250.jpg", nil, posterMax)
	if err == nil {
		return data, ctype, nil
	}
	if data, ctype, err2 := u.getImage(ctx, "/MediaCover/"+n+"/poster-250.jpg", nil, posterMax); err2 == nil {
		return data, ctype, nil
	}
	return nil, "", err // the first answer says more than the second
}

// posterRef names a picture: Kind says where it comes from and ID is that
// service's own number for the title.
//
//	plex       Plex's number for it (its rating key)
//	tmdbmovie  a movie's TMDB id, for Seerr's copy of TMDB's picture
//	tmdbtv     a show's TMDB id, likewise
//	movie      Radarr's id for the movie
//	show       Sonarr's id for the show
type posterRef struct {
	Kind string
	ID   int
}

var posterIDForm = regexp.MustCompile(`^[1-9][0-9]{0,8}$`)

// posterKind reports whether kind is one of the places a picture can come from.
func posterKind(kind string) bool {
	switch kind {
	case "plex", "tmdbmovie", "tmdbtv", "movie", "show":
		return true
	}
	return false
}

// parsePosterRef reads the two parts of a picture's address on Anchor. Only
// plain numbers are accepted, in one spelling each.
func parsePosterRef(kind, id string) (posterRef, bool) {
	if !posterKind(kind) || !posterIDForm.MatchString(id) {
		return posterRef{}, false
	}
	n, err := strconv.Atoi(id)
	if err != nil || !validID(n) {
		return posterRef{}, false
	}
	return posterRef{kind, n}, true
}

// parsePosterString reads a poster name as the list gives it, "plex/10".
func parsePosterString(s string) (posterRef, bool) {
	kind, id, ok := strings.Cut(s, "/")
	if !ok {
		return posterRef{}, false
	}
	return parsePosterRef(kind, id)
}

func (p posterRef) String() string { return p.Kind + "/" + strconv.Itoa(p.ID) }

// posterID is id if it could be a real one, and 0 if not.
func posterID(id int) int {
	if validID(id) {
		return id
	}
	return 0
}

// plexNumber reads a Plex rating key from the library listing, 0 if there isn't
// a usable one.
func plexNumber(key string) int {
	n, err := strconv.Atoi(key)
	if err != nil {
		return 0
	}
	return posterID(n)
}

// posterRefs lists the places a title's poster may come from, best first (see
// the top of this file). kind is movie or show. A number of 0, or one that
// couldn't be real, means that place doesn't have the title and is left out.
func posterRefs(kind string, plexKey, tmdb, arrID int) []string {
	if kind != "movie" && kind != "show" {
		return nil
	}
	tmdbKind := "tmdbmovie"
	if kind == "show" {
		tmdbKind = "tmdbtv"
	}
	var out []string
	for _, r := range []posterRef{{"plex", plexKey}, {tmdbKind, tmdb}, {kind, arrID}} {
		if validID(r.ID) {
			out = append(out, r.String())
		}
	}
	return out
}

// comingPosters works out where each Coming up row's poster may come from. The
// calendars give Sonarr's and Radarr's ids and TVDB and TMDB ids; Plex's own
// number for a title comes from the library listing, if Plex could be read.
func comingPosters(rows []comingRow, idx *plexIndex) {
	for i := range rows {
		r := &rows[i]
		plexKey := 0
		if idx != nil {
			if r.Kind == "movie" {
				plexKey = plexNumber(idx.movieKeys[r.tmdb])
			} else {
				key := ""
				if r.tvdb > 0 {
					key = idx.showsTVDB[r.tvdb]
				}
				if key == "" && r.tmdb > 0 {
					key = idx.showsTMDB[r.tmdb]
				}
				plexKey = plexNumber(key)
			}
		}
		r.Posters = posterRefs(r.Kind, plexKey, r.tmdb, r.arrID)
	}
}

// listedPosters is every picture the sheet may ask for: each name on each row,
// on either tab. Anything that isn't a plain, well-formed name is ignored.
func listedPosters(snap *snapshot) map[posterRef]bool {
	out := map[posterRef]bool{}
	add := func(names []string) {
		for _, s := range names {
			if ref, ok := parsePosterString(s); ok {
				out[ref] = true
			}
		}
	}
	for _, r := range snap.Rows {
		add(r.Posters)
	}
	if snap.Coming != nil {
		for _, r := range snap.Coming.Rows {
			add(r.Posters)
		}
	}
	return out
}

// poster returns the picture for a name on the sheet. It never starts a refresh
// of the list: if the sheet hasn't been opened lately, the answer is simply no.
func (rs *requestStatus) poster(ctx context.Context, cfg *Config, ref posterRef) ([]byte, string, bool) {
	if cfg.Requests == nil {
		return nil, "", false
	}
	sig := cfg.Requests.sig()
	rs.mu.Lock()
	cur := rs.current(sig)
	listed := cur != nil && cur.posters[ref]
	rs.mu.Unlock()
	if !listed {
		return nil, "", false
	}
	rc := cfg.Requests
	return rs.posters.get(ctx, posterKey{sig: sig, ref: ref}, func(ctx context.Context) ([]byte, string, error) {
		data, ctype, err := rs.fetchPoster(ctx, rc, ref)
		rs.health.note(ref.Kind, err)
		return data, ctype, err
	})
}

func (rs *requestStatus) fetchPoster(ctx context.Context, rc *Requests, ref posterRef) ([]byte, string, error) {
	switch ref.Kind {
	case "plex":
		if rc.Plex == "" || rs.keysFor(rc).Plex == "" {
			return nil, "", errors.New("Plex is not set up")
		}
		p, err := newPlex(rc.Plex, rs.keysFor(rc).Plex, rs.client)
		if err != nil {
			return nil, "", err
		}
		return p.poster(ctx, ref.ID)
	case "tmdbmovie", "tmdbtv":
		if rs.keysFor(rc).Seerr == "" {
			return nil, "", errors.New("Seerr is not set up")
		}
		s, err := newSeerr(rc.Seerr, rs.keysFor(rc).Seerr, rs.client)
		if err != nil {
			return nil, "", err
		}
		kind := "movie"
		if ref.Kind == "tmdbtv" {
			kind = "show"
		}
		return s.poster(ctx, kind, ref.ID)
	case "show":
		if rc.Sonarr == "" || rs.keysFor(rc).Sonarr == "" {
			return nil, "", errors.New("Sonarr is not set up")
		}
		s, err := newSonarr(rc.Sonarr, rs.keysFor(rc).Sonarr, rs.client)
		if err != nil {
			return nil, "", err
		}
		return s.poster(ctx, ref.ID)
	case "movie":
		if rc.Radarr == "" || rs.keysFor(rc).Radarr == "" {
			return nil, "", errors.New("Radarr is not set up")
		}
		r, err := newRadarr(rc.Radarr, rs.keysFor(rc).Radarr, rs.client)
		if err != nil {
			return nil, "", err
		}
		return r.poster(ctx, ref.ID)
	}
	return nil, "", errors.New("unknown kind")
}

// ---------- telling the admin why posters aren't loading ----------

// posterHealth remembers, per service, whether any of its posters has ever
// loaded since Anchor started. A service none of whose posters has worked is
// reported to the admin with the last reason it gave; a few titles with no
// picture are normal and aren't news, so one success silences the report.
type posterHealth struct {
	mu   sync.Mutex
	ok   map[string]bool
	last map[string]string
}

func newPosterHealth() *posterHealth {
	return &posterHealth{ok: map[string]bool{}, last: map[string]string{}}
}

// posterService names the app a kind of picture comes from.
func posterService(kind string) string {
	switch kind {
	case "plex":
		return "Plex"
	case "tmdbmovie", "tmdbtv":
		return "Seerr"
	case "movie":
		return "Radarr"
	case "show":
		return "Sonarr"
	}
	return ""
}

// note records how one fetch went (err nil: it worked).
func (h *posterHealth) note(kind string, err error) {
	svc := posterService(kind)
	if svc == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if err == nil {
		h.ok[svc] = true
		return
	}
	h.last[svc] = cleanText(err.Error(), 200)
}

// problems is one line for each service none of whose posters has loaded.
func (h *posterHealth) problems() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for svc, why := range h.last {
		if !h.ok[svc] {
			out = append(out, fmt.Sprintf("Posters from %s aren't loading: %s", svc, why))
		}
	}
	sort.Strings(out)
	return out
}

// ---------- keeping pictures ----------

// posterKey is one picture. The settings are part of it, so if an address in
// anchor.yaml changes, pictures from the old one are not reused.
type posterKey struct {
	sig  string
	ref  posterRef
	item string // non-numeric ebook ID; an independent cache uses the same image policy
}

type posterEntry struct {
	data  []byte // nil: Anchor asked and got no picture
	ctype string
	at    time.Time
	seq   uint64
}

type posterCall struct{ done chan struct{} }

// posterCache keeps pictures in memory for a day (and failures for ten
// minutes), at most posterEntries of them and posterBytes in all, oldest
// first out. Phones asking for the same picture at once share one fetch, and
// only posterFetches fetches run at a time. A fetch belongs to Anchor, not to
// the phone that asked: if that phone leaves, it carries on for the next one.
type posterCache struct {
	ctx    context.Context
	mu     sync.Mutex
	m      map[posterKey]*posterEntry
	bytes  int
	seq    uint64
	flight map[posterKey]*posterCall
	sem    chan struct{}
	now    func() time.Time
}

func newPosterCache() *posterCache {
	return &posterCache{
		ctx:    context.Background(),
		m:      map[posterKey]*posterEntry{},
		flight: map[posterKey]*posterCall{},
		sem:    make(chan struct{}, posterFetches),
		now:    time.Now,
	}
}

// get returns the picture, fetching it first if Anchor doesn't have it.
func (c *posterCache) get(ctx context.Context, key posterKey, fetch func(context.Context) ([]byte, string, error)) ([]byte, string, bool) {
	c.mu.Lock()
	if e := c.fresh(key); e != nil {
		c.mu.Unlock()
		return e.data, e.ctype, e.data != nil
	}
	call := c.flight[key]
	if call == nil {
		call = &posterCall{done: make(chan struct{})}
		c.flight[key] = call
		go c.run(key, call, fetch)
	}
	c.mu.Unlock()

	select {
	case <-call.done:
	case <-ctx.Done(): // this phone left; the fetch carries on for the others
		return nil, "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.fresh(key); e != nil {
		return e.data, e.ctype, e.data != nil
	}
	return nil, "", false
}

// fresh is the entry for key if it is still good. The caller holds the lock.
func (c *posterCache) fresh(key posterKey) *posterEntry {
	e := c.m[key]
	if e == nil {
		return nil
	}
	ttl := posterTTL
	if e.data == nil {
		ttl = posterMissTTL
	}
	if c.now().Sub(e.at) > ttl {
		c.drop(key)
		return nil
	}
	return e
}

func (c *posterCache) drop(key posterKey) {
	if e := c.m[key]; e != nil {
		c.bytes -= len(e.data)
		delete(c.m, key)
	}
}

// run does one fetch and wakes everyone waiting for it.
func (c *posterCache) run(key posterKey, call *posterCall, fetch func(context.Context) ([]byte, string, error)) {
	data, ctype, err := c.load(fetch)
	if err != nil {
		if key.item != "" {
			log.Printf("ebook cover unavailable: %v", err)
		} else {
			log.Printf("poster %s/%d: %v", key.ref.Kind, key.ref.ID, err)
		}
	}
	c.mu.Lock()
	c.drop(key)
	c.seq++
	c.m[key] = &posterEntry{data: data, ctype: ctype, at: c.now(), seq: c.seq}
	c.bytes += len(data)
	for len(c.m) > posterEntries || c.bytes > posterBytes {
		var oldest posterKey
		var lowest uint64
		for k, e := range c.m {
			if lowest == 0 || e.seq < lowest {
				oldest, lowest = k, e.seq
			}
		}
		c.drop(oldest)
	}
	delete(c.flight, key)
	c.mu.Unlock()
	close(call.done)
}

// load waits for a free place among the fetches, then fetches. A panic or a
// failure is a miss, never a crash.
func (c *posterCache) load(fetch func(context.Context) ([]byte, string, error)) (data []byte, ctype string, err error) {
	ctx, cancel := context.WithTimeout(c.ctx, posterTimeout)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			data, ctype, err = nil, "", fmt.Errorf("unexpected problem: %v", p)
		}
	}()
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return nil, "", errors.New("too many pictures being fetched")
	}
	data, ctype, err = fetch(ctx)
	if err == nil && (len(data) == 0 || len(data) > posterMax) {
		err = errors.New("not a usable picture")
	}
	if err != nil {
		return nil, "", err
	}
	return data, ctype, nil
}

// stats says how many entries are kept and how many bytes they hold.
func (c *posterCache) stats() (entries, bytes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m), c.bytes
}

// ---------- limits for one person ----------

// bucketLimiter lets each person make a burst of requests and then a steady
// trickle: opening the sheet asks for a screenful of pictures at once, which
// the one-request-every-two-seconds limit on the list would refuse.
type bucketLimiter struct {
	mu     sync.Mutex
	burst  float64
	refill time.Duration // one more request allowed per this
	m      map[string]*bucket
	now    func() time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

func newBucketLimiter(burst int, refill time.Duration) *bucketLimiter {
	return &bucketLimiter{burst: float64(burst), refill: refill, m: map[string]*bucket{}, now: time.Now}
}

func (l *bucketLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.m[key]
	if b == nil {
		if len(l.m) >= 1000 { // only known people get this far, so this is just a safety net
			l.m = map[string]*bucket{}
		}
		b = &bucket{tokens: l.burst, at: now}
		l.m[key] = b
	} else {
		b.tokens = min(l.burst, b.tokens+float64(now.Sub(b.at))/float64(l.refill))
		b.at = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
