package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const ebookTestPage = `{"total":1,"results":[{"id":"book1","mediaType":"book","addedAt":123,"media":{"ebookFormat":"epub","metadata":{"title":"<Book>","authorName":"Author"}}}]}`

func ebookReply(status int, body []byte) *http.Response {
	return &http.Response{StatusCode: status, Status: fmt.Sprint(status), Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body))}
}

func newEbookTest(t *testing.T) (*ebookService, *Config, *clock) {
	t.Helper()
	cfg, err := parseConfig([]byte(apiConfig))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Ebooks = &EbooksConfig{App: "seerr", Server: "https://abs.test", LibraryID: "lib"}
	s := newEbookService("test-key", false)
	ck := &clock{t: time.Now()}
	s.now = ck.Now
	return s, cfg, ck
}

func warmEbookTest(s *ebookService, cfg *Config, books ...ebook) {
	s.snapshot = &ebookSnapshot{Rows: books, At: s.now(), sig: cfg.Ebooks.sig(s.key)}
}

func TestEbookRefreshCooldownAndFailureBackoff(t *testing.T) {
	s, cfg, ck := newEbookTest(t)
	var calls atomic.Int32
	var fail atomic.Bool
	s.readClient.Transport = ebookDownloadTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer test-key" || strings.Contains(r.URL.RawQuery, "test-key") {
			t.Error("credential routing")
		}
		if fail.Load() {
			return ebookReply(503, nil), nil
		}
		return ebookReply(200, []byte(ebookTestPage)), nil
	})
	ctx := context.Background()
	if snap, _, err := s.get(ctx, cfg, false); err != nil || len(snap.Rows) != 1 {
		t.Fatalf("initial catalog: %v", err)
	}
	for i := 0; i < 3; i++ {
		s.get(ctx, cfg, true)
	}
	if calls.Load() != 1 {
		t.Fatal("forced cooldown did not suppress work")
	}
	ck.advance(ebookForcedGap + time.Second)
	s.get(ctx, cfg, true)
	if calls.Load() != 2 {
		t.Fatal("refresh did not resume")
	}
	ck.advance(ebookForcedGap + time.Second)
	fail.Store(true)
	if snap, stale, err := s.get(ctx, cfg, true); snap == nil || !stale || err == nil {
		t.Fatal("failed refresh did not retain stale snapshot")
	}
	for _, force := range []bool{false, true, true, false} {
		s.get(ctx, cfg, force)
	}
	if calls.Load() != 3 {
		t.Fatal("failure backoff bypassed")
	}
	ck.advance(ebookFailBackoff + time.Second)
	fail.Store(false)
	if _, stale, err := s.get(ctx, cfg, true); stale || err != nil {
		t.Fatalf("failed to recover: %v", err)
	}
	if calls.Load() != 4 {
		t.Fatal("recovery fetch missing")
	}
}

func TestEbookChangedConfigurationBackoffAndIsolation(t *testing.T) {
	s, cfg, _ := newEbookTest(t)
	warmEbookTest(s, cfg, ebook{ID: "old", Title: "Private old library", Format: "epub"})
	changed := *cfg
	e := *cfg.Ebooks
	e.LibraryID = "another"
	changed.Ebooks = &e
	var calls atomic.Int32
	s.readClient.Transport = ebookDownloadTransport(func(*http.Request) (*http.Response, error) { calls.Add(1); return ebookReply(503, nil), nil })
	for _, force := range []bool{false, true, false, true} {
		if snap, _, err := s.get(context.Background(), &changed, force); snap != nil || err == nil {
			t.Fatal("old library escaped its configuration signature")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("changed-library failure bypassed backoff")
	}
	if _, err := s.book(context.Background(), &changed, "old"); err == nil {
		t.Fatal("old library book authorized")
	}
}

func TestEbookForcedHourlyBudget(t *testing.T) {
	s, cfg, ck := newEbookTest(t)
	s.forced.perHour = 2
	var calls atomic.Int32
	s.readClient.Transport = ebookDownloadTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return ebookReply(200, []byte(ebookTestPage)), nil
	})
	s.get(context.Background(), cfg, true)
	ck.advance(time.Minute)
	s.get(context.Background(), cfg, true)
	ck.advance(time.Minute)
	s.get(context.Background(), cfg, true)
	if calls.Load() != 2 {
		t.Fatal("hourly refresh work budget bypassed")
	}
}

