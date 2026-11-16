package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGuardAddr(t *testing.T) {
	refused := []string{
		"127.0.0.1:80", "[::1]:80", // Anchor itself / this container
		"169.254.169.254:80", "[fe80::1]:80", // cloud metadata, link-local
		"[::ffff:169.254.169.254]:80", // the same, written as IPv6
		"0.0.0.0:80", "[::]:80",
		"224.0.0.1:80", "[ff02::1]:80", "255.255.255.255:80",
		"not-an-ip:80", "",
	}
	allowed := []string{
		"192.168.1.5:80", "10.0.0.1:80", "172.17.0.2:80", // LAN, Docker
		"100.101.102.103:80", "[fd7a:115c:a1e0::1]:80", // tailnet
	}
	strict := guardAddr(false)
	for _, a := range refused {
		if err := strict("tcp", a, nil); err == nil {
			t.Errorf("%q should be refused", a)
		}
	}
	for _, a := range allowed {
		if err := strict("tcp", a, nil); err != nil {
			t.Errorf("%q should be allowed: %v", a, err)
		}
	}
	// Development mode allows loopback and nothing else new.
	dev := guardAddr(true)
	for _, a := range []string{"127.0.0.1:80", "[::1]:80"} {
		if err := dev("tcp", a, nil); err != nil {
			t.Errorf("%q should be allowed in dev mode: %v", a, err)
		}
	}
	for _, a := range []string{"169.254.169.254:80", "0.0.0.0:80", "224.0.0.1:80"} {
		if err := dev("tcp", a, nil); err == nil {
			t.Errorf("%q should still be refused in dev mode", a)
		}
	}
}

func TestClientRefusesLoopbackUnlessDev(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	u, err := newUpstream("Test", srv.URL, "X-Api-Key", "k", newUpstreamClient(false), 1<<10, `^/ok$`)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := u.getJSON(context.Background(), "/ok", nil, &out); err == nil {
		t.Fatal("loopback destination was reached in production mode")
	}
	if hits.Load() != 0 {
		t.Fatalf("server saw %d requests", hits.Load())
	}
	u, _ = newUpstream("Test", srv.URL, "X-Api-Key", "k", newUpstreamClient(true), 1<<10, `^/ok$`)
	if err := u.getJSON(context.Background(), "/ok", nil, &out); err != nil {
		t.Fatalf("dev mode should reach loopback: %v", err)
	}
}

func TestClientIgnoresProxySettings(t *testing.T) {
	tr := newUpstreamClient(false).Transport.(*http.Transport)
	if tr.Proxy != nil {
		t.Fatal("an environment proxy could redirect where the keys go")
	}
}

func devUpstream(t *testing.T, base string, maxBody int64, paths ...string) *upstream {
	t.Helper()
	u, err := newUpstream("Test", base, "X-Api-Key", "sekrit", newUpstreamClient(true), maxBody, paths...)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestGetJSONSendsKeyInHeaderOnly(t *testing.T) {
	var method, header, accept, rawQuery, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, header, accept, rawQuery, path = r.Method, r.Header.Get("X-Api-Key"), r.Header.Get("Accept"), r.URL.RawQuery, r.URL.Path
		w.Write([]byte(`{"n": 7}`))
	}))
	defer srv.Close()
	u := devUpstream(t, srv.URL+"/prefix", 1<<10, `^/api/v1/movie/[0-9]{1,9}$`)
	var out struct{ N int }
	q := url.Values{"take": {"5"}}
	if err := u.getJSON(context.Background(), "/api/v1/movie/42", q, &out); err != nil {
		t.Fatal(err)
	}
	if out.N != 7 {
		t.Fatalf("decoded %+v", out)
	}
	if method != "GET" || header != "sekrit" || accept != "application/json" {
		t.Errorf("method %q key %q accept %q", method, header, accept)
	}
	if path != "/prefix/api/v1/movie/42" || rawQuery != "take=5" {
		t.Errorf("path %q query %q (the base path should be kept)", path, rawQuery)
	}
	if strings.Contains(rawQuery, "sekrit") || strings.Contains(path, "sekrit") {
		t.Error("the key reached the URL")
	}
}

