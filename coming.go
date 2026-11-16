package main

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// Coming up. The second tab of the Request status sheet lists what is due in
// the next six months, from what Sonarr and Radarr already track. It rides in the
// same snapshot as the request rows, so it has the same gate, the same limits
// and the same refresh.
//
// This file turns the two calendars into one short list. Like decide, it is a
// plain function of its input, so it can be tested without any network.

const (
	comingMonths = 6  // from today to the day before the same date six months on
	maxComing    = 40 // rows kept, soonest first
	dateLayout   = "2006-01-02"
)

// comingRow is one show or movie as the page gets it. Note is always one of a
// fixed set, so the page's wording never depends on upstream text:
//
//	episode   a show's next episode
//	episodes  several episodes on one day (Count says how many)
//	premiere  the first episode of a season (Season says which)
//	cinemas   a movie, the date being its theatrical release
//	home      a movie, the date being its home release
type comingRow struct {
	Title  string `json:"title"`
	Year   int    `json:"year,omitempty"`
	Kind   string `json:"kind"` // movie or show
	Date   string `json:"date"` // YYYY-MM-DD
	Note   string `json:"note"`
	Count  int    `json:"count,omitempty"`
	Season int    `json:"season,omitempty"`
	// Posters says where the poster may come from, best first (see posters.go).
	Posters []string `json:"posters,omitempty"`

	// What Posters is worked out from, kept here until Plex has been asked.
	arrID int // Sonarr's id for the show or Radarr's for the movie
	tvdb  int // shows
	tmdb  int
}

// comingList is the Coming up tab. Missing names the half that couldn't be
// read ("shows" for Sonarr, "movies" for Radarr), so the page can say that
// something may be left out.
type comingList struct {
	Rows    []comingRow `json:"rows"`
	Missing []string    `json:"missing,omitempty"`
}

// calEpisode is one episode on Sonarr's calendar.
type calEpisode struct {
	Series          int // Sonarr's id for the show
	TVDB, TMDB      int // the show's ids at TVDB and TMDB, 0 if the calendar didn't say
	Title           string
	Year            int
	Season, Episode int
	Air             time.Time // a real moment
	Monitored       bool
}

// calMovie is one movie on Radarr's calendar. Date is the one Radarr itself
// starts looking on (see radarrDate), empty if there isn't one.
type calMovie struct {
	ID        int
	TMDB      int // 0 if the calendar didn't say
	Title     string
	Year      int
	Monitored bool
	HasFile   bool
	Date      string // YYYY-MM-DD, a calendar date
	Kind      string // cinemas or home
}

// comingWindow is from the start of today (in loc) to the start of the day
// after the last day shown.
func comingWindow(now time.Time, loc *time.Location) (start, end time.Time) {
	d := now.In(loc)
	start = time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
	return start, start.AddDate(0, comingMonths, 0)
}

