package main

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Opt-in, loopback-only browser fixture. No environment keys, user config,
// upstream requests, notifications or actual ebook downloads are used.
func TestBrowserFixture(t *testing.T) {
	if os.Getenv("ANCHOR_BROWSER_TEST") != "1" {
		t.Skip("opt-in local browser fixture")
	}
	h := newTestHandler(t)
	h.devUser = "admin@example.com"
	cfg, _ := h.store.Get()
	cfg.Title = "Anchor local validation"
	cfg.Apps = []App{
		{ID: "ebooks", Name: "Ebooks", URL: "https://example.invalid/", Icon: "book", Access: []string{"admin"}, Check: "off"},
		{ID: "listen", Name: "Listen", URL: "https://example.invalid/", Icon: "book", Access: []string{"family"}, Check: "off", Mobile: &Mobile{IOSApp: "example://", IOS: "https://example.invalid/store", Server: "https://example.invalid/"}},
	}
	cfg.Ebooks = &EbooksConfig{App: "ebooks", Server: "https://example.invalid", LibraryID: "fixture"}
	h.ebooks = newEbookService("dummy-fixture-key", false)
	// Even an expired/forced refresh cannot make a network call.
	h.ebooks.readClient.Transport = ebookDownloadTransport(func(*http.Request) (*http.Response, error) { return nil, fmt.Errorf("fixture has no upstream") })
	h.booksReads = newRateLimiter(time.Second, 3600)
	rows := make([]ebook, 20000)
	for i := range rows {
		rows[i] = ebook{ID: fmt.Sprintf("book-%d", i), Title: fmt.Sprintf("Book %05d", i), Author: "Fixture Author", Format: "EPUB"}
	}
	h.ebooks.snapshot = &ebookSnapshot{Rows: rows, At: time.Now(), sig: cfg.Ebooks.sig(h.ebooks.key)}
	web, _ := fs.Sub(webFiles, "web")
	static := staticHandler(web)
	// Measure actual DOM rendering in the browser, without production telemetry.
	script, _ := fs.ReadFile(web, "app.js")
	js := strings.Replace(string(script), "function renderEbooks() {", "function renderEbooks() { const renderStart = performance.now();", 1)
	js = strings.Replace(js, `$("ebooks-refresh").disabled = ebookBusy;`, `$("ebooks-refresh").disabled = ebookBusy; $("ebooks-list").dataset.renderMs = String(performance.now() - renderStart);`, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/me", h.me)
	mux.HandleFunc("GET /api/events", h.events)
	mux.HandleFunc("GET /api/ebooks", h.ebooksList)
	mux.HandleFunc("POST /api/ebooks/refresh", h.ebooksList)
	mux.HandleFunc("GET /api/ebooks/{id}/cover", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	mux.HandleFunc("GET /api/ebooks/{id}/download", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	mux.HandleFunc("GET /app.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Cache-Control", "no-cache")
		fmt.Fprint(w, js)
	})
	mux.Handle("GET /icons/{name}", NewIconServer(t.TempDir(), web, func(string) bool { return false }))
	mux.Handle("GET /", static)
	listen := env("ANCHOR_BROWSER_LISTEN", "127.0.0.1:18081")
	host, _, err := net.SplitHostPort(listen)
	if err != nil || host != "127.0.0.1" {
		t.Fatal("browser fixture must listen on IPv4 loopback")
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	h.stopping = ctx.Done()
	fmt.Printf("Disposable browser fixture: http://%s (10 minute lifetime)\n", ln.Addr())
	if err := serveListenersContext(ctx, newHTTPServer(securityHeaders(mux)), []net.Listener{ln}, time.Second); err != nil {
		t.Fatal(err)
	}
}