func TestGetJSONRefusesPathsOutsideTheList(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	u := devUpstream(t, srv.URL, 1<<10, `^/api/v1/request$`, `^/api/v1/movie/[0-9]{1,9}$`)
	var out map[string]any
	for _, p := range []string{
		"/api/v1/settings", "/api/v1/request/", "/api/v1/movie/abc", "/api/v1/movie/1/../../settings",
		"/api/v1/movie/1?x=1", "/api/v1/movie/1#f", "//evil.test/api/v1/request", "/api/v1//request",
		"/api/v1/movie/%2e%2e", "/api/v1/movie/1\\..", "api/v1/request", "",
	} {
		if err := u.getJSON(context.Background(), p, nil, &out); err == nil {
			t.Errorf("path %q should be refused", p)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("refused paths still made %d requests", hits.Load())
	}
	if err := u.getJSON(context.Background(), "/api/v1/request", nil, &out); err != nil {
		t.Fatalf("listed path refused: %v", err)
	}
}

func TestGetJSONRefusesAnOtherHost(t *testing.T) {
	u := devUpstream(t, "http://192.168.1.10:5055", 1<<10, `^/x$`)
	for _, target := range []string{
		"http://192.168.1.11:5055/x", "http://192.168.1.10:5056/x", "https://192.168.1.10:5055/x",
		"http://192.168.1.10:5055@evil.test/x", "http://evil.test/x",
	} {
		parsed, err := url.Parse(target)
		if err != nil {
			t.Fatal(err)
		}
		if u.sameHost(parsed) {
			t.Errorf("%s should not count as the configured host", target)
		}
	}
	ok, _ := url.Parse("http://192.168.1.10:5055/x")
	if !u.sameHost(ok) {
		t.Error("the configured host should match")
	}
}

func TestRedirectsAreNeverFollowed(t *testing.T) {
	var elsewhere atomic.Int32
	var leaked atomic.Value
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		leaked.Store(r.Header.Get("X-Api-Key"))
		w.Write([]byte(`{}`))
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	}))
	defer srv.Close()

	u := devUpstream(t, srv.URL, 1<<10, `^/x$`)
	var out map[string]any
	err := u.getJSON(context.Background(), "/x", nil, &out)
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("a redirect should be an error, got %v", err)
	}
	if elsewhere.Load() != 0 {
		t.Fatalf("redirect was followed; the key reached another server (%v)", leaked.Load())
	}
}

func TestErrorsNeverContainTheKeyOrQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key sekrit in the body", http.StatusUnauthorized)
	}))
	u := devUpstream(t, srv.URL, 1<<10, `^/x$`)
	var out map[string]any
	q := url.Values{"token": {"hunter2"}}

	err := u.getJSON(context.Background(), "/x", q, &out)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want the status in the error, got %v", err)
	}
	for _, bad := range []string{"sekrit", "hunter2", "token="} {
		if strings.Contains(err.Error(), bad) {
			t.Errorf("error %q leaks %q", err, bad)
		}
	}

	srv.Close() // now nothing is listening
	err = u.getJSON(context.Background(), "/x", q, &out)
	if err == nil {
		t.Fatal("expected a connection error")
	}
	for _, bad := range []string{"sekrit", "hunter2", "token="} {
		if strings.Contains(err.Error(), bad) {
			t.Errorf("connection error %q leaks %q", err, bad)
		}
	}
}

func TestGetJSONLimitsAndValidatesTheBody(t *testing.T) {
	big := `{"a":"` + strings.Repeat("x", 500) + `"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/big":
			w.Write([]byte(big))
		case "/junk":
			w.Write([]byte(`<html>not json</html>`))
		default:
			w.Write([]byte(`{"a":"ok"}`))
		}
	}))
	defer srv.Close()
	u := devUpstream(t, srv.URL, 100, `^/(big|junk|small)$`)
	var out map[string]string

	if err := u.getJSON(context.Background(), "/big", nil, &out); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("oversized body: got %v", err)
	}
	if err := u.getJSON(context.Background(), "/junk", nil, &out); err == nil {
		t.Error("non-JSON body should be an error")
	}
	if err := u.getJSON(context.Background(), "/small", nil, &out); err != nil || out["a"] != "ok" {
		t.Errorf("small body: %v %v", err, out)
	}
}

func TestGetJSONHonoursTheCallersDeadline(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	u := devUpstream(t, srv.URL, 1<<10, `^/x$`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out map[string]any
	if err := u.getJSON(ctx, "/x", nil, &out); err == nil {
		t.Fatal("a cancelled context should stop the call")
	}
}

func TestNewUpstreamRejectsBadAddressesAndMissingKeys(t *testing.T) {
	c := newUpstreamClient(false)
	for _, base := range []string{"", "ftp://x:1", "http://", "http://u:p@h:1", "http://h:1/?a=b", "no scheme"} {
		if _, err := newUpstream("T", base, "X-Api-Key", "k", c, 10, `^/x$`); err == nil {
			t.Errorf("address %q should be rejected", base)
		}
	}
	if _, err := newUpstream("T", "http://h:1", "X-Api-Key", "", c, 10, `^/x$`); err == nil {
		t.Error("an empty key should be rejected")
	}
	if _, err := newUpstream("T", "http://h:1", "X-Api-Key", "k", c, 10, `^/x(`); err == nil {
		t.Error("a bad path pattern should be rejected")
	}
}
