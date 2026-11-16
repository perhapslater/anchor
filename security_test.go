package main

import (
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/fstest"
)

// countingTransport records outbound requests instead of making them.
type countingTransport struct{ n atomic.Int32 }

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.n.Add(1)
	return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody}, nil
}

func TestIconsOnlyServesConfiguredNames(t *testing.T) {
	bundled := fstest.MapFS{"immich.svg": {Data: []byte("<svg/>")}}
	allowed := map[string]bool{"immich": true, "not-bundled": true}
	s := NewIconServer(t.TempDir(), fs.FS(bundled), func(n string) bool { return allowed[n] })
	tr := &countingTransport{}
	s.client.Transport = tr

	mux := http.NewServeMux()
	mux.Handle("GET /icons/{name}", s)
	get := func(name string) int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/icons/"+name, nil))
		return rec.Code
	}

	if code := get("immich"); code != 200 {
		t.Fatalf("configured bundled icon: got %d", code)
	}
	for _, name := range []string{"zz-random-1", "zz-random-2", "jellyfin", "a..b"} {
		if code := get(name); code != 404 {
			t.Errorf("%s: got %d, want 404", name, code)
		}
	}
	if n := tr.n.Load(); n != 0 {
		t.Fatalf("names not in anchor.yaml caused %d outbound requests", n)
	}
	// A configured name that isn't bundled may still be fetched (once).
	get("not-bundled")
	if tr.n.Load() == 0 {
		t.Fatal("configured icon should be looked up online")
	}
}

func TestFunnelTrafficIsRefused(t *testing.T) {
	h := blockFunnel(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	for _, path := range []string{"/", "/api/me", "/icons/immich", "/healthz"} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Tailscale-Funnel-Request", "?1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s via Funnel: got %d, want 403", path, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 {
		t.Fatalf("normal tailnet request blocked: %d", rec.Code)
	}
}

func TestNoFolderListings(t *testing.T) {
	web := fstest.MapFS{
		"index.html":       {Data: []byte("<!doctype html>")},
		"icons/immich.svg": {Data: []byte("<svg/>")},
		"brand/anchor.svg": {Data: []byte("<svg/>")},
	}
	h := staticHandler(web)
	for path, want := range map[string]int{"/": 200, "/icons/": 404, "/brand/": 404, "/brand/anchor.svg": 200} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != want {
			t.Errorf("%s: got %d, want %d", path, rec.Code, want)
		}
	}
}

func TestListensOnLoopbackOnly(t *testing.T) {
	lns, err := listenAll("127.0.0.1:0,[::1]:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, l := range lns {
			l.Close()
		}
	}()
	for _, l := range lns {
		ip := l.Addr().(*net.TCPAddr).IP
		if !ip.IsLoopback() {
			t.Errorf("listening on non-loopback address %s", l.Addr())
		}
	}
	// The default must be loopback-only too.
	for _, a := range []string{"127.0.0.1:8080", "[::1]:8080"} {
		host, _, _ := net.SplitHostPort(a)
		if !net.ParseIP(host).IsLoopback() {
			t.Errorf("default %s is not loopback", a)
		}
	}
	if defaultListen != "127.0.0.1:8080,[::1]:8080" {
		t.Errorf("defaultListen changed: %q", defaultListen)
	}
}

func TestMissingIPv6IsNotFatal(t *testing.T) {
	// Simulate "can't bind IPv6" by taking the port first. Anchor must
	// still start on IPv4 loopback.
	busy, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback here; the real case is covered by this skip path")
	}
	defer busy.Close()
	lns, err := listenAll("127.0.0.1:0," + busy.Addr().String())
	if err != nil {
		t.Fatalf("IPv6 failure should be skipped: %v", err)
	}
	if len(lns) != 1 {
		t.Fatalf("want 1 listener, got %d", len(lns))
	}
	lns[0].Close()
}
