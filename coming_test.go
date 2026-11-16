package main

import (
	"context"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---------- the window ----------

func TestComingWindowIsSixLocalMonths(t *testing.T) {
	// 11pm on Oct 3 in New York is already Oct 4 in UTC. The window starts at
	// New York's midnight and runs six months: Oct 3 to Apr 2 inclusive.
	late := time.Date(2026, 10, 3, 23, 0, 0, 0, eastern)
	start, end := comingWindow(late, eastern)
	if !start.Equal(time.Date(2026, 10, 3, 0, 0, 0, 0, eastern)) {
		t.Errorf("start %v", start)
	}
	if got := end.Format("2006-01-02"); got != "2027-04-03" { // the day after the last day
		t.Errorf("end %v", end)
	}
}

// ---------- Sonarr's calendar ----------

const sonarrCalendar = `[
 {"seriesId":9,"seasonNumber":2,"episodeNumber":5,"airDateUtc":"2026-10-08T01:00:00Z","monitored":true,
  "series":{"id":9,"title":"Severance","year":2022,"overview":"ignored","images":[{"url":"/x.jpg"}]}},
 {"seriesId":"11","seasonNumber":"1","episodeNumber":"1","airDateUtc":"2026-10-20T12:00:00Z",
  "series":{"title":"Brand New","year":"2026"}},
 {"seasonNumber":1,"episodeNumber":1,"airDateUtc":"2026-10-21T12:00:00Z","series":{"id":12,"title":"Id Only In Series"}},
 {"seriesId":13,"seasonNumber":1,"episodeNumber":1,"airDateUtc":"2026-10-22T12:00:00Z","monitored":false,"series":{"title":"Unmonitored"}},
 {"seriesId":14,"seasonNumber":1,"episodeNumber":1,"airDateUtc":"not a date","series":{"title":"Bad Date"}},
 {"seriesId":15,"seasonNumber":1,"episodeNumber":1,"airDateUtc":"2026-10-23T12:00:00Z"},
 {"seasonNumber":1,"episodeNumber":1,"airDateUtc":"2026-10-24T12:00:00Z","series":{"title":"No Id"}}
]`

func TestSonarrCalendar(t *testing.T) {
	api := newFakeAPI(t, "X-Api-Key", map[string]string{"/api/v3/calendar": sonarrCalendar})
	s, err := newSonarr(api.srv.URL, "sonarr-key", devClient)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 3, 0, 0, 0, 0, eastern)
	got, err := s.calendar(context.Background(), start, start.AddDate(0, 0, 30))
	if err != nil {
		t.Fatal(err)
	}

	// One GET, to the fixed path, with a range Anchor worked out itself.
	asked := api.asked()
	if len(asked) != 1 {
		t.Fatalf("asked %v", asked)
	}
	u, _ := url.Parse(asked[0])
	if u.Path != "/api/v3/calendar" {
		t.Errorf("path %q", u.Path)
	}
	want := url.Values{
		"start":         {"2026-10-03T05:00:00Z"},
		"end":           {"2026-11-02T05:00:00Z"},
		"unmonitored":   {"false"},
		"includeSeries": {"true"},
	}
	if !reflect.DeepEqual(u.Query(), want) {
		t.Errorf("query %v, want %v", u.Query(), want)
	}
	if api.keys[0] != "sonarr-key" || strings.Contains(asked[0], "sonarr-key") {
		t.Errorf("the key belongs in the header only: %q %q", api.keys[0], asked[0])
	}

	// Episodes with no title, no series, no air time or no air date are dropped;
	// numbers may be strings, and the series id may be on the series.
	wantEps := []calEpisode{
		{Series: 9, Title: "Severance", Year: 2022, Season: 2, Episode: 5, Air: time.Date(2026, 10, 8, 1, 0, 0, 0, time.UTC), Monitored: true},
		{Series: 11, Title: "Brand New", Year: 2026, Season: 1, Episode: 1, Air: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC), Monitored: true},
		{Series: 12, Title: "Id Only In Series", Season: 1, Episode: 1, Air: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC), Monitored: true},
		{Series: 13, Title: "Unmonitored", Season: 1, Episode: 1, Air: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), Monitored: false},
	}
	if !reflect.DeepEqual(got, wantEps) {
		t.Fatalf("got:\n%+v\nwant:\n%+v", got, wantEps)
	}
}

