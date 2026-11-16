package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type ebookDownloadTransport func(*http.Request) (*http.Response, error)

func (f ebookDownloadTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Real net/http responses support write deadlines. Recorders must explicitly
// provide the same capability to exercise this streaming path.
type ebookDeadlineRecorder struct {
	*httptest.ResponseRecorder
	deadline time.Time
}

func (w *ebookDeadlineRecorder) SetWriteDeadline(d time.Time) error { w.deadline = d; return nil }

func ebookDownloadFixture(t *testing.T, data []byte, length int64) (*ebookService, *EbooksConfig, *ebook) {
	t.Helper()
	s := newEbookService("test-key", false)
	s.transfer = ebookTransferLimits{maxBytes: 1 << 20, total: 3 * time.Second, idle: time.Second}
	s.streamClient.Transport = ebookDownloadTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer test-key" || r.URL.RawQuery != "" {
			t.Error("download credential was not confined to its header")
		}
		if _, ok := r.Context().Deadline(); !ok {
			t.Error("download has no total deadline")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data)), ContentLength: length}, nil
	})
	return s, &EbooksConfig{App: "books", Server: "https://abs.test", LibraryID: "lib"}, &ebook{ID: "book1", Title: "A book", Format: "epub"}
}

func assertNoEbookDownloadSlots(t *testing.T, s *ebookService) {
	t.Helper()
	s.downloads.mu.Lock()
	defer s.downloads.mu.Unlock()
	if s.downloads.total != 0 || len(s.downloads.active) != 0 {
		t.Fatalf("transfer slots leaked: total=%d people=%v", s.downloads.total, s.downloads.active)
	}
}

func TestEbookDownloadBodiesAndSizeLimits(t *testing.T) {
	data := []byte("PK\x03\x04" + strings.Repeat("book data", 200))
	for _, tc := range []struct {
		name        string
		length, max int64
		wantStarted bool
		wantErr     bool
	}{
		{"known", int64(len(data)), int64(len(data) + 1), true, false},
		{"unknown", -1, int64(len(data) + 1), true, false},
		{"exact known limit", int64(len(data)), int64(len(data)), true, false},
		{"exact unknown limit", -1, int64(len(data)), true, false},
		{"declared oversized", int64(len(data)), 600, false, true},
		{"streamed oversized", -1, 600, true, true},
		{"lying length", 600, 600, true, true},
		{"truncated body", int64(len(data) + 10), int64(len(data) + 20), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, cfg, book := ebookDownloadFixture(t, data, tc.length)
			s.transfer.maxBytes = tc.max
			w := &ebookDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			started, err := s.download(context.Background(), cfg, book, "member", w)
			if started != tc.wantStarted || (err != nil) != tc.wantErr {
				t.Fatalf("started=%v err=%v", started, err)
			}
			if int64(w.Body.Len()) > tc.max {
				t.Fatal("forwarded bytes exceeded policy")
			}
			if !tc.wantErr && (!bytes.Equal(w.Body.Bytes(), data) || w.Header().Get("Content-Type") != "application/epub+zip" || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment;")) {
				t.Fatal("valid attachment changed")
			}
			if started && w.deadline.IsZero() {
				t.Fatal("final HTTP framing was left without a write deadline")
			}
			assertNoEbookDownloadSlots(t, s)
		})
	}
}

func TestEbookDownloadRejectsErrorPagesAndEmptyBodies(t *testing.T) {
	for _, tc := range []struct{ kind, data string }{
		{"text/html; charset=utf-8", "login"},
		{"application/problem+json", `{"error":"denied"}`},
		{"application/octet-stream", "\xef\xbb\xbf  <!DOCTYPE HTML><html>login</html>"},
		{"application/epub+zip", "<HTML>login</HTML>"},
		{"application/octet-stream", `{"error":"denied"}`},
		{"", ""},
	} {
		t.Run(tc.kind+tc.data, func(t *testing.T) {
			s, cfg, book := ebookDownloadFixture(t, nil, -1)
			s.streamClient.Transport = ebookDownloadTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {tc.kind}}, Body: io.NopCloser(strings.NewReader(tc.data)), ContentLength: -1}, nil
			})
			w := &ebookDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			started, err := s.download(context.Background(), cfg, book, "member", w)
			if started || err == nil || w.Body.Len() != 0 || w.Header().Get("Content-Disposition") != "" {
				t.Fatalf("error page became attachment: started=%v err=%v", started, err)
			}
			assertNoEbookDownloadSlots(t, s)
		})
	}
}

type ebookStallBody struct {
	ctx     context.Context
	prefix  []byte
	trickle bool
	closed  atomic.Bool
}

func (b *ebookStallBody) Read(p []byte) (int, error) {
	if len(b.prefix) != 0 {
		n := copy(p, b.prefix)
		b.prefix = b.prefix[n:]
		return n, nil
	}
	if b.trickle {
		select {
		case <-time.After(5 * time.Millisecond):
			p[0] = 'b'
			return 1, nil
		case <-b.ctx.Done():
			return 0, b.ctx.Err()
		}
	}
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *ebookStallBody) Close() error { b.closed.Store(true); return nil }

