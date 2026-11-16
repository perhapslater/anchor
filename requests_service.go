package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The shared snapshot behind the Request status sheet, and how it is built.
// Nothing runs in the background: a refresh happens only when somebody opens
// the sheet and the copy is old, or presses Refresh now, one at a time and
// with limits on how often (see the constants).

const (
	freshFor      = 5 * time.Minute  // a copy this young is served as it is
	forcedGap     = 30 * time.Second // Refresh now waits this long after any refresh
	forcedPerHour = 60               // and at most this many an hour, for everyone
	failBackoff   = 60 * time.Second // after a failed refresh, leave the services alone this long
	refreshLimit  = 25 * time.Second // one whole refresh
	parallelCalls = 4                // calls to the services at once
	maxRows       = 40               // titles shown
	seerrTake     = 60               // requests read, so that 40 remain after merging duplicates
	titleTTL      = 7 * 24 * time.Hour
	titleCacheMax = 500
)

var errNoData = errors.New("Request status is not available right now")

// snapshot is everything the sheet shows, as of one refresh. It lives only in
// memory and is never changed once built.
type snapshot struct {
	Rows     []requestRow
	Coming   *comingList // nil when neither Sonarr nor Radarr is configured
	Taken    time.Time
	PlexOK   bool
	Problems []string           // for admins: which service failed and how
	sig      string             // the requests: settings it was built from
	posters  map[posterRef]bool // the pictures the sheet may ask for (see posters.go)
}

type requestKeys struct{ Seerr, Plex, Sonarr, Radarr string }

// keysFromEnv reads the four keys. They live in container settings only.
func keysFromEnv() requestKeys {
	get := func(k string) string { return strings.TrimSpace(os.Getenv(k)) }
	return requestKeys{
		Seerr:  get("ANCHOR_SEERR_KEY"),
		Plex:   get("ANCHOR_PLEX_TOKEN"),
		Sonarr: get("ANCHOR_SONARR_KEY"),
		Radarr: get("ANCHOR_RADARR_KEY"),
	}
}

type requestResult struct {
	snap     *snapshot
	stale    bool // the last refresh failed, so this is older than it should be
	coolDown bool // Refresh now was too soon, or the services are being left alone
	err      error
}

type requestCall struct {
	done   chan struct{}
	sig    string
	result requestResult // immutable after done closes
}

type requestStatus struct {
	allowLoopback bool
	ctx           context.Context // shared work survives a visitor, but not server shutdown
	store         *ConfigStore
	keys          requestKeys
	client        *http.Client
	now           func() time.Time
	forced        *rateLimiter
	titles        *titleCache
	posters       *posterCache  // pictures, kept for a day
	health        *posterHealth // whether each service's posters are loading, for the admin
	// build makes a fresh snapshot. It is a field so tests can fake it.
	build func(ctx context.Context, cfg *Config) (*snapshot, error)

	mu          sync.Mutex
	snap        *snapshot
	flight      *requestCall // shared work and its configuration-specific result
	lastAttempt time.Time
	lastSig     string
	lastFailed  bool
	lastErr     error
}

func newRequestStatus(store *ConfigStore, keys requestKeys, allowLoopback bool) *requestStatus {
	rs := &requestStatus{
		allowLoopback: allowLoopback,
		ctx:           context.Background(),
		store:         store,
		keys:          keys,
		client:        newUpstreamClient(allowLoopback),
		now:           time.Now,
		titles:        newTitleCache(),
		posters:       newPosterCache(),
		health:        newPosterHealth(),
	}
	rs.forced = newRateLimiter(forcedGap, forcedPerHour)
	rs.forced.now = func() time.Time { return rs.now() }
	rs.posters.now = func() time.Time { return rs.now() }
	rs.build = rs.pull
	return rs
}

// get returns the snapshot, refreshing it first if it is over 5 minutes old.
func (rs *requestStatus) get(ctx context.Context, cfg *Config) requestResult {
	return rs.fetch(ctx, cfg, false)
}

// force is Refresh now: a refresh even though the copy is fresh, unless one
// just happened, or too many have, or the services are being left alone after
// a failure. In those cases the current copy comes back with coolDown set.
func (rs *requestStatus) force(ctx context.Context, cfg *Config) requestResult {
	return rs.fetch(ctx, cfg, true)
}