func TestEbookCatalogSingleFlightSurvivesCallerCancellation(t *testing.T) {
	s, cfg, _ := newEbookTest(t)
	entered, hold := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(hold) }) }
	defer release()
	var calls atomic.Int32
	s.readClient.Transport = ebookDownloadTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		entered <- struct{}{}
		select {
		case <-hold:
			return ebookReply(200, []byte(ebookTestPage)), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := s.get(ctx, cfg, false); done <- err }()
	<-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("departed caller did not stop waiting")
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			snap, _, err := s.get(context.Background(), cfg, false)
			if err != nil || snap == nil {
				t.Error("shared build failed")
			}
		}()
	}
	release()
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("catalog fetches=%d", calls.Load())
	}
}

func TestEbookCoversShareBoundedCacheAndNegativeResults(t *testing.T) {
	s, cfg, ck := newEbookTest(t)
	warmEbookTest(s, cfg, ebook{ID: "book1", Format: "epub"})
	var calls atomic.Int32
	var fail atomic.Bool
	pic := []byte("\x89PNG\r\n\x1a\nsmall picture")
	s.readClient.Transport = ebookDownloadTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		if fail.Load() {
			return ebookReply(404, nil), nil
		}
		return ebookReply(200, pic), nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := s.cover(context.Background(), cfg, "book1"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("same cover was fetched more than once")
	}
	ck.advance(posterTTL + time.Second)
	warmEbookTest(s, cfg, ebook{ID: "book1", Format: "epub"})
	s.cover(context.Background(), cfg, "book1")
	entries, n := s.covers.stats()
	if entries != 1 || n != len(pic) {
		t.Fatalf("replacement accounting: entries=%d bytes=%d", entries, n)
	}
	ck.advance(posterTTL + time.Second)
	warmEbookTest(s, cfg, ebook{ID: "book1", Format: "epub"})
	fail.Store(true)
	for i := 0; i < 3; i++ {
		if _, _, err := s.cover(context.Background(), cfg, "book1"); err == nil {
			t.Fatal("missing cover accepted")
		}
	}
	if calls.Load() != 3 {
		t.Fatal("negative cover caching missing")
	}
	entries, n = s.covers.stats()
	if entries != 1 || n != 0 {
		t.Fatal("miss retained image bytes")
	}
}

func TestEbookDifferentCoverFetchesAreBounded(t *testing.T) {
	s, cfg, _ := newEbookTest(t)
	books := make([]ebook, 12)
	for i := range books {
		books[i] = ebook{ID: fmt.Sprintf("b%d", i), Format: "epub"}
	}
	warmEbookTest(s, cfg, books...)
	entered := make(chan struct{}, 12)
	hold := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(hold) }) }
	defer release()
	g := &gauge{}
	s.readClient.Transport = ebookDownloadTransport(func(r *http.Request) (*http.Response, error) {
		g.enter()
		defer g.leave()
		entered <- struct{}{}
		select {
		case <-hold:
			return ebookReply(200, []byte("\x89PNG\r\n\x1a\n")), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	var wg sync.WaitGroup
	for _, book := range books {
		wg.Add(1)
		go func(id string) { defer wg.Done(); s.cover(context.Background(), cfg, id) }(book.ID)
	}
	for i := 0; i < posterFetches; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("cover workers did not start")
		}
	}
	select {
	case <-entered:
		t.Fatal("too many active cover fetches")
	case <-time.After(30 * time.Millisecond):
	}
	release()
	wg.Wait()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.max > posterFetches {
		t.Fatalf("parallel covers=%d", g.max)
	}
}