func TestEbookDownloadStallsTotalDeadlineAndCancellation(t *testing.T) {
	for _, name := range []string{"before body", "mid body", "total despite progress", "caller cancellation"} {
		t.Run(name, func(t *testing.T) {
			s, cfg, book := ebookDownloadFixture(t, nil, -1)
			s.transfer.idle = 50 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var body *ebookStallBody
			s.streamClient.Transport = ebookDownloadTransport(func(r *http.Request) (*http.Response, error) {
				body = &ebookStallBody{ctx: r.Context()}
				if name != "before body" {
					body.prefix = bytes.Repeat([]byte{'b'}, 512)
				}
				if name == "total despite progress" {
					body.trickle = true
				}
				if name == "caller cancellation" {
					time.AfterFunc(20*time.Millisecond, cancel)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body, ContentLength: -1}, nil
			})
			if name == "total despite progress" {
				s.transfer.total, s.transfer.idle = 60*time.Millisecond, time.Second
			}
			w := &ebookDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			before := time.Now()
			started, err := s.download(ctx, cfg, book, "member", w)
			if err == nil || started != (name != "before body") || !body.closed.Load() {
				t.Fatalf("started=%v err=%v closed=%v", started, err, body.closed.Load())
			}
			if time.Since(before) > time.Second {
				t.Fatal("stalled transfer did not stop promptly")
			}
			assertNoEbookDownloadSlots(t, s)
		})
	}
}

func TestEbookDownloadLimitsActiveWork(t *testing.T) {
	l := newEbookDownloadLimiter()
	var releases []func()
	for _, login := range []string{"one", "one", "two", "two", "three", "three"} {
		release, ok := l.acquire(login)
		if !ok {
			t.Fatal("valid slot denied")
		}
		releases = append(releases, release)
	}
	for _, login := range []string{"one", "four", ""} {
		if release, ok := l.acquire(login); ok {
			release()
			t.Fatalf("over-limit %q allowed", login)
		}
	}
	for _, release := range releases {
		release()
	}
	if l.total != 0 || len(l.active) != 0 {
		t.Fatal("released slots retained")
	}
	// Exercise simultaneous contenders, not just sequential bookkeeping.
	var wg sync.WaitGroup
	entered := make(chan bool, 20)
	leave := make(chan struct{})
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, ok := l.acquire("same")
			entered <- ok
			if ok {
				<-leave
				release()
			}
		}()
	}
	allowed := 0
	for i := 0; i < 20; i++ {
		if <-entered {
			allowed++
		}
	}
	close(leave)
	wg.Wait()
	if allowed != ebookDownloadsPerLogin {
		t.Fatalf("active same-person transfers=%d", allowed)
	}
}

func TestEbookDownloadFailureReleasesSlots(t *testing.T) {
	for _, status := range []int{0, http.StatusFound, http.StatusForbidden, http.StatusInternalServerError} {
		s, cfg, book := ebookDownloadFixture(t, nil, -1)
		s.streamClient.Transport = ebookDownloadTransport(func(*http.Request) (*http.Response, error) {
			if status == 0 {
				return nil, errors.New("simulated failure")
			}
			return &http.Response{StatusCode: status, Body: http.NoBody, Header: make(http.Header)}, nil
		})
		started, err := s.download(context.Background(), cfg, book, "member", httptest.NewRecorder())
		if started || err == nil {
			t.Fatalf("status %d: started=%v err=%v", status, started, err)
		}
		assertNoEbookDownloadSlots(t, s)
	}
}

type ebookStalledWriter struct{ *ebookDeadlineRecorder }

func (w ebookStalledWriter) Write([]byte) (int, error) {
	<-time.After(time.Until(w.deadline))
	return 0, os.ErrDeadlineExceeded
}

func TestEbookDownloadBoundsBlockedWrites(t *testing.T) {
	s, cfg, book := ebookDownloadFixture(t, bytes.Repeat([]byte{'b'}, 1024), -1)
	s.transfer.idle = 30 * time.Millisecond
	w := ebookStalledWriter{&ebookDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}}
	before := time.Now()
	started, err := s.download(context.Background(), cfg, book, "member", w)
	if !started || err == nil || time.Since(before) > time.Second {
		t.Fatalf("blocked write was not bounded: started=%v err=%v", started, err)
	}
	assertNoEbookDownloadSlots(t, s)
}

func TestEbookDownloadRequiresWriteDeadlineSupport(t *testing.T) {
	s, cfg, book := ebookDownloadFixture(t, []byte("PK\x03\x04book"), -1)
	w := httptest.NewRecorder()
	started, err := s.download(context.Background(), cfg, book, "member", w)
	if started || err == nil || w.Body.Len() != 0 {
		t.Fatal("stream started without a supported downstream write deadline")
	}
	assertNoEbookDownloadSlots(t, s)
}