func TestSonarrCalendarErrors(t *testing.T) {
	api := newFakeAPI(t, "X-Api-Key", map[string]string{"/api/v3/calendar": `{"not":"a list"}`})
	s, _ := newSonarr(api.srv.URL, "k", devClient)
	_, err := s.calendar(context.Background(), now, now.AddDate(0, 0, 30))
	if err == nil {
		t.Fatal("an answer that isn't a list should be an error")
	}
	if strings.Contains(err.Error(), "start=") || strings.Contains(err.Error(), "includeSeries") {
		t.Fatalf("the query doesn't belong in an error: %v", err)
	}
}

// ---------- Radarr's calendar and the date rule ----------

func TestRadarrDateKinds(t *testing.T) {
	d := func(s string) time.Time { return parseTime(s + "T00:00:00Z") }
	none := time.Time{}
	for _, c := range []struct {
		name                       string
		minAvail                   string
		cinemas, digital, physical time.Time
		wantDate                   time.Time
		wantKind                   string
	}{
		{"home release (digital first)", "released", d("2026-10-01"), d("2026-12-12"), d("2026-11-30"), d("2026-11-30"), "home"},
		{"in cinemas", "inCinemas", d("2026-10-20"), d("2026-12-12"), none, d("2026-10-20"), "cinemas"},
		{"in cinemas, no theatrical date", "inCinemas", none, none, d("2026-12-01"), d("2026-12-01"), "home"},
		{"announced: nothing to wait for", "announced", d("2027-01-01"), d("2027-02-01"), none, none, ""},
		{"released, only a theatrical date", "released", d("2026-10-20"), none, none, none, ""},
		{"capitals don't matter", "INCINEMAS", d("2026-10-20"), none, none, d("2026-10-20"), "cinemas"},
		{"nothing known", "released", none, none, none, none, ""},
	} {
		date, kind := radarrDate(c.minAvail, c.cinemas, c.digital, c.physical)
		if !date.Equal(c.wantDate) || kind != c.wantKind {
			t.Errorf("%s: got %v %q, want %v %q", c.name, date, kind, c.wantDate, c.wantKind)
		}
	}
}

const radarrCalendar = `[
 {"id":31,"title":"Home Movie","year":2026,"monitored":true,"hasFile":false,"minimumAvailability":"released",
  "inCinemas":"2026-09-01T00:00:00Z","digitalRelease":"2026-11-01T00:00:00Z","physicalRelease":"2026-12-01T00:00:00Z","overview":"ignored"},
 {"id":32,"title":"Cinema Movie","year":"2026","hasFile":false,"minimumAvailability":"inCinemas","inCinemas":"2026-10-20T00:00:00Z"},
 {"id":33,"title":"Have It","hasFile":true,"minimumAvailability":"released","digitalRelease":"2026-10-10T00:00:00Z"},
 {"id":34,"title":"Not Monitored","monitored":false,"minimumAvailability":"released","digitalRelease":"2026-10-10T00:00:00Z"},
 {"id":35,"title":"No Date","minimumAvailability":"announced","inCinemas":"2026-10-12T00:00:00Z"},
 {"id":36,"minimumAvailability":"released","digitalRelease":"2026-10-12T00:00:00Z"}
]`

