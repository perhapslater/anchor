package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The upstream total counts every library item, including audiobooks without
// an ebook. Reaching the page bound with a complete mixed library is valid.
func TestQAEbookMixedCatalogAtPageBoundary(t *testing.T) {
	s, cfg, _ := newEbookTest(t)
	var calls atomic.Int32
	s.readClient.Transport = ebookDownloadTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil {
			return nil, err
		}
		items := make([]absLibraryItem, ebookPageSize)
		for i := range items {
			items[i] = absLibraryItem{ID: fmt.Sprintf("audio%d", page*ebookPageSize+i), MediaType: "book"}
		}
		items[0].Media = absBook{EbookFormat: "epub", Metadata: absBookMetadata{Title: fmt.Sprintf("Ebook %d", page)}}
		body, err := json.Marshal(absLibraryItems{Total: ebookPageSize * ebookMaxPages, Results: items})
		if err != nil {
			return nil, err
		}
		return ebookReply(http.StatusOK, body), nil
	})
	rows, err := s.fetchCatalog(context.Background(), cfg.Ebooks)
	if err != nil || len(rows) != ebookMaxPages || calls.Load() != ebookMaxPages {
		t.Fatalf("complete mixed catalog: rows=%d calls=%d err=%v", len(rows), calls.Load(), err)
	}
}

// Observe the waiter reaching its select without a scheduler-dependent sleep.
type qaWaitContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (c *qaWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

func TestQARequestConfigurationChangeDuringBuild(t *testing.T) {
	rs, _, _, cfg := newFakeService(t)
	changed := *cfg
	rc := *cfg.Requests
	rc.Seerr = "https://new-seerr.test"
	changed.Requests = &rc
	entered, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	var calls atomic.Int32
	rs.build = func(ctx context.Context, cfg *Config) (*snapshot, error) {
		calls.Add(1)
		if cfg.Requests.Seerr != rc.Seerr {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return &snapshot{Rows: []requestRow{{Title: cfg.Requests.Seerr}}}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	oldResult, newResult := make(chan requestResult, 1), make(chan requestResult, 1)
	go func() { oldResult <- rs.get(ctx, cfg) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("old build did not start")
	}
	waiting := &qaWaitContext{Context: ctx, observed: make(chan struct{})}
	go func() { newResult <- rs.get(waiting, &changed) }()
	select {
	case <-waiting.observed:
	case <-ctx.Done():
		t.Fatal("new request did not wait for active build")
	}
	unblock.Do(func() { close(release) })
	old, newer := <-oldResult, <-newResult
	if old.err != nil || old.snap == nil || old.snap.Rows[0].Title != cfg.Requests.Seerr {
		t.Fatalf("old request lost its result: %+v", old)
	}
	if newer.err != nil || newer.snap == nil || newer.snap.Rows[0].Title != rc.Seerr || calls.Load() != 2 {
		t.Fatalf("new settings were not fetched: result=%+v builds=%d", newer, calls.Load())
	}
}

func TestQARequestFailureIsScopedToConfiguration(t *testing.T) {
	rs, fb, _, cfg := newFakeService(t)
	if result := rs.get(context.Background(), cfg); result.err != nil {
		t.Fatal(result.err)
	}
	changed := *cfg
	rc := *cfg.Requests
	rc.Seerr = "https://unavailable-seerr.test"
	changed.Requests = &rc
	fb.fail.Store(true)
	if result := rs.get(context.Background(), &changed); result.err == nil || result.snap != nil {
		t.Fatal("failed changed configuration exposed another snapshot")
	}
	result := rs.get(context.Background(), cfg)
	if result.err != nil || result.snap == nil || result.stale {
		t.Fatalf("unrelated configuration failure marked healthy cached data stale: %+v", result)
	}
}

func TestQARequestWaiterCancellationOnConfigurationChange(t *testing.T) {
	rs, _, _, cfg := newFakeService(t)
	entered := make(chan struct{})
	rs.build = func(ctx context.Context, _ *Config) (*snapshot, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	shared, stop := context.WithCancel(context.Background())
	defer stop()
	rs.ctx = shared
	oldResult := make(chan requestResult, 1)
	go func() { oldResult <- rs.get(shared, cfg) }()
	<-entered
	changed := *cfg
	rc := *cfg.Requests
	rc.Seerr = "https://changed.test"
	changed.Requests = &rc
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if result := rs.get(ctx, &changed); !errors.Is(result.err, context.Canceled) {
		t.Fatalf("canceled config waiter: %+v", result)
	}
	stop()
	<-oldResult
}

func TestQAEbookCatalogStopsAfterAllUnfilteredItems(t *testing.T) {
	s, cfg, _ := newEbookTest(t)
	var calls atomic.Int32
	s.readClient.Transport = ebookDownloadTransport(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.URL.Query().Get("page") != "0" {
			t.Error("requested a page beyond the complete library")
			return ebookReply(http.StatusInternalServerError, nil), nil
		}
		items := make([]absLibraryItem, ebookPageSize)
		for i := range items {
			items[i] = absLibraryItem{ID: fmt.Sprintf("audio%d", i), MediaType: "book"}
		}
		body, err := json.Marshal(absLibraryItems{Total: ebookPageSize, Results: items})
		if err != nil {
			return nil, err
		}
		return ebookReply(http.StatusOK, body), nil
	})
	rows, err := s.fetchCatalog(context.Background(), cfg.Ebooks)
	if err != nil || len(rows) != 0 || calls.Load() != 1 {
		t.Fatalf("audio-only catalog: rows=%d calls=%d err=%v", len(rows), calls.Load(), err)
	}
}

func TestQAEbookCatalogKeepsPageLimit(t *testing.T) {
	s, cfg, _ := newEbookTest(t)
	items := make([]absLibraryItem, ebookPageSize)
	body, err := json.Marshal(absLibraryItems{Total: ebookPageSize*ebookMaxPages + 1, Results: items})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	s.readClient.Transport = ebookDownloadTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return ebookReply(http.StatusOK, body), nil
	})
	if rows, err := s.fetchCatalog(context.Background(), cfg.Ebooks); err == nil || rows != nil || calls.Load() != ebookMaxPages {
		t.Fatalf("incomplete oversized catalog accepted: rows=%v calls=%d err=%v", rows, calls.Load(), err)
	}
}
