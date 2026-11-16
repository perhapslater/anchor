package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// One small client per service. Each one asks only the questions Anchor
// needs, through an upstream (see upstream.go) limited to the paths listed
// here, and hands back plain values. Numbers may arrive as 5 or "5" and
// anything may be missing: these answers come from software Anchor doesn't
// control, so nothing here is allowed to fail on a surprise.

const (
	arrBody      = 4 << 20  // largest answer accepted from Seerr, Sonarr, Radarr
	plexBody     = 16 << 20 // a page of a big Plex library
	plexPageSize = 1000
	maxPlexPages = 50
	maxID        = 999999999 // the largest id that goes into a path
)

// flexInt reads 5, "5", 5.0, null, "" or junk as a number, never failing.
type flexInt int

func (n *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if v, err := strconv.Atoi(s); err == nil {
		*n = flexInt(v)
	} else if f, err := strconv.ParseFloat(s, 64); err == nil {
		*n = flexInt(f)
	} else {
		*n = 0
	}
	return nil
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// earliest returns the earliest of the times that are set.
func earliest(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if !t.IsZero() && (out.IsZero() || t.Before(out)) {
			out = t
		}
	}
	return out
}

func validID(id int) bool { return id > 0 && id <= maxID }

// ---------- Seerr ----------

type seerrClient struct{ u *upstream }

func newSeerr(base, key string, c *http.Client) (*seerrClient, error) {
	u, err := newUpstream("Seerr", base, "X-Api-Key", key, c, arrBody,
		`^/api/v1/request$`, `^/api/v1/(movie|tv)/[0-9]{1,9}$`, seerrImagePattern)
	if err != nil {
		return nil, err
	}
	return &seerrClient{u}, nil
}

// seerrPosterPath is what Seerr says a title's poster file is called at TMDB:
// a slash, a plain name and a picture extension. Nothing else is followed.
var seerrPosterPath = regexp.MustCompile(`^/[A-Za-z0-9_-]{1,64}\.(jpg|png)$`)

// poster fetches a title's poster from Seerr, by the title's TMDB id. Seerr
// knows the file's name at TMDB (the same one it shows in its own pages), and
// serves a copy of the picture itself, so Anchor needs no address outside the
// four it is allowed. kind is "movie" or "show", as for title.
func (s *seerrClient) poster(ctx context.Context, kind string, tmdb int) ([]byte, string, error) {
	if !validID(tmdb) {
		return nil, "", errors.New("Seerr: bad id")
	}
	var path string
	switch kind {
	case "movie":
		path = "/api/v1/movie/"
	case "show":
		path = "/api/v1/tv/"
	default:
		return nil, "", errors.New("Seerr: bad kind")
	}
	var raw struct {
		PosterPath string `json:"posterPath"`
	}
	if err := s.u.getJSON(ctx, path+strconv.Itoa(tmdb), nil, &raw); err != nil {
		return nil, "", err
	}
	if !seerrPosterPath.MatchString(raw.PosterPath) {
		return nil, "", errors.New("Seerr has no poster for it")
	}
	return s.u.getImage(ctx, "/imageproxy/tmdb/t/p/w185"+raw.PosterPath, nil, posterMax)
}

// seerrRequest is one request, as much of it as Anchor uses.
type seerrRequest struct {
	Kind        string // movie or show
	State       reqState
	MediaStatus int
	TMDB, TVDB  int
	Seasons     []int // requested seasons (shows)
	Created     time.Time
}

// requests lists the newest requests, newest first. Entries Anchor can't
// make sense of are left out.
func (s *seerrClient) requests(ctx context.Context, take int) ([]seerrRequest, error) {
	var raw struct {
		Results []struct {
			Status  flexInt `json:"status"`
			Type    string  `json:"type"`
			Created string  `json:"createdAt"`
			Media   struct {
				Type   string  `json:"mediaType"`
				TMDB   flexInt `json:"tmdbId"`
				TVDB   flexInt `json:"tvdbId"`
				Status flexInt `json:"status"`
			} `json:"media"`
			Seasons []struct {
				Number flexInt `json:"seasonNumber"`
			} `json:"seasons"`
		} `json:"results"`
	}
	q := url.Values{"take": {strconv.Itoa(take)}, "skip": {"0"}, "sort": {"added"}}
	if err := s.u.getJSON(ctx, "/api/v1/request", q, &raw); err != nil {
		return nil, err
	}
	var out []seerrRequest
	for _, r := range raw.Results {
		kind := strings.ToLower(r.Type)
		if kind == "" {
			kind = strings.ToLower(r.Media.Type)
		}
		switch kind {
		case "movie":
		case "tv":
			kind = "show"
		default:
			continue
		}
		if !validID(int(r.Media.TMDB)) {
			continue
		}
		req := seerrRequest{
			Kind:        kind,
			State:       seerrState(int(r.Status)),
			MediaStatus: int(r.Media.Status),
			TMDB:        int(r.Media.TMDB),
			TVDB:        int(r.Media.TVDB),
			Created:     parseTime(r.Created),
		}
		for _, se := range r.Seasons {
			if se.Number > 0 {
				req.Seasons = append(req.Seasons, int(se.Number))
			}
		}
		out = append(out, req)
	}
	return out, nil
}