func TestEbookAPIAuthorizationAndNoStore(t *testing.T) {
	for _, route := range []string{"list", "refresh", "cover", "download"} {
		for _, login := range []string{"", "unknown@example.com", "dad@example.com", "mom@example.com", "admin@example.com"} {
			for _, disabled := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/disabled=%v", route, login, disabled), func(t *testing.T) {
					s, cfg, _ := newEbookTest(t)
					cfg.appByID("seerr").Disabled = disabled
					warmEbookTest(s, cfg, ebook{ID: "book1", Format: "epub"})
					s.readClient.Transport = ebookDownloadTransport(func(r *http.Request) (*http.Response, error) {
						if strings.Contains(r.URL.Path, "/cover") {
							return ebookReply(200, []byte("\x89PNG\r\n\x1a\n")), nil
						}
						return ebookReply(200, []byte(ebookTestPage)), nil
					})
					s.streamClient.Transport = ebookDownloadTransport(func(*http.Request) (*http.Response, error) { return ebookReply(200, []byte("PK\x03\x04book")), nil })
					h := &handler{store: &ConfigStore{cfg: cfg}, ebooks: s, booksReads: newRateLimiter(time.Second, 100), bookCovers: newBucketLimiter(60, time.Second), bookFiles: newRateLimiter(time.Second, 100)}
					r := httptest.NewRequest("GET", "https://anchor.test/api/ebooks", strings.NewReader("{}"))
					r.RemoteAddr = loopback
					r.Header.Set("Tailscale-User-Login", login)
					r.Header.Set("Origin", "https://anchor.test")
					r.Header.Set("Content-Type", "application/json")
					r.SetPathValue("id", "book1")
					if route == "refresh" {
						r.Method = "POST"
					}
					w := &ebookDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
					var fn http.HandlerFunc
					switch route {
					case "list", "refresh":
						fn = h.ebooksList
					case "cover":
						fn = h.ebookCover
					case "download":
						fn = h.ebookDownload
					}
					securityHeaders(fn).ServeHTTP(w, r)
					want := 404
					if !disabled && (login == "mom@example.com" || login == "admin@example.com") {
						want = 200
					}
					if w.Code != want || w.Header().Get("Cache-Control") != "no-store" {
						t.Fatalf("status=%d cache=%q", w.Code, w.Header().Get("Cache-Control"))
					}
				})
			}
		}
	}
}

func TestEbookInvalidAndUnlistedIDsDoNotFetch(t *testing.T) {
	s, cfg, _ := newEbookTest(t)
	warmEbookTest(s, cfg, ebook{ID: "book1", Format: "epub"})
	var calls atomic.Int32
	s.readClient.Transport = ebookDownloadTransport(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, errors.New("unexpected") })
	for _, id := range []string{"../secret", "http://elsewhere", "missing", ""} {
		if _, _, err := s.cover(context.Background(), cfg, id); err == nil {
			t.Fatalf("accepted %q", id)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("unlisted object caused upstream traffic")
	}
}

func TestEbookGateRejectsForgedAdminAndBadRefresh(t *testing.T) {
	for _, reason := range []string{"external forged", "disabled", "cross-origin refresh", "missing JSON"} {
		t.Run(reason, func(t *testing.T) {
			s, cfg, _ := newEbookTest(t)
			warmEbookTest(s, cfg, ebook{ID: "book1", Format: "epub"})
			var calls atomic.Int32
			s.readClient.Transport = ebookDownloadTransport(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, errors.New("unexpected") })
			h := &handler{store: &ConfigStore{cfg: cfg}, ebooks: s, booksReads: newRateLimiter(time.Second, 100)}
			r := httptest.NewRequest("POST", "https://anchor.test/api/ebooks/refresh", strings.NewReader("{}"))
			r.RemoteAddr = loopback
			r.Header.Set("Tailscale-User-Login", "admin@example.com")
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Origin", "https://anchor.test")
			want := 404
			switch reason {
			case "external forged":
				r.RemoteAddr = "192.0.2.1:1234"
			case "disabled":
				cfg.Ebooks = nil
			case "cross-origin refresh":
				r.Header.Set("Origin", "https://other.test")
				want = 403
			case "missing JSON":
				r.Header.Del("Content-Type")
				want = 415
			}
			w := httptest.NewRecorder()
			h.ebooksList(w, r)
			if w.Code != want || calls.Load() != 0 {
				t.Fatalf("status=%d calls=%d", w.Code, calls.Load())
			}
		})
	}
}

