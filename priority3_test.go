package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestCompleteOrigin(t *testing.T) {
	for _, tc := range []struct {
		name, target, origin, scheme string
		want                         bool
	}{
		{"TLS", "https://anchor.test/api/report", "https://anchor.test", "", true},
		{"HTTP development", "http://localhost:8080/api/report", "http://localhost:8080", "http", true},
		{"Serve HTTPS", "http://anchor.test/api/report", "https://anchor.test", "https", true},
		{"Serve default port", "http://ANCHOR.test/api/report", "https://anchor.test:443", "https", true},
		{"HTTP default port", "http://localhost/api/report", "http://localhost:80", "http", true},
		{"wrong scheme", "http://anchor.test/api/report", "http://anchor.test", "https", false},
		{"TLS downgrade", "https://anchor.test/api/report", "http://anchor.test", "", false},
		{"wrong port", "http://anchor.test/api/report", "https://anchor.test:444", "https", false},
		{"wrong host", "http://anchor.test/api/report", "https://other.test", "https", false},
		{"credentials", "http://anchor.test/api/report", "https://x@anchor.test", "https", false},
		{"path", "http://anchor.test/api/report", "https://anchor.test/", "https", false},
		{"query", "http://anchor.test/api/report", "https://anchor.test?", "https", false},
		{"fragment", "http://anchor.test/api/report", "https://anchor.test#", "https", false},
		{"empty port", "http://anchor.test/api/report", "https://anchor.test:", "https", false},
		{"opaque", "http://anchor.test/api/report", "https:anchor.test", "https", false},
		{"null", "http://anchor.test/api/report", "null", "https", false},
		{"multiple", "http://anchor.test/api/report", "https://anchor.test https://evil.test", "https", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", tc.target, nil)
			r.Header.Set("Origin", tc.origin)
			// A visitor cannot change the configured external scheme.
			r.Header.Set("X-Forwarded-Proto", "http")
			r.Header.Set("Forwarded", "proto=http;host=evil.test")
			h := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				if got := sameOrigin(r); got != tc.want {
					t.Fatalf("sameOrigin=%v, want %v", got, tc.want)
				}
			})
			if tc.scheme != "" {
				externalScheme(tc.scheme, h).ServeHTTP(httptest.NewRecorder(), r)
			} else {
				h.ServeHTTP(httptest.NewRecorder(), r)
			}
		})
	}
}

func TestConfigRemainingValidation(t *testing.T) {
	for _, config := range []string{
		"title: One\n---\ntitle: Two\n", "title: One\n---\n", "notice: {message: hi, acces: [family]}",
		"notice: true", "notice: [hello]",
		"apps: [{name: Bad, url: 'javascript:alert(1)', access: [family]}]",
		"apps: [{name: Bad, url: 'https://user:pass@host/', access: [family]}]",
		"apps: [{name: Bad, url: '/relative', access: [family]}]",
		"apps: [{name: '!!!', url: 'https://host/', access: [family]}]",
		"apps: [{id: '../bad', name: Bad, url: 'https://host/', access: [family]}]",
		"apps: [{name: Bad, url: 'https://host/', check: 'file:///tmp/x', access: [family]}]",
		"apps: [{name: Bad, url: 'https://host/', access: [family], mobile: {ios: 'javascript:alert(1)'}}]",
		"apps: [{name: Bad, url: 'https://host/', access: [family], mobile: {ios_app: 'data:text/html,x'}}]",
		"apps: [{name: Bad, url: 'https://host/', access: [family], mobile: {server: 'file:///tmp/x'}}]",
		"apps: [{name: Bad, url: 'https://host/', access: [family], icon: '../hidden'}]",
	} {
		if _, err := parseConfig([]byte(config)); err == nil {
			t.Errorf("accepted malformed config: %s", config)
		}
	}
	for _, target := range []string{"absorb://", "intent://open#Intent;scheme=example;package=example.app;end", "https://app.test/open?q=x#section"} {
		if !validAppLaunch(target) {
			t.Errorf("refused valid app link %q", target)
		}
	}
	if _, err := parseConfig(exampleConfig); err != nil {
		t.Fatal(err)
	}
	// Deep nested normal structs still get KnownFields validation.
	if _, err := parseConfig([]byte("apps: [{name: App, url: 'https://host/', access: [family], guide: {stepps: [x]}}]")); err == nil {
		t.Fatal("unknown guide field accepted")
	}
}

func TestEmbeddedShellVersion(t *testing.T) {
	web, _ := fs.Sub(webFiles, "web")
	read := func(web fs.FS) string {
		r := httptest.NewRecorder()
		staticHandler(web).ServeHTTP(r, httptest.NewRequest("GET", "/sw.js", nil))
		if r.Code != 200 || r.Header().Get("Cache-Control") != "no-cache" {
			t.Fatal("worker must revalidate")
		}
		return r.Body.String()
	}
	first := read(web)
	if strings.Contains(first, "__ANCHOR_SHELL_VERSION__") {
		t.Fatal("unexpanded worker cache identity")
	}
	if first != read(web) {
		t.Fatal("unstable cache identity")
	}
	files := fstest.MapFS{}
	for _, name := range []string{"index.html", "style.css", "app.js", "manifest.webmanifest", "brand/anchor.svg", "sw.js"} {
		body, _ := fs.ReadFile(web, name)
		files[name] = &fstest.MapFile{Data: body}
	}
	files["app.js"].Data = append(files["app.js"].Data, []byte("\n// new release")...)
	if first == read(files) {
		t.Fatal("changed shell reused old cache identity")
	}
}