// noData is what to say when there is nothing to show: why the last refresh
// failed, if it did. The caller holds the lock.
func (rs *requestStatus) noData(sig string) error {
	if rs.lastSig == sig && rs.lastFailed && rs.lastErr != nil {
		return rs.lastErr
	}
	return errNoData
}

// missingKeys names the settings a configured feature needs but doesn't have.
func missingKeys(rc *Requests, k requestKeys) []string {
	var out []string
	if k.Seerr == "" {
		out = append(out, "ANCHOR_SEERR_KEY")
	}
	if k.Plex == "" {
		out = append(out, "ANCHOR_PLEX_TOKEN")
	}
	if rc.Sonarr != "" && k.Sonarr == "" {
		out = append(out, "ANCHOR_SONARR_KEY")
	}
	if rc.Radarr != "" && k.Radarr == "" {
		out = append(out, "ANCHOR_RADARR_KEY")
	}
	return out
}

// current is the snapshot, but only if it was built from these settings.
func (rs *requestStatus) current(sig string) *snapshot {
	if rs.snap != nil && rs.snap.sig == sig {
		return rs.snap
	}
	return nil
}

func (rs *requestStatus) fetch(ctx context.Context, cfg *Config, forced bool) requestResult {
	sig := cfg.Requests.sig()
	for {
		if err := ctx.Err(); err != nil {
			return requestResult{err: err}
		}
		rs.mu.Lock()
		cur, now := rs.current(sig), rs.now()
		failed := rs.lastFailed && rs.lastSig == sig
		backingOff := failed && now.Sub(rs.lastAttempt) < failBackoff

		var res requestResult
		var call *requestCall
		switch {
		case rs.flight != nil: // somebody is already refreshing: share that
			call = rs.flight
		case !forced && cur != nil && now.Sub(cur.Taken) < freshFor:
			res = requestResult{snap: cur, stale: failed}
		case backingOff:
			res = requestResult{snap: cur, stale: cur != nil, coolDown: forced}
		case forced && cur != nil && now.Sub(cur.Taken) < forcedGap:
			res = requestResult{snap: cur, stale: failed, coolDown: true}
		case forced && !rs.forced.allow("refresh"):
			res = requestResult{snap: cur, stale: failed, coolDown: true}
		default:
			call = &requestCall{done: make(chan struct{}), sig: sig}
			rs.flight = call
			go rs.run(cfg, sig, call)
		}
		if call == nil {
			if res.snap == nil {
				res.err = rs.noData(sig)
			}
			rs.mu.Unlock()
			return res
		}
		rs.mu.Unlock()

		select {
		case <-call.done:
		case <-ctx.Done(): // this visitor left; the refresh carries on for the others
			return requestResult{err: ctx.Err()}
		}
		if call.sig == sig {
			return call.result
		}
		// The active refresh used another configuration. It cannot satisfy this
		// reader; reevaluate the cache and build with the reader's settings.
		forced = false
	}
}

// run does one refresh and wakes everyone waiting for it.
func (rs *requestStatus) run(cfg *Config, sig string, call *requestCall) {
	ctx, cancel := context.WithTimeout(rs.ctx, refreshLimit)
	snap, err := rs.safeBuild(ctx, cfg)
	cancel()

	rs.mu.Lock()
	rs.lastAttempt, rs.lastSig = rs.now(), sig
	if err != nil {
		rs.lastFailed, rs.lastErr = true, err
		log.Printf("request status: refresh failed: %v", err)
	} else {
		snap.Taken, snap.sig, snap.posters = rs.now(), sig, listedPosters(snap)
		rs.snap, rs.lastFailed, rs.lastErr = snap, false, nil
	}
	call.result = requestResult{snap: rs.current(sig), stale: rs.lastFailed}
	if call.result.snap == nil {
		call.result.err = rs.noData(sig)
	}
	rs.flight = nil
	rs.mu.Unlock()
	close(call.done)
}

// safeBuild turns a panic into an error. This runs in its own goroutine, and
// a surprise in what some service sent must not take Anchor down.
func (rs *requestStatus) safeBuild(ctx context.Context, cfg *Config) (snap *snapshot, err error) {
	defer func() {
		if p := recover(); p != nil {
			snap, err = nil, fmt.Errorf("unexpected problem while refreshing: %v", p)
		}
	}()
	return rs.build(ctx, cfg)
}

