package main

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProbes lets a test decide each app's check result.
type fakeProbes struct {
	mu  sync.Mutex
	res map[string]probeResult
}

func (f *fakeProbes) set(id string, r probeResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.res[id] = r
}

func (f *fakeProbes) probe(a App) probeResult {
	if strings.EqualFold(a.Check, "off") {
		return probeResult{state: "unknown", detail: "checks turned off", skipped: true}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.res[a.ID]; ok {
		return r
	}
	return probeResult{state: "up", detail: "200 OK"}
}

var (
	up   = probeResult{state: "up", detail: "200 OK"}
	down = probeResult{state: "down", detail: "connection refused", failure: true}
	dns  = probeResult{state: "unknown", detail: "no such host", dns: true}
)

const allDownConfig = `
contact: Alex
people:
  admin@example.com: {name: Admin, groups: [admin, family]}
  mom@example.com:   {name: Mom, groups: [family]}
notify: {server: https://ntfy.example.com, topic: anchor_alert}
apps:
  - {name: Photos, url: https://photos.test/, access: [family]}
  - {name: Books, url: https://books.test/, access: [family]}
  - {name: Movies, url: https://movies.test/, access: [family]}
  - {name: NoCheck, url: https://x.test/, access: [family], check: "off"}
`

func setupAllDown(t *testing.T, cfg string) (*handler, *fakeNotifier, *fakeProbes) {
	h, fn := newFeatureHandler(t, cfg)
	fp := &fakeProbes{res: map[string]probeResult{}}
	h.checker.probe = fp.probe
	return h, fn, fp
}

func (h *handler) runRound() {
	cfg, _ := h.store.Get()
	h.checker.round(cfg)
}

func titles(fn *fakeNotifier) []string {
	fn.mu.Lock()
	defer fn.mu.Unlock()
	var t []string
	for _, n := range fn.sent {
		t = append(t, n.Title)
	}
	return t
}

func TestEverythingDownFailsOpenWithOneAlert(t *testing.T) {
	h, fn, fp := setupAllDown(t, allDownConfig)
	for _, id := range []string{"photos", "books", "movies"} {
		fp.set(id, down)
	}

	h.runRound()
	time.Sleep(50 * time.Millisecond)
	if fn.count() != 0 {
		t.Fatalf("one all-down round must not alert yet: %v", titles(fn))
	}
	h.runRound()
	h.runRound()
	fn.waitCount(t, 1)
	time.Sleep(50 * time.Millisecond)
	if got := titles(fn); len(got) != 1 || got[0] != "Anchor can't reach any apps" {
		t.Fatalf("want exactly one Anchor alert, got %v", got)
	}

	// Nothing is locked for the family; dots are grey.
	m := get(t, h, loopback, "mom@example.com", "")
	for _, name := range []string{"Photos", "Books", "Movies"} {
		a := findApp(m, name)
		if a.Locked || a.Status != "unknown" {
			t.Errorf("%s: locked=%v status=%s, want unlocked unknown", name, a.Locked, a.Status)
		}
	}
	if m.CheckProblem != "" {
		t.Error("the check problem is for admins only")
	}
	if a := get(t, h, loopback, "admin@example.com", ""); !strings.Contains(a.CheckProblem, "can't reach any") {
		t.Errorf("admin should see the problem: %q", a.CheckProblem)
	}

	// Recovery: one "again" message, no per-app "is back" spam.
	for _, id := range []string{"photos", "books", "movies"} {
		fp.set(id, up)
	}
	h.runRound()
	fn.waitCount(t, 2)
	time.Sleep(50 * time.Millisecond)
	if got := titles(fn); len(got) != 2 || got[1] != "Anchor can reach your apps again" {
		t.Fatalf("recovery messages: %v", got)
	}
	if a := get(t, h, loopback, "admin@example.com", ""); a.CheckProblem != "" {
		t.Error("problem should clear after recovery")
	}
	if a := findApp(get(t, h, loopback, "mom@example.com", ""), "Photos"); a.Status != "up" {
		t.Errorf("status after recovery: %s", a.Status)
	}
}

func TestOneAppDownStillLocksAndAlerts(t *testing.T) {
	h, fn, fp := setupAllDown(t, allDownConfig)
	fp.set("books", down)
	h.runRound()
	h.runRound()
	fn.waitCount(t, 1)
	if got := titles(fn); got[0] != "Books is down" {
		t.Fatalf("got %v", got)
	}
	if b := findApp(get(t, h, loopback, "mom@example.com", ""), "Books"); !b.Locked {
		t.Fatal("a single broken app should still lock")
	}
}

func TestDNSFailureEverywhereCountsAsAnchorProblem(t *testing.T) {
	h, fn, fp := setupAllDown(t, allDownConfig)
	for _, id := range []string{"photos", "books", "movies"} {
		fp.set(id, dns)
	}
	h.runRound()
	h.runRound()
	fn.waitCount(t, 1)
	if got := titles(fn); got[0] != "Anchor can't reach any apps" {
		t.Fatalf("got %v", got)
	}
}

func TestAppAlreadyDownStaysDownAfterAnchorRecovers(t *testing.T) {
	h, fn, fp := setupAllDown(t, allDownConfig)
	fp.set("books", down)
	h.runRound()
	h.runRound() // Books is down and alerted
	fn.waitCount(t, 1)

	for _, id := range []string{"photos", "movies"} {
		fp.set(id, down)
	}
	h.runRound()
	h.runRound() // everything down: Anchor alert, Books shows paused
	fn.waitCount(t, 2)
	if b := findApp(get(t, h, loopback, "mom@example.com", ""), "Books"); b.Locked {
		t.Fatal("during an all-down event nothing is locked")
	}

	for _, id := range []string{"photos", "movies"} {
		fp.set(id, up)
	}
	h.runRound() // Anchor recovers; Books is still really down
	fn.waitCount(t, 3)
	time.Sleep(50 * time.Millisecond)
	if got := titles(fn); len(got) != 3 || got[2] != "Anchor can reach your apps again" {
		t.Fatalf("Books must not re-alert or fake-recover: %v", got)
	}
	if b := findApp(get(t, h, loopback, "mom@example.com", ""), "Books"); !b.Locked || b.Status != "down" {
		t.Fatalf("Books should be down and locked again: %+v", b)
	}
}

func TestSingleAppConfigIsNeverAllDown(t *testing.T) {
	cfg := `
people: {mom@example.com: {groups: [family]}}
notify: {server: https://ntfy.example.com, topic: anchor_alert}
apps:
  - {name: Photos, url: https://photos.test/, access: [family]}
`
	h, fn, fp := setupAllDown(t, cfg)
	fp.set("photos", down)
	h.runRound()
	h.runRound()
	fn.waitCount(t, 1)
	if got := titles(fn); got[0] != "Photos is down" {
		t.Fatalf("with one app, a failure is that app's problem: %v", got)
	}
}