func TestMeScopeIncludesEffectiveIdentity(t *testing.T) {
	h := newTestHandler(t)
	cfg, _ := h.store.Get()
	p := cfg.People["wife@example.com"]
	p.Name = "Same name"
	cfg.People["wife@example.com"] = p
	p = cfg.People["mom@example.com"]
	p.Name = "Same name"
	cfg.People["mom@example.com"] = p
	a := get(t, h, loopback, "admin@example.com", "?as=wife@example.com")
	b := get(t, h, loopback, "admin@example.com", "?as=mom@example.com")
	if a.ViewingAs != b.ViewingAs || a.Scope == b.Scope || a.Scope == "" {
		t.Fatal("scope must distinguish previews whose names match")
	}
	if a.Scope != get(t, h, loopback, "admin@example.com", "?as=wife@example.com").Scope {
		t.Fatal("unchanged scope is unstable")
	}
}

func TestServerDrainsAndBoundsShutdown(t *testing.T) {
	for _, finish := range []bool{true, false} {
		t.Run(map[bool]string{true: "completes", false: "stalled"}[finish], func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started, release, handlerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
			srv := newHTTPServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(handlerDone)
				close(started)
				select {
				case <-release:
					io.WriteString(w, "complete")
				case <-r.Context().Done():
				}
			}))
			done := make(chan error, 1)
			go func() { done <- serveListenersContext(ctx, srv, []net.Listener{ln}, 200*time.Millisecond) }()
			response := make(chan string, 1)
			go func() {
				client := &http.Client{Timeout: 2 * time.Second}
				r, err := client.Get("http://" + ln.Addr().String())
				if err != nil {
					response <- ""
					return
				}
				defer r.Body.Close()
				b, _ := io.ReadAll(r.Body)
				response <- string(b)
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("request did not start")
			}
			cancel()
			if finish {
				close(release)
			}
			select {
			case err := <-done:
				if finish && err != nil {
					t.Fatal(err)
				}
				if !finish && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("stalled shutdown: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("shutdown hung")
			}
			if body := <-response; finish && body != "complete" {
				t.Fatalf("drain truncated response: %q", body)
			}
			select {
			case <-handlerDone:
			case <-time.After(time.Second):
				t.Fatal("handler not canceled")
			}
			if conn, err := net.DialTimeout("tcp", ln.Addr().String(), 100*time.Millisecond); err == nil {
				conn.Close()
				t.Fatal("listener still accepts after shutdown")
			}
		})
	}
}

func TestLifecycleCancellation(t *testing.T) {
	h := newTestHandler(t)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	h.checker.probe = func(App) probeResult {
		select {
		case <-started:
		default:
			close(started)
		}
		return probeResult{skipped: true}
	}
	// The fixture has several apps checked concurrently; use one for this probe.
	cfg, _ := h.store.Get()
	cfg.Apps = cfg.Apps[:1]
	done := make(chan struct{})
	go func() { h.checker.RunContext(ctx); close(done) }()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("health loop ignored cancellation")
	}
	if waitContext(ctx, time.Hour) {
		t.Fatal("canceled retry slept")
	}
	called := make(chan bool, 1)
	sendAsyncContext(ctx, &fakeLifecycleNotifier{}, Notification{}, func(ok bool) { called <- ok })
	select {
	case ok := <-called:
		if ok {
			t.Fatal("canceled alert sent")
		}
	case <-time.After(time.Second):
		t.Fatal("alert callback never completed")
	}
}

type fakeLifecycleNotifier struct{}

func (*fakeLifecycleNotifier) Send(context.Context, Notification) error {
	return errors.New("unexpected call")
}

func TestShutdownClosesEventStreams(t *testing.T) {
	h := newTestHandler(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.stopping = ctx.Done()
	w := &ebookDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() { h.events(w, httptest.NewRequest("GET", "/api/events", nil)); close(done) }()
	waitSubscribers(t, h.checker.changes, 1)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SSE prevented shutdown")
	}
	waitSubscribers(t, h.checker.changes, 0)
}

func TestShutdownCancelsHealthProbeAndSharedWork(t *testing.T) {
	h := newTestHandler(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.checker.ctx = ctx
	started := make(chan struct{})
	h.checker.client.Transport = ebookDownloadTransport(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	done := make(chan struct{})
	go func() { h.checker.probeHTTP(App{URL: "https://example.invalid/"}); close(done) }()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("probe/retry ignored cancellation")
	}

	rs := newRequestStatus(h.store, requestKeys{}, false)
	rs.ctx = ctx
	rs.build = func(work context.Context, _ *Config) (*snapshot, error) {
		if work.Err() == nil {
			t.Error("shared work outlived its server context")
		}
		return nil, work.Err()
	}
	finished := &requestCall{done: make(chan struct{}), sig: "fixture"}
	cfg, _ := h.store.Get()
	rs.run(cfg, "fixture", finished)
	select {
	case <-finished.done:
	default:
		t.Fatal("refresh did not release waiters")
	}

	cache := newPosterCache()
	cache.ctx = ctx
	var gotContext context.Context
	call := &posterCall{done: make(chan struct{})}
	cache.run(posterKey{sig: "fixture"}, call, func(work context.Context) ([]byte, string, error) { gotContext = work; return nil, "", work.Err() })
	// A canceled cache can skip the fetch entirely while waiting for a slot.
	if gotContext != nil && gotContext.Err() == nil {
		t.Fatal("image fetch outlived server context")
	}
}