func TestEbookDownloadLargeSupportedFormats(t *testing.T) {
	// Multiple copy buffers, without allocating a multi-gigabyte test file.
	data := bytes.Repeat([]byte{'b'}, 4<<20)
	for _, format := range []string{"epub", "pdf", "cbr", "cbz", "azw3", "mobi"} {
		t.Run(format, func(t *testing.T) {
			s, cfg, book := ebookDownloadFixture(t, data, int64(len(data)))
			s.transfer.maxBytes = int64(len(data))
			book.Format = format
			w := &ebookDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			started, err := s.download(context.Background(), cfg, book, "member", w)
			if !started || err != nil || !bytes.Equal(data, w.Body.Bytes()) || w.Header().Get("Content-Type") != ebookMIME(format) {
				t.Fatalf("large %s transfer failed: %v", format, err)
			}
			assertNoEbookDownloadSlots(t, s)
		})
	}
}

func TestEbookDownloadHTTPConnectionCanBeReused(t *testing.T) {
	s, cfg, book := ebookDownloadFixture(t, []byte("PK\x03\x04book"), -1)
	s.transfer.idle = 50 * time.Millisecond
	h := ebookDownloadAPI(t, s, cfg, book)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/ebooks/{id}/download", h.ebookDownload)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	srv := httptest.NewServer(securityHeaders(mux))
	defer srv.Close()
	client := srv.Client()
	req, err := http.NewRequest("GET", srv.URL+"/api/ebooks/"+book.ID+"/download", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Tailscale-User-Login", "mom@example.com")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(data) != "PK\x03\x04book" {
		t.Fatalf("download: %q %v", data, err)
	}
	<-time.After(2 * s.transfer.idle)
	var reused atomic.Bool
	var connections atomic.Int32
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		connections.Add(1)
		reused.Store(info.Reused)
	}}
	req, err = http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), "GET", srv.URL+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(resp.Body)
	if err != nil || string(data) != "ok" {
		t.Fatalf("subsequent response: %q %v", data, err)
	}
	if !reused.Load() || connections.Load() != 1 {
		t.Fatal("subsequent request did not succeed on the original connection")
	}
	assertNoEbookDownloadSlots(t, s)
}

func ebookDownloadAPI(t *testing.T, s *ebookService, cfg *EbooksConfig, book *ebook) *handler {
	t.Helper()
	c, err := parseConfig([]byte(apiConfig))
	if err != nil {
		t.Fatal(err)
	}
	c.Ebooks = cfg
	c.Ebooks.App = "seerr"
	s.snapshot = &ebookSnapshot{Rows: []ebook{*book}, At: time.Now(), sig: cfg.sig(s.key)}
	return &handler{store: &ConfigStore{cfg: c}, ebooks: s, bookFiles: newRateLimiter(time.Second, 1200)}
}

func TestEbookDownloadAPIRejectsBusyAndUnauthorized(t *testing.T) {
	for _, login := range []string{"mom@example.com", "dad@example.com", "", "unknown@example.com"} {
		t.Run(login, func(t *testing.T) {
			s, cfg, book := ebookDownloadFixture(t, []byte("PK\x03\x04book"), -1)
			h := ebookDownloadAPI(t, s, cfg, book)
			for i := 0; i < ebookDownloadsPerLogin; i++ {
				release, ok := s.downloads.acquire("mom@example.com")
				if !ok {
					t.Fatal("slot setup failed")
				}
				defer release()
			}
			var calls atomic.Int32
			s.streamClient.Transport = ebookDownloadTransport(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, errors.New("unexpected fetch") })
			r := httptest.NewRequest("GET", "/api/ebooks/"+book.ID+"/download", nil)
			r.RemoteAddr = loopback
			r.Header.Set("Tailscale-User-Login", login)
			r.SetPathValue("id", book.ID)
			w := httptest.NewRecorder()
			h.ebookDownload(w, r)
			want := http.StatusNotFound
			if login == "mom@example.com" {
				want = http.StatusTooManyRequests
			}
			if w.Code != want || calls.Load() != 0 {
				t.Fatalf("status=%d want=%d fetches=%d", w.Code, want, calls.Load())
			}
		})
	}
}

func TestEbookDownloadAPIAbortsPartialUnknownLength(t *testing.T) {
	s, cfg, book := ebookDownloadFixture(t, bytes.Repeat([]byte{'b'}, 2048), -1)
	s.transfer.maxBytes = 600
	h := ebookDownloadAPI(t, s, cfg, book)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/ebooks/{id}/download", h.ebookDownload)
	srv := httptest.NewServer(securityHeaders(mux))
	defer srv.Close()
	req, err := http.NewRequest("GET", srv.URL+"/api/ebooks/"+book.ID+"/download", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Tailscale-User-Login", "mom@example.com")
	resp, err := srv.Client().Do(req)
	if err == nil {
		defer resp.Body.Close()
		if _, err := io.ReadAll(resp.Body); err == nil {
			t.Fatal("partial unknown-length transfer looked complete to HTTP client")
		}
	}
	assertNoEbookDownloadSlots(t, s)
}