// seerrState maps Seerr's request status numbers: 1 pending, 2 approved,
// 3 declined, 4 failed, 5 completed.
func seerrState(n int) reqState {
	switch n {
	case 1:
		return reqPending
	case 3:
		return reqDeclined
	case 4:
		return reqFailed
	}
	return reqApproved
}

// title looks up a title's name and year. The name is cleaned here, because
// it is text from the internet that ends up on the family's screens.
func (s *seerrClient) title(ctx context.Context, kind string, tmdb int) (string, int, error) {
	if !validID(tmdb) {
		return "", 0, errors.New("Seerr: bad id")
	}
	var path string
	switch kind {
	case "movie":
		path = "/api/v1/movie/"
	case "show":
		path = "/api/v1/tv/"
	default:
		return "", 0, errors.New("Seerr: bad kind")
	}
	var raw struct {
		Title        string `json:"title"`
		Name         string `json:"name"`
		ReleaseDate  string `json:"releaseDate"`
		FirstAirDate string `json:"firstAirDate"`
	}
	if err := s.u.getJSON(ctx, path+strconv.Itoa(tmdb), nil, &raw); err != nil {
		return "", 0, err
	}
	name, date := raw.Title, raw.ReleaseDate
	if kind == "show" {
		name, date = raw.Name, raw.FirstAirDate
	}
	return cleanText(name, 120), yearOf(date), nil
}

func yearOf(date string) int {
	if len(date) >= 4 {
		if y, err := strconv.Atoi(date[:4]); err == nil {
			return cleanYear(y)
		}
	}
	return 0
}

// ---------- Plex ----------

type plexClient struct {
	u        *upstream
	pageSize int
}

func newPlex(base, token string, c *http.Client) (*plexClient, error) {
	u, err := newUpstream("Plex", base, "X-Plex-Token", token, c, plexBody,
		`^/library/sections$`, `^/library/sections/[0-9]{1,6}/all$`, `^/library/metadata/[0-9]{1,9}/children$`,
		`^/photo/:/transcode$`, `^/library/metadata/[0-9]{1,9}/thumb$`) // the last two are for a movie's poster
	if err != nil {
		return nil, err
	}
	return &plexClient{u: u, pageSize: plexPageSize}, nil
}

// poster fetches a movie's poster by Plex's own number for it (its rating key).
// Plex scales it to the same 250 pixels Sonarr and Radarr keep ready; a full
// size poster can be a megabyte. If Plex can't scale it, the original is asked
// for, and is accepted only if it is small enough. Everything in the address
// is built here from the number.
func (p *plexClient) poster(ctx context.Context, ratingKey int) ([]byte, string, error) {
	if !validID(ratingKey) {
		return nil, "", errors.New("Plex: bad id")
	}
	thumb := "/library/metadata/" + strconv.Itoa(ratingKey) + "/thumb"
	q := url.Values{"url": {thumb}, "width": {"250"}, "height": {"375"}, "minSize": {"1"}}
	data, ctype, err := p.u.getImage(ctx, "/photo/:/transcode", q, posterMax)
	if err == nil {
		return data, ctype, nil
	}
	if data, ctype, err2 := p.u.getImage(ctx, thumb, nil, posterMax); err2 == nil {
		return data, ctype, nil
	}
	return nil, "", err // the first answer says more than the second
}

// plexIndex is what the Plex libraries hold, by the ids Seerr uses.
type plexIndex struct {
	movies    map[int]bool   // TMDB id
	movieKeys map[int]string // TMDB id -> Plex rating key, which a movie's poster is fetched by
	showsTVDB map[int]string // TVDB id -> Plex rating key
	showsTMDB map[int]string // TMDB id -> Plex rating key
}