func TestRadarrCalendar(t *testing.T) {
	api := newFakeAPI(t, "X-Api-Key", map[string]string{"/api/v3/calendar": radarrCalendar})
	r, err := newRadarr(api.srv.URL, "radarr-key", devClient)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 3, 0, 0, 0, 0, eastern)
	got, err := r.calendar(context.Background(), start, start.AddDate(0, 0, 30))
	if err != nil {
		t.Fatal(err)
	}

	asked := api.asked()
	if len(asked) != 1 {
		t.Fatalf("asked %v", asked)
	}
	u, _ := url.Parse(asked[0])
	// Release dates are calendar dates, so the range is too: no time of day,
	// and not shifted into UTC.
	want := url.Values{"start": {"2026-10-03"}, "end": {"2026-11-02"}, "unmonitored": {"false"}}
	if u.Path != "/api/v3/calendar" || !reflect.DeepEqual(u.Query(), want) {
		t.Errorf("asked %q", asked[0])
	}
	if api.keys[0] != "radarr-key" || strings.Contains(asked[0], "radarr-key") {
		t.Errorf("the key belongs in the header only: %q %q", api.keys[0], asked[0])
	}

	wantMovies := []calMovie{
		{ID: 31, Title: "Home Movie", Year: 2026, Monitored: true, Date: "2026-11-01", Kind: "home"},
		{ID: 32, Title: "Cinema Movie", Year: 2026, Monitored: true, Date: "2026-10-20", Kind: "cinemas"},
		{ID: 33, Title: "Have It", Monitored: true, HasFile: true, Date: "2026-10-10", Kind: "home"},
		{ID: 34, Title: "Not Monitored", Monitored: false, Date: "2026-10-10", Kind: "home"},
		{ID: 35, Title: "No Date", Monitored: true},
	}
	if !reflect.DeepEqual(got, wantMovies) {
		t.Fatalf("got:\n%+v\nwant:\n%+v", got, wantMovies)
	}
}

// ---------- turning the calendars into rows ----------

// ep makes an episode that airs at the given UTC time.
func ep(series int, title string, season, episode int, air string) calEpisode {
	return calEpisode{Series: series, Title: title, Season: season, Episode: episode, Air: parseTime(air), Monitored: true}
}

func movie(id int, title, date, kind string) calMovie {
	return calMovie{ID: id, Title: title, Monitored: true, Date: date, Kind: kind}
}

// Tests here use New York time and "now" of Oct 3, 7am there.
func coming(eps []calEpisode, movies []calMovie) []comingRow {
	rows, _ := buildComing(eps, movies, now, eastern)
	return rows
}

func TestBuildComingSaysWhichShowsItListed(t *testing.T) {
	var eps []calEpisode
	for i := 0; i < 50; i++ { // more shows than fit
		air := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC).Add(time.Duration(i) * 8 * time.Hour)
		eps = append(eps, ep(100+i, "Show "+strconv.Itoa(1000+i), 1, 2, air.Format(time.RFC3339)))
	}
	rows, listed := buildComing(eps, []calMovie{movie(7, "A Movie", "2026-10-05", "home")}, now, eastern)
	if len(rows) != maxComing {
		t.Fatalf("%d rows", len(rows))
	}
	// Only shows that made it into the list, with the date they are listed under.
	shows := 0
	for _, r := range rows {
		if r.Kind == "show" {
			shows++
		}
	}
	if len(listed) != shows {
		t.Fatalf("%d shows listed, but %d are in the rows", len(listed), shows)
	}
	if listed[100] == "" || listed[149] != "" {
		t.Fatalf("the soonest show should be listed and the cut-off one not: %q %q", listed[100], listed[149])
	}
	if _, ok := listed[7]; ok {
		t.Error("a movie's id is not a show's")
	}
}

