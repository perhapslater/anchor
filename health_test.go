package main

import (
	"crypto/tls"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// Each check must use a new connection, so a connection that died silently
// on the tailnet can never be reused and cause a false "Not responding".
func TestChecksUseFreshConnections(t *testing.T) {
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login", http.StatusFound)
	}))
	srv.EnableHTTP2 = true
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.StartTLS()
	defer srv.Close()

	c := NewChecker(nil)
	tr := c.client.Transport.(*http.Transport)
	tr.TLSClientConfig.RootCAs = srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs

	app := App{ID: "abs", URL: srv.URL + "/"}
	for i := 0; i < 3; i++ {
		c.checkOne(app)
		if st := c.Get("abs"); st.State != "up" {
			t.Fatalf("check %d: got %q (%s), want up", i, st.State, st.Detail)
		}
	}
	if n := conns.Load(); n != 3 {
		t.Fatalf("expected 3 separate connections, got %d", n)
	}
}

func TestDownNeedsTwoFailures(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listening now: connection refused

	c := NewChecker(nil)
	app := App{ID: "x", URL: "http://" + addr + "/"}
	c.checkOne(app)
	if st := c.Get("x"); st.State == "down" {
		t.Fatal("one failed round should not show red")
	}
	c.checkOne(app)
	if st := c.Get("x"); st.State != "down" {
		t.Fatalf("two failed rounds should show red, got %q", st.State)
	}
}

// Checks and notifications must only ever offer HTTP/1.1, so a server that
// supports HTTP/2 (like Tailscale Serve) can't switch to it. This broke once
// after a Go upgrade: all apps showed "malformed HTTP response \x00\x00...".
func TestOnlyHTTP1IsOffered(t *testing.T) {
	var offered []string
	var proto string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proto = r.Proto
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	srv.TLS.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		offered = hello.SupportedProtos
		return nil, nil
	}

	for name, tr := range map[string]*http.Transport{
		"status checks": NewChecker(nil).client.Transport.(*http.Transport),
		"ntfy":          newNtfyNotifier(nil, "").client.Transport.(*http.Transport),
	} {
		if name == "ntfy" {
			// Keep the production TLS/HTTP protocol settings while permitting
			// this test's owned loopback TLS server at the socket boundary.
			tr.DialContext = newUpstreamClient(true).Transport.(*http.Transport).DialContext
		}
		tr.TLSClientConfig.RootCAs = srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
		resp, err := (&http.Client{Transport: tr}).Get(srv.URL)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		resp.Body.Close()
		if len(offered) == 0 {
			t.Fatalf("%s: test didn't capture the TLS hello", name)
		}
		if proto != "HTTP/1.1" {
			t.Errorf("%s: server saw %s, want HTTP/1.1", name, proto)
		}
		for _, p := range offered {
			if p == "h2" {
				t.Errorf("%s: client offered h2 (offered %v)", name, offered)
			}
		}
	}
}