var (
	plexGUID       = regexp.MustCompile(`^(tmdb|tvdb)://([0-9]{1,9})$`)
	plexLegacyGUID = regexp.MustCompile(`^com\.plexapp\.agents\.(themoviedb|thetvdb)://([0-9]{1,9})`)
	plexSectionKey = regexp.MustCompile(`^[0-9]{1,6}$`)
	plexRatingKey  = regexp.MustCompile(`^[0-9]{1,9}$`)
)

// index reads every movie and show library once (in pages).
func (p *plexClient) index(ctx context.Context) (*plexIndex, error) {
	var secs struct {
		MediaContainer struct {
			Directory []struct {
				Key  string `json:"key"`
				Type string `json:"type"`
			} `json:"Directory"`
		} `json:"MediaContainer"`
	}
	if err := p.u.getJSON(ctx, "/library/sections", nil, &secs); err != nil {
		return nil, err
	}
	idx := &plexIndex{movies: map[int]bool{}, movieKeys: map[int]string{}, showsTVDB: map[int]string{}, showsTMDB: map[int]string{}}
	for _, d := range secs.MediaContainer.Directory {
		if !plexSectionKey.MatchString(d.Key) || (d.Type != "movie" && d.Type != "show") {
			continue
		}
		if err := p.readLibrary(ctx, d.Key, d.Type, idx); err != nil {
			return nil, err
		}
	}
	return idx, nil
}

func (p *plexClient) readLibrary(ctx context.Context, key, kind string, idx *plexIndex) error {
	start := 0
	for page := 0; page < maxPlexPages; page++ {
		var lib struct {
			MediaContainer struct {
				TotalSize flexInt `json:"totalSize"`
				Metadata  []struct {
					RatingKey json.Number `json:"ratingKey"`
					GUID      string      `json:"guid"`
					Guids     []struct {
						ID string `json:"id"`
					} `json:"Guid"`
				} `json:"Metadata"`
			} `json:"MediaContainer"`
		}
		q := url.Values{
			"includeGuids":           {"1"},
			"X-Plex-Container-Start": {strconv.Itoa(start)},
			"X-Plex-Container-Size":  {strconv.Itoa(p.pageSize)},
		}
		if err := p.u.getJSON(ctx, "/library/sections/"+key+"/all", q, &lib); err != nil {
			return err
		}
		items := lib.MediaContainer.Metadata
		for _, it := range items {
			rating := it.RatingKey.String()
			ids := []string{it.GUID}
			for _, g := range it.Guids {
				ids = append(ids, g.ID)
			}
			for _, id := range ids {
				src, n := plexExternalID(id)
				switch {
				case n == 0:
				case kind == "movie" && src == "tmdb":
					idx.movies[n] = true
					if plexRatingKey.MatchString(rating) {
						idx.movieKeys[n] = rating
					}
				case kind == "show" && src == "tvdb" && plexRatingKey.MatchString(rating):
					idx.showsTVDB[n] = rating
				case kind == "show" && src == "tmdb" && plexRatingKey.MatchString(rating):
					idx.showsTMDB[n] = rating
				}
			}
		}
		start += len(items)
		if total := int(lib.MediaContainer.TotalSize); len(items) < p.pageSize || (total > 0 && start >= total) {
			return nil
		}
	}
	return nil
}

// plexExternalID reads "tmdb://603" or an older agent's
// "com.plexapp.agents.themoviedb://603?lang=en" as ("tmdb", 603).
func plexExternalID(guid string) (string, int) {
	src, num := "", ""
	if m := plexGUID.FindStringSubmatch(guid); m != nil {
		src, num = m[1], m[2]
	} else if m := plexLegacyGUID.FindStringSubmatch(guid); m != nil {
		src, num = map[string]string{"themoviedb": "tmdb", "thetvdb": "tvdb"}[m[1]], m[2]
	}
	n, _ := strconv.Atoi(num)
	return src, n
}