// buildComing makes the list: one row per show, at its next episode, and one
// per movie, soonest first. Specials, unmonitored titles, movies already
// downloaded and anything undated or outside the window are left out.
// Episodes that have already aired are too, and a show moves on to its next
// one; a movie dated today still counts. The list is never nil.
//
// It also says which shows made it into the list, by Sonarr's id, and the date
// each is listed under (see trimRepeatedDates).
func buildComing(eps []calEpisode, movies []calMovie, now time.Time, loc *time.Location) ([]comingRow, map[int]string) {
	start, end := comingWindow(now, loc)
	first, last := start.Format(dateLayout), end.AddDate(0, 0, -1).Format(dateLayout)
	type entry struct {
		row  comingRow
		show int // Sonarr's id; 0 for a movie
	}
	var entries []entry
	add := func(r comingRow, show int) {
		if r.Title = dropYearSuffix(cleanText(r.Title, 120), r.Year); r.Title != "" {
			entries = append(entries, entry{r, show})
		}
	}

	byShow := map[int][]calEpisode{}
	for _, e := range eps {
		if !e.Monitored || e.Season <= 0 || e.Air.IsZero() || !e.Air.After(now) {
			continue
		}
		if e.Air.In(loc).Format(dateLayout) > last {
			continue
		}
		byShow[e.Series] = append(byShow[e.Series], e)
	}
	for _, list := range byShow {
		sort.Slice(list, func(i, j int) bool {
			a, b := list[i], list[j]
			switch {
			case !a.Air.Equal(b.Air):
				return a.Air.Before(b.Air)
			case a.Season != b.Season:
				return a.Season < b.Season
			}
			return a.Episode < b.Episode
		})
		next := list[0]
		date := next.Air.In(loc).Format(dateLayout)
		sameDay := 0
		for _, e := range list {
			if e.Air.In(loc).Format(dateLayout) == date {
				sameDay++
			}
		}
		row := comingRow{Title: next.Title, Year: cleanYear(next.Year), Kind: "show", Date: date,
			arrID: posterID(next.Series), tvdb: posterID(next.TVDB), tmdb: posterID(next.TMDB)}
		switch {
		case next.Episode == 1:
			row.Note, row.Season = "premiere", next.Season
		case sameDay > 1:
			row.Note, row.Count = "episodes", sameDay
		default:
			row.Note = "episode"
		}
		add(row, next.Series)
	}

	seen := map[int]bool{}
	for _, m := range movies {
		if !m.Monitored || m.HasFile || (m.Kind != "cinemas" && m.Kind != "home") {
			continue
		}
		if _, err := time.Parse(dateLayout, m.Date); err != nil || m.Date < first || m.Date > last {
			continue
		}
		if m.ID > 0 {
			if seen[m.ID] {
				continue
			}
			seen[m.ID] = true
		}
		add(comingRow{Title: m.Title, Year: cleanYear(m.Year), Kind: "movie", Date: m.Date, Note: m.Kind, arrID: posterID(m.ID), tmdb: posterID(m.TMDB)}, 0)
	}

	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i].row, entries[j].row
		if a.Date != b.Date {
			return a.Date < b.Date
		}
		if ta, tb := strings.ToLower(a.Title), strings.ToLower(b.Title); ta != tb {
			return ta < tb
		}
		return a.Kind < b.Kind
	})
	if len(entries) > maxComing {
		entries = entries[:maxComing]
	}
	rows, listed := make([]comingRow, len(entries)), map[int]string{}
	for i, e := range entries {
		rows[i] = e.row
		if e.show > 0 {
			listed[e.show] = e.row.Date
		}
	}
	return rows, listed
}

// trimRepeatedDates takes the "next episode airs" date off a Requests row when
// the Coming up tab already lists that same show on that same day, so the two
// tabs don't say the same thing. Anything the calendar doesn't back up stays:
// a date beyond the window, a different day, a show that wasn't listed
// (or a calendar that couldn't be read), and the dates of titles that aren't
// out yet, which are the answer to "when?" itself.
func trimRepeatedDates(rows []requestRow, listed map[int]string) {
	for i := range rows {
		r := &rows[i]
		if r.State == "partial" && r.Kind == "show" && r.Date != "" && r.series > 0 && listed[r.series] == r.Date {
			r.Date = ""
		}
	}
}

// dropYearSuffix takes " (2026)" off the end of a title when 2026 is the year
// already shown next to it. Sonarr adds a year to a show's name to tell it
// from another with the same name, and Seerr, which names the Requests tab's
// rows, doesn't. Other endings such as " (US)" are left alone.
func dropYearSuffix(title string, year int) string {
	if year == 0 {
		return title
	}
	if t := strings.TrimSuffix(title, " ("+strconv.Itoa(year)+")"); t != title && strings.TrimSpace(t) != "" {
		return t
	}
	return title
}

// cleanYear returns the year if it could be a real one, and 0 if not.
func cleanYear(y int) int {
	if y >= 1870 && y <= 2200 {
		return y
	}
	return 0
}
