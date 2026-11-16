package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

const testConfig = `
title: Test
contact: Alex
people:
  Admin@Example.com: {name: Admin, groups: [admin, family]}
  wife@example.com:  {name: Wife, groups: [wife, family]}
  mom@example.com:   {name: Mom, groups: [mom, family]}
apps:
  - {name: Photos, url: https://photos.test/, access: [family], check: "off"}
  - {name: Cameras, url: https://nvr.test/, access: [wife, admin], check: "off"}
  - {name: Alerts, url: https://ntfy.test/, access: [admin], check: "off"}
  - {name: Just Mom, url: https://mom.test/, access: [mom@example.com], check: "off"}
`

func newTestHandler(t *testing.T) *handler {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "anchor.yaml"), []byte(testConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := NewConfigStore(filepath.Join(dir, "anchor.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, e := store.Get(); e != "" {
		t.Fatalf("config error: %s", e)
	}
	return &handler{store: store, checker: NewChecker(store)}
}

func get(t *testing.T, h *handler, remote, login, query string) apiMe {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/me"+query, nil)
	req.RemoteAddr = remote
	if login != "" {
		req.Header.Set("Tailscale-User-Login", login)
	}
	rec := httptest.NewRecorder()
	h.me(rec, req)
	var out apiMe
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func names(m apiMe) []string {
	var n []string
	for _, s := range m.Sections {
		for _, a := range s.Apps {
			n = append(n, a.Name)
		}
	}
	return n
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

const loopback = "127.0.0.1:50000"

func TestEachPersonSeesTheirApps(t *testing.T) {
	h := newTestHandler(t)
	cases := map[string][]string{
		"admin@example.com": {"Photos", "Cameras", "Alerts", "Just Mom"},
		"ADMIN@example.com": {"Photos", "Cameras", "Alerts", "Just Mom"}, // case-insensitive
		"wife@example.com":  {"Photos", "Cameras"},
		"mom@example.com":   {"Photos", "Just Mom"},
		"stranger@x.com":    nil,
	}
	for login, want := range cases {
		got := names(get(t, h, loopback, login, ""))
		if !equal(got, want) {
			t.Errorf("%s: got %v, want %v", login, got, want)
		}
	}
}

func TestHeadersFromOutsideAreIgnored(t *testing.T) {
	h := newTestHandler(t)
	m := get(t, h, "192.168.1.50:40000", "admin@example.com", "")
	if m.Known || m.Login != "" || len(names(m)) != 0 {
		t.Fatalf("header from LAN address was trusted: %+v", m)
	}
}

func TestAdminPreview(t *testing.T) {
	h := newTestHandler(t)
	m := get(t, h, loopback, "admin@example.com", "?as=mom@example.com")
	if !equal(names(m), []string{"Photos", "Just Mom"}) || m.ViewingAs != "Mom" {
		t.Fatalf("admin preview wrong: %v %q", names(m), m.ViewingAs)
	}
	// Non-admins can't use preview to see more.
	m = get(t, h, loopback, "mom@example.com", "?as=admin@example.com")
	if !equal(names(m), []string{"Photos", "Just Mom"}) || m.Admin || m.People != nil {
		t.Fatalf("non-admin preview leaked: %v", names(m))
	}
}

func TestBadConfigIsRejected(t *testing.T) {
	bad := []string{
		"apps: [{name: X, url: https://x/}]",  // no access
		"apps: [{name: X, access: [family]}]", // no url
		"titel: typo",                         // unknown field
		"check_interval: 1s",                  // too fast
		"apps: [{name: X, url: u, access: [a]}, {name: x, url: u, access: [a]}]", // duplicate
	}
	for _, c := range bad {
		if _, err := parseConfig([]byte(c)); err == nil {
			t.Errorf("expected error for %q", c)
		}
	}
	if _, err := parseConfig(exampleConfig); err != nil {
		t.Errorf("example config invalid: %v", err)
	}
}