// seasons says how many episodes Plex has of each season of one show.
func (p *plexClient) seasons(ctx context.Context, ratingKey string) (map[int]int, error) {
	if !plexRatingKey.MatchString(ratingKey) {
		return nil, errors.New("Plex: bad id")
	}
	var raw struct {
		MediaContainer struct {
			Metadata []struct {
				Index     flexInt `json:"index"`
				LeafCount flexInt `json:"leafCount"`
			} `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := p.u.getJSON(ctx, "/library/metadata/"+ratingKey+"/children", nil, &raw); err != nil {
		return nil, err
	}
	out := map[int]int{}
	for _, s := range raw.MediaContainer.Metadata {
		out[int(s.Index)] = int(s.LeafCount)
	}
	return out, nil
}

// ---------- the download queue (Sonarr and Radarr) ----------

// queueState is what a title's downloads are doing right now.
type queueState struct{ Downloading, Problem bool }

// fetchQueue reads the download queue, keyed by series id (Sonarr) or movie
// id (Radarr). Titles that are queued or paused are present but neither
// downloading nor in trouble.
func fetchQueue(ctx context.Context, u *upstream, movies bool) (map[int]queueState, error) {
	var raw struct {
		Records []struct {
			SeriesID flexInt `json:"seriesId"`
			MovieID  flexInt `json:"movieId"`
			Status   string  `json:"status"`
			Tracked  string  `json:"trackedDownloadStatus"`
			State    string  `json:"trackedDownloadState"`
		} `json:"records"`
	}
	q := url.Values{"page": {"1"}, "pageSize": {"500"}}
	if err := u.getJSON(ctx, "/api/v3/queue", q, &raw); err != nil {
		return nil, err
	}
	out := map[int]queueState{}
	for _, r := range raw.Records {
		id := int(r.SeriesID)
		if movies {
			id = int(r.MovieID)
		}
		if id <= 0 {
			continue
		}
		status, tracked, state := strings.ToLower(r.Status), strings.ToLower(r.Tracked), strings.ToLower(r.State)
		problem := tracked == "warning" || tracked == "error" ||
			status == "failed" || status == "warning" || status == "downloadclientunavailable" ||
			state == "failed" || state == "failedpending" || state == "importblocked"
		q := out[id]
		q.Problem = q.Problem || problem
		q.Downloading = q.Downloading || (status == "downloading" && !problem)
		out[id] = q
	}
	return out, nil
}

// ---------- Sonarr ----------

type sonarrClient struct{ u *upstream }

func newSonarr(base, key string, c *http.Client) (*sonarrClient, error) {
	u, err := newUpstream("Sonarr", base, "X-Api-Key", key, c, arrBody, `^/api/v3/series$`, `^/api/v3/queue$`, `^/api/v3/calendar$`, coverAPIPattern, coverUIPattern)
	if err != nil {
		return nil, err
	}
	return &sonarrClient{u}, nil
}

// poster fetches a show's cover, by Sonarr's own id for the show.
func (s *sonarrClient) poster(ctx context.Context, id int) ([]byte, string, error) {
	return fetchCover(ctx, s.u, id)
}

func (s *sonarrClient) queue(ctx context.Context) (map[int]queueState, error) {
	return fetchQueue(ctx, s.u, false)
}

// calendar lists the monitored episodes Sonarr has on its calendar between two
// moments, with each show's name and year. Entries missing a show, a name or
// an air time are left out.
func (s *sonarrClient) calendar(ctx context.Context, start, end time.Time) ([]calEpisode, error) {
	var raw []struct {
		SeriesID   flexInt `json:"seriesId"`
		Season     flexInt `json:"seasonNumber"`
		Episode    flexInt `json:"episodeNumber"`
		AirDateUTC string  `json:"airDateUtc"`
		Monitored  *bool   `json:"monitored"`
		Series     *struct {
			ID     flexInt `json:"id"`
			Title  string  `json:"title"`
			Year   flexInt `json:"year"`
			TVDBID flexInt `json:"tvdbId"`
			TMDBID flexInt `json:"tmdbId"`
		} `json:"series"`
	}
	q := url.Values{
		"start":         {start.UTC().Format(time.RFC3339)},
		"end":           {end.UTC().Format(time.RFC3339)},
		"unmonitored":   {"false"},
		"includeSeries": {"true"},
	}
	if err := s.u.getJSON(ctx, "/api/v3/calendar", q, &raw); err != nil {
		return nil, err
	}
	var out []calEpisode
	for _, e := range raw {
		air := parseTime(e.AirDateUTC)
		if e.Series == nil || strings.TrimSpace(e.Series.Title) == "" || air.IsZero() {
			continue
		}
		id := int(e.SeriesID)
		if id <= 0 {
			id = int(e.Series.ID)
		}
		if id <= 0 {
			continue
		}
		out = append(out, calEpisode{
			Series: id, TVDB: posterID(int(e.Series.TVDBID)), TMDB: posterID(int(e.Series.TMDBID)),
			Title: e.Series.Title, Year: int(e.Series.Year),
			Season: int(e.Season), Episode: int(e.Episode), Air: air,
			Monitored: e.Monitored == nil || *e.Monitored,
		})
	}
	return out, nil
}

type sonarrStats struct {
	FileCount      flexInt `json:"episodeFileCount"`
	EpisodeCount   flexInt `json:"episodeCount"` // aired and monitored
	NextAiring     string  `json:"nextAiring"`
	PreviousAiring string  `json:"previousAiring"`
}

// facts says how the requested seasons of a show stand in Sonarr: how many
// episodes have aired, how many have files, when the next one airs (shown in
// loc, since an air time is a real moment) and when the latest aired. It
// returns nil if Sonarr doesn't know the show.
func (s *sonarrClient) facts(ctx context.Context, tvdb int, seasons []int, queue map[int]queueState, loc *time.Location, now time.Time) (*arrFacts, error) {
	if !validID(tvdb) {
		return nil, errors.New("Sonarr: bad id")
	}
	var series []struct {
		ID         flexInt      `json:"id"`
		NextAiring string       `json:"nextAiring"`
		Previous   string       `json:"previousAiring"`
		Statistics *sonarrStats `json:"statistics"`
		Seasons    []struct {
			Number     flexInt      `json:"seasonNumber"`
			Statistics *sonarrStats `json:"statistics"`
		} `json:"seasons"`
	}
	if err := s.u.getJSON(ctx, "/api/v3/series", url.Values{"tvdbId": {strconv.Itoa(tvdb)}}, &series); err != nil {
		return nil, err
	}
	if len(series) == 0 {
		return nil, nil
	}
	sr := series[0]

	wanted := map[int]bool{}
	for _, n := range seasons {
		wanted[n] = true
	}
	var stats []sonarrStats
	for _, se := range sr.Seasons {
		if se.Statistics != nil && (wanted[int(se.Number)] || (len(wanted) == 0 && se.Number > 0)) {
			stats = append(stats, *se.Statistics)
		}
	}
	if len(sr.Seasons) == 0 && sr.Statistics != nil { // an older Sonarr: whole-show numbers
		st := *sr.Statistics
		st.NextAiring, st.PreviousAiring = sr.NextAiring, sr.Previous
		stats = append(stats, st)
	}

	out := &arrFacts{}
	var next, prev time.Time
	for _, st := range stats {
		out.Aired += int(st.EpisodeCount)
		out.Files += int(st.FileCount)
		if n := parseTime(st.NextAiring); n.After(now) {
			next = earliest(next, n)
		}
		if p := parseTime(st.PreviousAiring); p.After(prev) {
			prev = p
		}
	}
	out.Available = out.Aired > 0
	out.Since = prev
	if !next.IsZero() {
		out.Upcoming, out.Expected = true, next.In(loc).Format("2006-01-02")
	}
	q := queue[int(sr.ID)]
	out.Downloading, out.QueueProblem = q.Downloading, q.Problem
	out.Series = int(sr.ID)
	return out, nil
}

// ---------- Radarr ----------

type radarrClient struct{ u *upstream }

func newRadarr(base, key string, c *http.Client) (*radarrClient, error) {
	u, err := newUpstream("Radarr", base, "X-Api-Key", key, c, arrBody, `^/api/v3/movie$`, `^/api/v3/queue$`, `^/api/v3/calendar$`, coverAPIPattern, coverUIPattern)
	if err != nil {
		return nil, err
	}
	return &radarrClient{u}, nil
}

// poster fetches a movie's cover, by Radarr's own id for the movie.
func (r *radarrClient) poster(ctx context.Context, id int) ([]byte, string, error) {
	return fetchCover(ctx, r.u, id)
}

func (r *radarrClient) queue(ctx context.Context) (map[int]queueState, error) {
	return fetchQueue(ctx, r.u, true)
}

// radarrDate is the date Radarr itself starts looking for a movie on, and
// which kind of date that is: "cinemas" (the movie is set to "In Cinemas" and
// has a theatrical date) or "home" (the earlier of digital and physical). A
// movie set to "Announced" has no date to wait for, so it, and a movie with
// no date to go on, give a zero time and "". Both the Requests tab and the
// Coming up tab use this, so a movie never has two different dates.
func radarrDate(minAvailability string, cinemas, digital, physical time.Time) (time.Time, string) {
	switch strings.ToLower(minAvailability) {
	case "announced":
		return time.Time{}, ""
	case "incinemas":
		if !cinemas.IsZero() {
			return cinemas, "cinemas"
		}
	}
	if home := earliest(digital, physical); !home.IsZero() {
		return home, "home"
	}
	return time.Time{}, ""
}

// calendar lists the monitored movies Radarr has on its calendar between two
// days. Radarr's release dates are calendar dates, so the range is too (the
// first day, and the day after the last).
func (r *radarrClient) calendar(ctx context.Context, start, end time.Time) ([]calMovie, error) {
	var raw []struct {
		ID                  flexInt `json:"id"`
		TMDBID              flexInt `json:"tmdbId"`
		Title               string  `json:"title"`
		Year                flexInt `json:"year"`
		Monitored           *bool   `json:"monitored"`
		HasFile             bool    `json:"hasFile"`
		MinimumAvailability string  `json:"minimumAvailability"`
		InCinemas           string  `json:"inCinemas"`
		DigitalRelease      string  `json:"digitalRelease"`
		PhysicalRelease     string  `json:"physicalRelease"`
	}
	q := url.Values{"start": {start.Format(dateLayout)}, "end": {end.Format(dateLayout)}, "unmonitored": {"false"}}
	if err := r.u.getJSON(ctx, "/api/v3/calendar", q, &raw); err != nil {
		return nil, err
	}
	var out []calMovie
	for _, m := range raw {
		if strings.TrimSpace(m.Title) == "" {
			continue
		}
		date, kind := radarrDate(m.MinimumAvailability, parseTime(m.InCinemas), parseTime(m.DigitalRelease), parseTime(m.PhysicalRelease))
		cm := calMovie{ID: int(m.ID), TMDB: posterID(int(m.TMDBID)), Title: m.Title, Year: int(m.Year), Monitored: m.Monitored == nil || *m.Monitored, HasFile: m.HasFile, Kind: kind}
		if !date.IsZero() {
			cm.Date = date.UTC().Format(dateLayout)
		}
		out = append(out, cm)
	}
	return out, nil
}

// facts says how a movie stands in Radarr. The date it gives is the one
// Radarr itself starts looking on: the home release (earlier of digital and
// physical) unless the movie is set to "In Cinemas" (theatrical date) or
// "Announced" (no wait). Release dates are calendar dates, so they are shown
// as given. It returns nil if Radarr doesn't know the movie.
func (r *radarrClient) facts(ctx context.Context, tmdb int, queue map[int]queueState, loc *time.Location, now time.Time) (*arrFacts, error) {
	if !validID(tmdb) {
		return nil, errors.New("Radarr: bad id")
	}
	var movies []struct {
		ID                  flexInt `json:"id"`
		HasFile             bool    `json:"hasFile"`
		IsAvailable         *bool   `json:"isAvailable"`
		MinimumAvailability string  `json:"minimumAvailability"`
		InCinemas           string  `json:"inCinemas"`
		DigitalRelease      string  `json:"digitalRelease"`
		PhysicalRelease     string  `json:"physicalRelease"`
	}
	if err := r.u.getJSON(ctx, "/api/v3/movie", url.Values{"tmdbId": {strconv.Itoa(tmdb)}}, &movies); err != nil {
		return nil, err
	}
	if len(movies) == 0 {
		return nil, nil
	}
	m := movies[0]

	expected, _ := radarrDate(m.MinimumAvailability, parseTime(m.InCinemas), parseTime(m.DigitalRelease), parseTime(m.PhysicalRelease))

	out := &arrFacts{HasFile: m.HasFile}
	if validID(int(m.ID)) {
		out.Movie = int(m.ID)
	}
	if !expected.IsZero() {
		out.Expected = expected.UTC().Format("2006-01-02")
		out.Upcoming = out.Expected > now.In(loc).Format("2006-01-02")
		if !out.Upcoming {
			out.Since = expected
		}
	}
	switch {
	case m.IsAvailable != nil:
		out.Available = *m.IsAvailable
	default:
		out.Available = strings.EqualFold(m.MinimumAvailability, "announced") || (!expected.IsZero() && !out.Upcoming)
	}
	q := queue[int(m.ID)]
	out.Downloading, out.QueueProblem = q.Downloading, q.Problem
	return out, nil
}