func TestADatedTitleLosesAMatchingYear(t *testing.T) {
	for _, c := range []struct {
		title string
		year  int
		want  string
	}{
		{"WAR (2026)", 2026, "WAR"},
		{"Dune (2021)", 2021, "Dune"},
		{"WAR (2026)", 2025, "WAR (2026)"}, // a different year isn't the same thing
		{"WAR (2026)", 0, "WAR (2026)"},    // no year to compare with
		{"Shameless (US)", 2011, "Shameless (US)"},
		{"Blade Runner (1982) (2026)", 2026, "Blade Runner (1982)"},
		{"(2026)", 2026, "(2026)"}, // nothing would be left
		{"Fahrenheit 451", 1966, "Fahrenheit 451"},
	} {
		if got := dropYearSuffix(c.title, c.year); got != c.want {
			t.Errorf("%q (%d): got %q, want %q", c.title, c.year, got, c.want)
		}
	}
	rows := coming([]calEpisode{{Series: 1, Title: "WAR (2026)", Year: 2026, Season: 1, Episode: 2, Air: parseTime("2026-10-08T01:00:00Z"), Monitored: true}}, nil)
	if len(rows) != 1 || rows[0].Title != "WAR" || rows[0].Year != 2026 {
		t.Fatalf("got %+v", rows)
	}
}

// ---------- not saying the same thing on both tabs ----------

func TestTheNextEpisodeIsNotRepeatedOnRequests(t *testing.T) {
	weekly := requestRow{Title: "WAR", Kind: "show", State: "partial", Ready: 1, Date: "2026-10-08", series: 9}
	soon := requestRow{Title: "Premiere", Kind: "show", State: "soon", Date: "2026-10-08", series: 9}
	downloading := requestRow{Title: "Busy", Kind: "show", State: "partial", Ready: 1, Downloading: true, series: 9}
	movie := requestRow{Title: "Film", Kind: "movie", State: "soon", Date: "2026-10-08"}
	differentDay := requestRow{Title: "Other day", Kind: "show", State: "partial", Ready: 1, Date: "2026-10-15", series: 9}
	otherShow := requestRow{Title: "Other show", Kind: "show", State: "partial", Ready: 1, Date: "2026-10-08", series: 10}
	unknown := requestRow{Title: "No id", Kind: "show", State: "partial", Ready: 1, Date: "2026-10-08"}

	rows := []requestRow{weekly, soon, downloading, movie, differentDay, otherShow, unknown}
	trimRepeatedDates(rows, map[int]string{9: "2026-10-08"})

	want := []string{"", "2026-10-08", "", "2026-10-08", "2026-10-15", "2026-10-08", "2026-10-08"}
	for i, r := range rows {
		if r.Date != want[i] {
			t.Errorf("%s: date %q, want %q", r.Title, r.Date, want[i])
		}
	}
	// Nothing else about the row changes.
	if rows[0].State != "partial" || rows[0].Ready != 1 || rows[0].Title != "WAR" {
		t.Errorf("row changed: %+v", rows[0])
	}
	// Without a Coming up list, nothing is trimmed.
	rows = []requestRow{weekly}
	trimRepeatedDates(rows, nil)
	if rows[0].Date != "2026-10-08" {
		t.Errorf("date lost with no calendar: %+v", rows[0])
	}
}

