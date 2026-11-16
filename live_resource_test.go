package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEventAdmissionBoundsAndCleanup(t *testing.T) {
	c := NewChanges()
	var stops []func()
	for person := 0; person < 16; person++ {
		for tab := 0; tab < 8; tab++ {
			_, stop, ok := c.subscribe(fmt.Sprint(person), true)
			if !ok {
				t.Fatal("valid stream refused")
			}
			stops = append(stops, stop)
		}
	}
	if _, _, ok := c.subscribe("another", true); ok {
		t.Fatal("global stream cap bypassed")
	}
	for _, stop := range stops {
		stop()
		stop()
	}
	if len(c.subs) != 0 || len(c.people) != 0 {
		t.Fatal("stream cleanup leaked")
	}
	for i := 0; i < 4; i++ {
		_, stop, ok := c.subscribe("", true)
		if !ok {
			t.Fatal("anonymous setup")
		}
		defer stop()
	}
	if _, _, ok := c.subscribe("", true); ok {
		t.Fatal("anonymous cap bypassed")
	}
	h, _, _ := setupAllDown(t, allDownConfig)
	h.checker.changes = c
	r := httptest.NewRequest("GET", "/api/events", nil)
	r.RemoteAddr = "192.0.2.1:1000"
	r.Header.Set("Tailscale-User-Login", "mom@example.com")
	w := httptest.NewRecorder()
	h.events(w, r)
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatal("forged external identity escaped anonymous limit")
	}
}

type failedEventWriter struct{ *ebookDeadlineRecorder }

func (w failedEventWriter) Write([]byte) (int, error) { return 0, errors.New("blocked peer") }

func TestEventWriteFailureReleasesSubscription(t *testing.T) {
	h, _, _ := setupAllDown(t, allDownConfig)
	w := failedEventWriter{&ebookDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}}
	done := make(chan struct{})
	go func() { h.events(w, httptest.NewRequest("GET", "/api/events", nil)); close(done) }()
	waitSubscribers(t, h.checker.changes, 1)
	h.checker.changes.Notify()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("write failure did not stop stream")
	}
	waitSubscribers(t, h.checker.changes, 0)
	if w.deadline.IsZero() {
		t.Fatal("stream writes have no deadline")
	}
}

func TestHTTPBodyTimeoutDoesNotKillEventStreams(t *testing.T) {
	h, _, _ := setupAllDown(t, allDownConfig)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/events", h.events)
	mux.HandleFunc("POST /slow", func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			w.WriteHeader(http.StatusRequestTimeout)
		}
	})
	srv := httptest.NewUnstartedServer(mux)
	srv.Config = newHTTPServer(mux)
	if srv.Config.ReadTimeout == 0 || srv.Config.IdleTimeout == 0 || srv.Config.WriteTimeout != 0 {
		t.Fatal("incorrect HTTP timeout policy")
	}
	srv.Config.ReadTimeout = 40 * time.Millisecond
	srv.Start()
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-time.After(2 * srv.Config.ReadTimeout)
	h.checker.changes.Notify()
	lines := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "data:") {
				lines <- sc.Text()
				return
			}
		}
		lines <- ""
	}()
	select {
	case line := <-lines:
		if line != "data: changed" {
			t.Fatal("SSE ended at body timeout")
		}
	case <-time.After(time.Second):
		t.Fatal("no SSE event")
	}
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(time.Second))
	fmt.Fprint(conn, "POST /slow HTTP/1.1\r\nHost: local.test\r\nContent-Length: 100\r\n\r\nx")
	answer, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer answer.Body.Close()
	if answer.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("slow body: %d", answer.StatusCode)
	}
}
