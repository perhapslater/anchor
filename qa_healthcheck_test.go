package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHealthcheckEndpointAddressFamilies(t *testing.T) {
	for listen, want := range map[string]string{
		defaultListen:                     "http://127.0.0.1:8080/healthz",
		"127.0.0.2:8081":                  "http://127.0.0.2:8081/healthz",
		"[::1]:8082":                      "http://[::1]:8082/healthz",
		"0.0.0.0:8083":                    "http://127.0.0.1:8083/healthz",
		"[::]:8084":                       "http://[::1]:8084/healthz",
		":8085":                           "http://127.0.0.1:8085/healthz",
		", 127.0.0.2:8086,127.0.0.1:8086": "http://127.0.0.2:8086/healthz",
		"configured-host.invalid:8087":    "http://configured-host.invalid:8087/healthz",
	} {
		endpoint, err := healthcheckURL(listen)
		if err != nil || endpoint != want {
			t.Errorf("healthcheck endpoint for %q: %q, %v; want %q", listen, endpoint, err, want)
		}
	}
	for _, listen := range []string{"", ", ,", "not-an-address", "::1"} {
		if _, err := healthcheckURL(listen); err == nil {
			t.Errorf("malformed listener accepted: %q", listen)
		}
	}
}

func TestHealthcheckCLIUsesConfiguredAddress(t *testing.T) {
	if os.Getenv("ANCHOR_QA_HEALTHCHECK_CHILD") == "1" {
		os.Args = []string{os.Args[0], "healthcheck"}
		main()
		return
	}
	for _, target := range []struct{ network, address string }{{"tcp4", "127.0.0.2:0"}, {"tcp6", "[::1]:0"}} {
		t.Run(target.address, func(t *testing.T) {
			listener, err := net.Listen(target.network, target.address)
			if err != nil {
				t.Skipf("loopback family unavailable: %v", err)
			}
			_, port, _ := net.SplitHostPort(listener.Addr().String())
			// Own the incorrect IPv4 destination too, so a buggy healthcheck
			// cannot accidentally contact an unrelated local service.
			wrongListener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", port))
			if err != nil {
				listener.Close()
				t.Skip("could not reserve both isolated loopback destinations")
			}
			var reached atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/healthz" {
					t.Errorf("unexpected healthcheck path: %s", r.URL.Path)
				}
				reached.Add(1)
				w.Write([]byte("ok"))
			}))
			server.Listener = listener
			server.Start()
			defer server.Close()
			wrong := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			wrong.Listener = wrongListener
			wrong.Start()
			defer wrong.Close()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, executable, "-test.run=^TestHealthcheckCLIUsesConfiguredAddress$")
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(strings.ToUpper(entry), "ANCHOR_") {
					command.Env = append(command.Env, entry)
				}
			}
			command.Env = append(command.Env, "ANCHOR_QA_HEALTHCHECK_CHILD=1", "ANCHOR_LISTEN="+listener.Addr().String())
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("healthcheck failed for its live configured listener: %v; %s", err, output)
			}
			if reached.Load() != 1 {
				t.Fatalf("healthcheck reached configured endpoint %d times", reached.Load())
			}
		})
	}
}