// ---------- titles ----------

type titleEntry struct {
	title string
	year  int
	at    time.Time
}

// titleCache remembers titles for a week so a refresh doesn't look every one
// up again. Titles hardly ever change. The cache is capped.
type titleCache struct {
	mu sync.Mutex
	m  map[string]titleEntry
}

func newTitleCache() *titleCache { return &titleCache{m: map[string]titleEntry{}} }

func (c *titleCache) get(key string, now time.Time) (titleEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if ok && now.Sub(e.at) > titleTTL {
		delete(c.m, key)
		return titleEntry{}, false
	}
	return e, ok
}

func (c *titleCache) put(key string, e titleEntry, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e.at = now
	c.m[key] = e
	for len(c.m) > titleCacheMax {
		oldestKey, oldest := "", now
		for k, v := range c.m {
			if oldestKey == "" || v.at.Before(oldest) {
				oldestKey, oldest = k, v.at
			}
		}
		delete(c.m, oldestKey)
	}
}

func (c *titleCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

// ---------- building a snapshot ----------

// problemList collects what went wrong for the admin, once per service.
type problemList struct {
	mu   sync.Mutex
	seen map[string]bool
	list []string
}

func (p *problemList) add(service string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.seen == nil {
		p.seen = map[string]bool{}
	}
	if !p.seen[service] {
		p.seen[service] = true
		p.list = append(p.list, err.Error())
	}
}

func missingKey(service, variable string) error {
	return fmt.Errorf("%s needs a key: set %s in the container settings", service, variable)
}

func connectionMissingKey(rc *Requests, service, variable string) error {
	if rc.keys != nil {
		return fmt.Errorf("%s API credential is missing; edit this connection in Administration", service)
	}
	return missingKey(service, variable)
}

// pullEnv is what one refresh works with.
type pullEnv struct {
	seerr  *seerrClient
	plex   *plexClient
	index  *plexIndex // nil when Plex could not be read
	sonarr *sonarrClient
	radarr *radarrClient
	sq, rq map[int]queueState
	loc    *time.Location
	now    time.Time
	probs  problemList
}

// pull reads Seerr's newest requests and checks each against Plex, Sonarr and
// Radarr. Only Seerr failing fails the whole refresh. A service that can't be
// reached is recorded as a problem and the rows are worded accordingly
// (nothing is "ready" without Plex).
func (rs *requestStatus) pull(ctx context.Context, cfg *Config) (*snapshot, error) {
	rc := cfg.Requests
	env := &pullEnv{loc: time.Local, now: rs.now()}

	if rs.keysFor(rc).Seerr == "" {
		return nil, connectionMissingKey(rc, "Seerr", "ANCHOR_SEERR_KEY")
	}
	var err error
	if env.seerr, err = newSeerr(rc.Seerr, rs.keysFor(rc).Seerr, rs.client); err != nil {
		return nil, err
	}
	reqs, err := env.seerr.requests(ctx, seerrTake)
	if err != nil {
		return nil, err
	}
	items := mergeRequests(reqs)
	if len(items) > maxRows {
		items = items[:maxRows]
	}

	rs.connectPlex(ctx, env, rc)

	// Each service's calendar is asked for only if its queue was just read, so
	// a service that is down costs one failed call, not two.
	var (
		episodes          []calEpisode
		movies            []calMovie
		showsOK, moviesOK bool
		winStart, winEnd  = comingWindow(env.now, env.loc)
	)
	if rc.Sonarr != "" {
		if rs.keysFor(rc).Sonarr == "" {
			env.probs.add("Sonarr", connectionMissingKey(rc, "Sonarr", "ANCHOR_SONARR_KEY"))
		} else if s, err := newSonarr(rc.Sonarr, rs.keysFor(rc).Sonarr, rs.client); err != nil {
			env.probs.add("Sonarr", err)
		} else if q, err := s.queue(ctx); err != nil {
			env.probs.add("Sonarr", err)
		} else {
			env.sonarr, env.sq = s, q
			if episodes, err = s.calendar(ctx, winStart, winEnd); err != nil {
				env.probs.add("Sonarr", err)
			} else {
				showsOK = true
			}
		}
	}
	if rc.Radarr != "" {
		if rs.keysFor(rc).Radarr == "" {
			env.probs.add("Radarr", connectionMissingKey(rc, "Radarr", "ANCHOR_RADARR_KEY"))
		} else if r, err := newRadarr(rc.Radarr, rs.keysFor(rc).Radarr, rs.client); err != nil {
			env.probs.add("Radarr", err)
		} else if q, err := r.queue(ctx); err != nil {
			env.probs.add("Radarr", err)
		} else {
			env.radarr, env.rq = r, q
			if movies, err = r.calendar(ctx, winStart, winEnd); err != nil {
				env.probs.add("Radarr", err)
			} else {
				moviesOK = true
			}
		}
	}

	// A row that can't be built stays as a plain "getting it ready".
	rows := make([]requestRow, len(items))
	for i, it := range items {
		rows[i] = requestRow{Title: "Untitled request", Kind: it.Kind, State: "preparing"}
	}
	parallelDo(len(items), parallelCalls, func(i int) { rows[i] = rs.row(ctx, env, items[i]) })

	snap := &snapshot{Rows: rows, PlexOK: env.index != nil, Problems: env.probs.list}
	if rc.Sonarr != "" || rc.Radarr != "" {
		list, listed := buildComing(episodes, movies, env.now, env.loc)
		comingPosters(list, env.index)
		snap.Coming = &comingList{Rows: list}
		trimRepeatedDates(snap.Rows, listed) // the Requests tab needn't say what Coming up does
		if rc.Sonarr != "" && !showsOK {
			snap.Coming.Missing = append(snap.Coming.Missing, "shows")
		}
		if rc.Radarr != "" && !moviesOK {
			snap.Coming.Missing = append(snap.Coming.Missing, "movies")
		}
	}
	coming := 0
	if snap.Coming != nil {
		coming = len(snap.Coming.Rows)
	}
	log.Printf("request status: %d titles, %d coming up, plex ok=%v, problems=%q", len(rows), coming, snap.PlexOK, snap.Problems)
	return snap, nil
}

func (rs *requestStatus) connectPlex(ctx context.Context, env *pullEnv, rc *Requests) {
	if rs.keysFor(rc).Plex == "" {
		env.probs.add("Plex", connectionMissingKey(rc, "Plex", "ANCHOR_PLEX_TOKEN"))
		return
	}
	p, err := newPlex(rc.Plex, rs.keysFor(rc).Plex, rs.client)
	if err != nil {
		env.probs.add("Plex", err)
		return
	}
	idx, err := p.index(ctx)
	if err != nil {
		env.probs.add("Plex", err)
		return
	}
	env.plex, env.index = p, idx
}

// row works out one title.
func (rs *requestStatus) row(ctx context.Context, env *pullEnv, it seerrRequest) requestRow {
	title, year := rs.titleFor(ctx, env, it)
	f := facts{kind: it.Kind, req: it.State, created: it.Created, mediaStatus: it.MediaStatus,
		plexOK: env.index != nil, now: env.now}
	var notes []string
	note := func(service string, err error) {
		env.probs.add(service, err)
		notes = append(notes, err.Error())
	}

	// Declined, pending and failed requests need no further questions.
	if it.State == reqApproved {
		if it.Kind == "movie" {
			f.inPlex = f.plexOK && env.index.movies[it.TMDB]
			if !f.inPlex && env.radarr != nil {
				if a, err := env.radarr.facts(ctx, it.TMDB, env.rq, env.loc, env.now); err != nil {
					note("Radarr", err)
				} else {
					f.arr = a
				}
			}
		} else {
			if f.plexOK {
				if rk := showKey(env.index, it); rk != "" {
					if seasons, err := env.plex.seasons(ctx, rk); err != nil {
						note("Plex", err)
						f.plexOK = false // not read, so not trusted
					} else {
						f.plexEps = countEpisodes(seasons, it.Seasons)
					}
				}
			}
			if env.sonarr != nil && it.TVDB > 0 {
				if a, err := env.sonarr.facts(ctx, it.TVDB, it.Seasons, env.sq, env.loc, env.now); err != nil {
					note("Sonarr", err)
				} else {
					f.arr = a
				}
			}
		}
	}

	row := decide(f)
	if f.arr != nil {
		row.series = f.arr.Series
	}
	// Where its poster may come from: Plex if it is in Plex (a movie that is
	// is never asked about at Radarr), Seerr by its TMDB id, and Sonarr's or
	// Radarr's own cover if they know it.
	plexKey, arrID := 0, 0
	if env.index != nil {
		if it.Kind == "movie" {
			plexKey = plexNumber(env.index.movieKeys[it.TMDB])
		} else {
			plexKey = plexNumber(showKey(env.index, it))
		}
	}
	if f.arr != nil {
		arrID = f.arr.Movie
		if it.Kind == "show" {
			arrID = f.arr.Series
		}
	}
	row.Posters = posterRefs(it.Kind, plexKey, it.TMDB, arrID)
	row.Title, row.Year = title, year
	if row.Title == "" {
		row.Title = "Untitled request"
	}
	if row.Detail == "" && len(notes) > 0 {
		row.Detail = strings.Join(notes, " ")
	}
	return row
}

func showKey(idx *plexIndex, it seerrRequest) string {
	if it.TVDB > 0 {
		if rk := idx.showsTVDB[it.TVDB]; rk != "" {
			return rk
		}
	}
	return idx.showsTMDB[it.TMDB]
}

// countEpisodes adds up Plex's episodes for the requested seasons (every
// season above 0 if none were named).
func countEpisodes(plexSeasons map[int]int, requested []int) int {
	n := 0
	if len(requested) == 0 {
		for season, eps := range plexSeasons {
			if season > 0 {
				n += eps
			}
		}
		return n
	}
	for _, season := range requested {
		n += plexSeasons[season]
	}
	return n
}

func (rs *requestStatus) titleFor(ctx context.Context, env *pullEnv, it seerrRequest) (string, int) {
	key := it.Kind + ":" + strconv.Itoa(it.TMDB)
	if e, ok := rs.titles.get(key, env.now); ok {
		return e.title, e.year
	}
	title, year, err := env.seerr.title(ctx, it.Kind, it.TMDB)
	if err != nil {
		env.probs.add("Seerr", err)
		return "", 0
	}
	if title != "" {
		rs.titles.put(key, titleEntry{title: title, year: year}, env.now)
	}
	return title, year
}

// mergeRequests combines requests for the same title (two people asking for
// the same movie, or different seasons of one show) and keeps Seerr's
// newest-first order.
func mergeRequests(in []seerrRequest) []seerrRequest {
	rank := map[reqState]int{reqApproved: 4, reqPending: 3, reqFailed: 2, reqDeclined: 1}
	at := map[string]int{}
	var out []seerrRequest
	for _, r := range in {
		key := r.Kind + ":" + strconv.Itoa(r.TMDB)
		i, seen := at[key]
		if !seen {
			at[key] = len(out)
			r.Seasons = sortedUnique(r.Seasons)
			out = append(out, r)
			continue
		}
		m := &out[i]
		m.Seasons = sortedUnique(append(append([]int(nil), m.Seasons...), r.Seasons...))
		if m.TVDB == 0 {
			m.TVDB = r.TVDB
		}
		if r.Created.After(m.Created) {
			m.Created = r.Created
		}
		if rank[r.State] > rank[m.State] {
			m.State = r.State
		}
	}
	return out
}

func sortedUnique(in []int) []int {
	if len(in) == 0 {
		return nil
	}
	out := append([]int(nil), in...)
	sort.Ints(out)
	uniq := out[:1]
	for _, n := range out[1:] {
		if n != uniq[len(uniq)-1] {
			uniq = append(uniq, n)
		}
	}
	return uniq
}

// parallelDo runs fn(0..n-1) with at most limit running at once. A job that
// panics doesn't stop the others; the numbers of the ones that did are
// returned.
func parallelDo(n, limit int, fn func(i int)) []int {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		panicked []int
	)
	sem := make(chan struct{}, limit)
	for i := 0; i < n; i++ {
		sem <- struct{}{}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if p := recover(); p != nil {
					log.Printf("request status: job %d: %v", i, p)
					mu.Lock()
					panicked = append(panicked, i)
					mu.Unlock()
				}
			}()
			fn(i)
		}(i)
	}
	wg.Wait()
	sort.Ints(panicked)
	return panicked
}

func (rs *requestStatus) keysFor(rc *Requests) requestKeys {
	if rc != nil && rc.keys != nil {
		return *rc.keys
	}
	return rs.keys
}
