package main

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// changed reports whether open pages were told something changed, and
// clears the signal for the next check.
func changed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-time.After(50 * time.Millisecond):
		return false
	}
}

func TestPagesAreToldWhenAnAppChanges(t *testing.T) {
	h, _, fp := setupAllDown(t, allDownConfig)
	ch, stop := h.checker.changes.Subscribe()
	defer stop()

	h.runRound()
	if !changed(ch) {
		t.Fatal("first results (grey to green) should reach open pages")
	}
	h.runRound()
	if changed(ch) {
		t.Fatal("nothing changed, so pages shouldn't be told to reload")
	}

	fp.set("books", down)
	h.runRound()
	if changed(ch) {
		t.Fatal("one failure doesn't change the dot, so pages shouldn't be told")
	}
	h.runRound()
	if !changed(ch) {
		t.Fatal("Books turning red should reach open pages")
	}

	fp.set("books", up)
	h.runRound()
	if !changed(ch) {
		t.Fatal("Books coming back should reach open pages")
	}
}

func TestPagesAreToldWhenAnchorLosesEverything(t *testing.T) {
	h, _, fp := setupAllDown(t, allDownConfig)
	h.runRound()
	ch, stop := h.checker.changes.Subscribe()
	defer stop()

	for _, id := range []string{"photos", "books", "movies"} {
		fp.set(id, down)
	}
	h.runRound()
	if !changed(ch) {
		t.Fatal("dots pausing and the admin note appearing should reach open pages")
	}
	for _, id := range []string{"photos", "books", "movies"} {
		fp.set(id, up)
	}
	h.runRound()
	if !changed(ch) {
		t.Fatal("recovering from an all-down event should reach open pages")
	}
}

// countingProbes records which apps were checked.
func countingProbes(h *handler, fp *fakeProbes) func() []string {
	var mu sync.Mutex
	var ids []string
	h.checker.probe = func(a App) probeResult {
		mu.Lock()
		ids = append(ids, a.ID)
		mu.Unlock()
		return fp.probe(a)
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := ids
		ids = nil
		return out
	}
}

// An app that's down is checked again every few seconds, so it turns
// green right after it comes back instead of at the next full round.
func TestDownAppRecoversBetweenFullRounds(t *testing.T) {
	h, fn, fp := setupAllDown(t, allDownConfig)
	fp.set("books", down)
	h.runRound()
	h.runRound()
	fn.waitCount(t, 1) // "Books is down"
	ch, stop := h.checker.changes.Subscribe()
	defer stop()

	fp.set("books", up)
	h.checker.recheck = 10 * time.Millisecond
	start := time.Now()
	h.checker.waitForNextRound(300 * time.Millisecond)
	waited := time.Since(start)

	if b := findApp(get(t, h, loopback, "mom@example.com", ""), "Books"); b.Status != "up" || b.Locked {
		t.Fatalf("Books should be back before the next full round: %+v", b)
	}
	if !changed(ch) {
		t.Fatal("the recovery should reach open pages")
	}
	fn.waitCount(t, 2) // "Books is back"
	if waited < 300*time.Millisecond || waited > 2*time.Second {
		t.Fatalf("rechecks must not move the next full round: waited %v, want about 300ms", waited)
	}
}

// Only red apps are rechecked early. An app with a single failure keeps
// its full-round grace period, so a quick restart still doesn't flash red.
func TestRechecksOnlyTouchRedApps(t *testing.T) {
	h, _, fp := setupAllDown(t, allDownConfig)
	fp.set("books", down)
	h.runRound()
	h.runRound() // Books red
	fp.set("photos", down)
	h.runRound() // Photos: one failure, still not red
	probed := countingProbes(h, fp)

	h.checker.recheck = 10 * time.Millisecond
	h.checker.waitForNextRound(100 * time.Millisecond)

	ids := probed()
	if len(ids) == 0 {
		t.Fatal("Books should have been rechecked")
	}
	for _, id := range ids {
		if id != "books" {
			t.Fatalf("only red apps should be rechecked early, but %q was", id)
		}
	}
	if p := findApp(get(t, h, loopback, "mom@example.com", ""), "Photos"); p.Status == "down" {
		t.Fatal("rechecks must not speed up turning red")
	}
}

func TestKickEndsTheWaitEarly(t *testing.T) {
	h, _, _ := setupAllDown(t, allDownConfig)
	h.checker.Kick()
	done := make(chan struct{})
	go func() { h.checker.waitForNextRound(time.Minute); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a settings change should start a new round right away")
	}
}

// ---------- /api/events ----------

func TestEventsStreamTellsPagesAboutChanges(t *testing.T) {
	h, _, _ := setupAllDown(t, allDownConfig)
	srv := httptest.NewServer(http.HandlerFunc(h.events))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type %q, want text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control %q, want no-store", cc)
	}

	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()

	waitSubscribers(t, h.checker.changes, 1)
	h.checker.changes.Notify()
	deadline := time.After(2 * time.Second)
	for got := false; !got; {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("stream ended early")
			}
			if strings.HasPrefix(l, "data:") {
				if strings.TrimSpace(strings.TrimPrefix(l, "data:")) != "changed" {
					t.Fatalf("the event should carry no app data, got %q", l)
				}
				got = true
			}
		case <-deadline:
			t.Fatal("no event arrived after a change")
		}
	}

	// Closing the page must free its slot, so nothing piles up over time.
	resp.Body.Close()
	waitSubscribers(t, h.checker.changes, 0)
}

func waitSubscribers(t *testing.T, c *Changes, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		n := len(c.subs)
		c.mu.Unlock()
		if n == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("want %d open pages listening", want)
}

// Several changes in a row, before a page has caught up, arrive as one.
func TestNotifyNeverBlocks(t *testing.T) {
	c := NewChanges()
	ch, stop := c.Subscribe()
	defer stop()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			c.Notify()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Notify blocked on a page that wasn't reading")
	}
	if !changed(ch) {
		t.Fatal("the page should still hear about the changes")
	}
	if changed(ch) {
		t.Fatal("a burst of changes should arrive as a single event")
	}
}
