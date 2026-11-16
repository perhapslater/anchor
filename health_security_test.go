package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestHealthTransportIgnoresProxy(t *testing.T) {
	tr := NewChecker(nil).client.Transport.(*http.Transport)
	if tr.Proxy != nil {
		t.Error("health transport inherits environment proxy")
	}
	if tr.DialContext == nil {
		t.Fatal("missing dial function")
	}
}
func TestValidationHealthIgnoresEnvironmentProxy(t *testing.T) {
	var hits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("https_proxy", proxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	NewChecker(nil).probeHTTP(App{ID: "audit", URL: "http://health-audit.invalid/"})
	if hits.Load() != 0 {
		t.Fatalf("health request reached owned non-forwarding proxy: %d", hits.Load())
	}
}
func TestValidationHealthOwnedLoopbackCompatible(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer server.Close()
	if result := NewChecker(nil).probeHTTP(App{URL: server.URL}); result.state != "up" {
		t.Fatalf("loopback health: %+v", result)
	}
}

func TestHealthRejectsUnspecifiedBeforeLoopbackRewrite(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(204) }))
	defer server.Close()
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	result := NewChecker(nil).probeHTTP(App{URL: "http://0.0.0.0:" + port})
	if result.state != "down" || result.detail != "address not allowed" || hits.Load() != 0 {
		t.Fatalf("unspecified health target not rejected: %+v hits=%d", result, hits.Load())
	}
}