func TestOneRowPerShowAtItsNextEpisode(t *testing.T) {
	got := coming([]calEpisode{
		ep(1, "Weekly", 1, 6, "2026-10-15T01:00:00Z"),
		ep(1, "Weekly", 1, 5, "2026-10-08T01:00:00Z"), // out of order on purpose
		ep(1, "Weekly", 1, 7, "2026-10-22T01:00:00Z"),
	}, nil)
	want := []comingRow{{Title: "Weekly", Kind: "show", Date: "2026-10-07", Note: "episode", arrID: 1}} // 9pm on the 7th in New York
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestSeveralEpisodesTheSameDay(t *testing.T) {
	got := coming([]calEpisode{
		ep(1, "Drop", 1, 3, "2026-10-09T12:00:00Z"),
		ep(1, "Drop", 1, 2, "2026-10-09T12:00:00Z"),
		ep(1, "Drop", 1, 4, "2026-10-09T16:00:00Z"),
		ep(1, "Drop", 1, 5, "2026-10-16T12:00:00Z"), // another day: not counted
	}, nil)
	want := []comingRow{{Title: "Drop", Kind: "show", Date: "2026-10-09", Note: "episodes", Count: 3, arrID: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestASeasonPremiere(t *testing.T) {
	got := coming([]calEpisode{
		ep(1, "Returning", 3, 1, "2026-10-20T12:00:00Z"),
		ep(1, "Returning", 3, 2, "2026-10-20T12:00:00Z"), // a whole season at once: still a premiere
		ep(2, "Brand New", 1, 1, "2026-10-21T12:00:00Z"),
	}, nil)
	want := []comingRow{
		{Title: "Returning", Kind: "show", Date: "2026-10-20", Note: "premiere", Season: 3, arrID: 1},
		{Title: "Brand New", Kind: "show", Date: "2026-10-21", Note: "premiere", Season: 1, arrID: 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestEpisodesThatHaveAlreadyAiredAreNotComingUp(t *testing.T) {
	// "now" is 12:00 UTC on Oct 3.
	got := coming([]calEpisode{
		ep(1, "This Morning", 1, 4, "2026-10-03T10:00:00Z"), // aired two hours ago
		ep(1, "This Morning", 1, 5, "2026-10-10T10:00:00Z"),
		ep(2, "Tonight", 1, 2, "2026-10-04T02:00:00Z"), // 10pm tonight in New York
		ep(3, "Only Past", 1, 1, "2026-10-01T10:00:00Z"),
	}, nil)
	want := []comingRow{
		{Title: "Tonight", Kind: "show", Date: "2026-10-03", Note: "episode", arrID: 2},
		{Title: "This Morning", Kind: "show", Date: "2026-10-10", Note: "episode", arrID: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestShowsLeftOut(t *testing.T) {
	unmonitored := ep(2, "Unmonitored", 1, 1, "2026-10-09T12:00:00Z")
	unmonitored.Monitored = false
	got := coming([]calEpisode{
		ep(1, "Special", 0, 1, "2026-10-09T12:00:00Z"), // season 0
		unmonitored,
		ep(3, "Too Far Off", 1, 1, "2027-04-03T12:00:00Z"), // the day after the window
		ep(4, "Last Day", 1, 2, "2027-04-02T12:00:00Z"),    // the window's last day
		{Series: 5, Title: "", Season: 1, Episode: 1, Air: parseTime("2026-10-09T12:00:00Z"), Monitored: true},
		{Series: 6, Title: "No Time", Season: 1, Episode: 1, Monitored: true},
		{Series: 7, Title: "\U0000200b \U0000202e", Season: 1, Episode: 1, Air: parseTime("2026-10-09T12:00:00Z"), Monitored: true}, // nothing left once cleaned
	}, nil)
	want := []comingRow{{Title: "Last Day", Kind: "show", Date: "2027-04-02", Note: "episode", arrID: 4}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestAPremiereMonthsAwayIsListed(t *testing.T) {
	// A show requested now that doesn't start until Dec 25 is 83 days off:
	// well past a month, but it belongs on the calendar.
	got := coming([]calEpisode{ep(1, "Far Off", 1, 1, "2026-12-25T12:00:00Z")}, []calMovie{movie(1, "Far Film", "2027-03-01", "home")})
	want := []comingRow{
		{Title: "Far Off", Kind: "show", Date: "2026-12-25", Note: "premiere", Season: 1, arrID: 1},
		{Title: "Far Film", Kind: "movie", Date: "2027-03-01", Note: "home", arrID: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestAShowThatReturnsAfterBreakKeepsItsOwnSeasonAndEpisode(t *testing.T) {
	// Episode 1 of a later season is a premiere; episode 4 of the same season is not.
	got := coming([]calEpisode{ep(1, "Back", 2, 4, "2026-10-09T12:00:00Z")}, nil)
	if len(got) != 1 || got[0].Note != "episode" || got[0].Season != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestMovies(t *testing.T) {
	have := movie(3, "Have It", "2026-10-10", "home")
	have.HasFile = true
	unmonitored := movie(4, "Unmonitored", "2026-10-10", "home")
	unmonitored.Monitored = false
	got := coming(nil, []calMovie{
		movie(1, "In Cinemas", "2026-10-20", "cinemas"),
		movie(2, "At Home", "2026-10-12", "home"),
		have, unmonitored,
		movie(5, "Today", "2026-10-03", "home"),     // still shows, like a show airing tonight
		movie(6, "Yesterday", "2026-10-02", "home"), // out already: the Requests tab's business
		movie(7, "Last Day", "2027-04-02", "home"),
		movie(8, "Too Far Off", "2027-04-03", "home"),
		movie(9, "No Date", "", ""),
		movie(10, "Bad Date", "soon", "home"),
		movie(11, "", "2026-10-12", "home"),
		movie(2, "At Home Again", "2026-10-12", "home"), // the same movie twice
	})
	want := []comingRow{
		{Title: "Today", Kind: "movie", Date: "2026-10-03", Note: "home", arrID: 5},
		{Title: "At Home", Kind: "movie", Date: "2026-10-12", Note: "home", arrID: 2},
		{Title: "In Cinemas", Kind: "movie", Date: "2026-10-20", Note: "cinemas", arrID: 1},
		{Title: "Last Day", Kind: "movie", Date: "2027-04-02", Note: "home", arrID: 7},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestMovieAndShowOrderAndYear(t *testing.T) {
	show := ep(1, "banana", 1, 2, "2026-10-12T12:00:00Z")
	show.Year = 2024
	showB := ep(2, "Apple", 1, 2, "2026-10-12T12:00:00Z")
	m := movie(1, "Cherry", "2026-10-12", "home")
	m.Year = 1999
	bad := movie(2, "Absurd Year", "2026-10-12", "home")
	bad.Year = 99999
	got := coming([]calEpisode{show, showB}, []calMovie{m, bad})
	var order []string
	for _, r := range got {
		order = append(order, r.Title+"/"+strconv.Itoa(r.Year))
	}
	// Soonest first, then by title without regard to capitals; a year that
	// can't be right is left off.
	if want := "Absurd Year/0 Apple/0 banana/2024 Cherry/1999"; strings.Join(order, " ") != want {
		t.Fatalf("order %v", order)
	}
}

func TestTitlesAreCleanedAndCut(t *testing.T) {
	long := strings.Repeat("x", 300)
	got := coming(nil, []calMovie{
		movie(1, "A\U0000202eB\U0000200b\n  C", "2026-10-12", "home"),
		movie(2, long, "2026-10-13", "home"),
	})
	if got[0].Title != "AB C" {
		t.Errorf("%q", got[0].Title)
	}
	if n := len([]rune(got[1].Title)); n != 120 {
		t.Errorf("a long title was cut to %d", n)
	}
}

func TestComingIsCapped(t *testing.T) {
	var eps []calEpisode
	for i := 0; i < 70; i++ {
		air := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC).Add(time.Duration(i) * 8 * time.Hour)
		eps = append(eps, ep(i+1, "Show "+strconv.Itoa(1000+i), 1, 2, air.Format(time.RFC3339)))
	}
	got := coming(eps, nil)
	if len(got) != maxComing {
		t.Fatalf("%d rows", len(got))
	}
	if got[0].Title != "Show 1000" || got[maxComing-1].Title != "Show 1039" {
		t.Fatalf("kept %q .. %q: the soonest should be kept", got[0].Title, got[maxComing-1].Title)
	}
}

func TestNothingComing(t *testing.T) {
	if got := coming(nil, nil); got == nil || len(got) != 0 {
		t.Fatalf("an empty list should be empty, not nil, so it reads as [] in JSON: %#v", got)
	}
}