func TestEbookConfigurationSwitchDuringSharedBuild(t *testing.T) {
	s, cfg, _ := newEbookTest(t)
	changed := *cfg
	e := *cfg.Ebooks
	e.LibraryID = "new-library"
	changed.Ebooks = &e
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var calls atomic.Int32
	s.readClient.Transport = ebookDownloadTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if strings.Contains(r.URL.Path, "/lib/") {
			entered <- struct{}{}
			<-release
			return ebookReply(200, []byte(ebookTestPage)), nil
		}
		return ebookReply(200, []byte(strings.ReplaceAll(ebookTestPage, "book1", "new-book"))), nil
	})
	oldResult := make(chan *ebookSnapshot, 1)
	newResult := make(chan *ebookSnapshot, 1)
	go func() { snap, _, _ := s.get(context.Background(), cfg, false); oldResult <- snap }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("old catalog did not start")
	}
	go func() { snap, _, _ := s.get(context.Background(), &changed, false); newResult <- snap }()
	unblock()
	old, newer := <-oldResult, <-newResult
	if old == nil || newer == nil || old.Rows[0].ID != "book1" || newer.Rows[0].ID != "new-book" || calls.Load() != 2 {
		t.Fatal("in-flight configuration results mixed or rebuilt")
	}
}

func TestEbookCatalogPagesFiltersAndSortsMetadata(t *testing.T) {
	s, cfg, _ := newEbookTest(t)
	items := make([]absLibraryItem, ebookPageSize)
	for i := range items {
		items[i] = absLibraryItem{ID: fmt.Sprintf("podcast%d", i), MediaType: "podcast"}
	}
	items[0] = absLibraryItem{ID: "first", MediaType: "book", AddedAt: 10, Media: absBook{EbookFormat: "epub", Metadata: absBookMetadata{Title: "  Book\u202e   One "}}}
	items[1] = absLibraryItem{ID: "second", MediaType: "book", AddedAt: 100, Media: absBook{EbookFormat: "pdf", Metadata: absBookMetadata{Title: "Second"}}}
	items[2] = items[0]
	items[2].ID = "../bad"
	items[3] = items[0]
	items[3].ID = "missing"
	items[3].IsMissing = true
	items[4] = items[0]
	items[4].ID = "unsupported"
	items[4].Media.EbookFormat = "html"
	last := absLibraryItem{ID: "last", MediaType: "book", AddedAt: 200, Media: absBook{EbookFormat: "mobi", EbookFile: &absEbookFile{Ino: "777", EbookFormat: "cbz"}, Metadata: absBookMetadata{Title: "Last"}}}
	var calls atomic.Int32
	s.readClient.Transport = ebookDownloadTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		page := absLibraryItems{Total: ebookPageSize + 1, Results: items}
		if r.URL.Query().Get("page") == "1" {
			page.Results = []absLibraryItem{last}
		}
		data, err := json.Marshal(page)
		if err != nil {
			return nil, err
		}
		return ebookReply(200, data), nil
	})
	rows, err := s.fetchCatalog(context.Background(), cfg.Ebooks)
	if err != nil || len(rows) != 3 || calls.Load() != 2 {
		t.Fatalf("paged catalog: rows=%d calls=%d err=%v", len(rows), calls.Load(), err)
	}
	if rows[0].ID != "last" || rows[0].Format != "cbz" || rows[0].FileIno != "777" || rows[1].ID != "second" || rows[2].Title != "Book One" {
		t.Fatalf("catalog contract: %+v", rows)
	}
}
