package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPeopleRejectBlankAndCollidingLogins(t *testing.T) {
	for name, people := range map[string]string{
		"empty":          `"": {groups: [admin]}`,
		"whitespace":     `" \t ": {groups: [admin]}`,
		"case collision": "Member@Example.com: {groups: [family]}\n  member@example.com: {groups: [admin]}",
		"trim collision": "' member@example.com ': {groups: [family]}\n  member@example.com: {groups: [admin]}",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseConfig([]byte("people:\n  " + people + "\n")); err == nil {
				t.Fatal("ambiguous or blank login was accepted")
			}
		})
	}
	cfg, err := parseConfig([]byte("people:\n  ' Member@Example.com ': {groups: [family]}\n  non-email-login: {groups: [family]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, login := range []string{"member@example.com", "non-email-login"} {
		if _, ok := cfg.person(login); !ok {
			t.Errorf("valid normalized login %q was lost", login)
		}
	}
}

func TestAnonymousCannotUseMalformedPersonEntry(t *testing.T) {
	h, _ := newFeatureHandler(t, featureConfig)
	cfg, _ := h.store.Get()
	// Defense in depth: bypass parsing to ensure permission checks themselves
	// cannot interpret a malformed entry as an anonymous administrator.
	for _, login := range []string{"", " \t"} {
		cfg.People[login] = Person{Name: "Invalid", Groups: []string{"admin", "family"}}
		if _, ok := cfg.person(login); ok || cfg.isAdmin(login) || cfg.allowed([]string{"everyone", "family", "admin"}, login) {
			t.Fatalf("blank principal %q received permissions", login)
		}
		cfg.Notice = &Notice{Message: "Private family notice"}
		if cfg.noticeFor(login) != nil {
			t.Fatal("anonymous visitor received a notice")
		}
	}
	m := get(t, h, loopback, "", "?as=mom@example.com")
	if m.Known || m.Admin || len(m.Sections) != 0 || len(m.People) != 0 || m.Notice != nil {
		t.Fatalf("anonymous dashboard leaked data: %+v", m)
	}
	if got := postReport(h, "", "photos", nil).Code; got != http.StatusForbidden {
		t.Fatalf("anonymous report: %d", got)
	}
	cfg.Requests = &Requests{App: "photos"}
	h.requests = newRequestStatus(h.store, requestKeys{}, false)
	cfg.Ebooks = &EbooksConfig{App: "photos", Server: "https://abs.test", LibraryID: "lib"}
	h.ebooks = newEbookService("test-key", false)
	for name, gate := range map[string]func(http.ResponseWriter, *http.Request) (*Config, visitor, bool){
		"requests": h.requestsGate, "ebooks": h.ebooksGate,
	} {
		r := httptest.NewRequest("GET", "/api/"+name, nil)
		r.RemoteAddr = loopback
		w := httptest.NewRecorder()
		if _, _, ok := gate(w, r); ok || w.Code != http.StatusNotFound {
			t.Errorf("anonymous %s gate allowed request", name)
		}
	}
	r := httptest.NewRequest("GET", "/open/photos", nil)
	r.RemoteAddr = loopback
	r.SetPathValue("id", "photos")
	w := httptest.NewRecorder()
	h.open(w, r)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/" || strings.Contains(w.Body.String(), "photos.test") {
		t.Fatal("anonymous shortcut revealed the app")
	}
}

func TestApplicationAccessIncludesAdministratorsAndExcludesDisabledApps(t *testing.T) {
	cfg := &Config{People: map[string]Person{
		"administrator": {Groups: []string{"ADMIN"}},
		"member":        {Groups: []string{"family"}},
		"other":         {Groups: []string{"other"}},
	}}
	for _, access := range [][]string{{"family"}, {"member"}, {"admin"}, {"everyone"}} {
		app := &App{Access: access}
		for _, login := range []string{"administrator", "member", "other", "unknown", ""} {
			want := login == "administrator" || login == "member" && access[0] != "admin" || login == "other" && access[0] == "everyone"
			if got := cfg.visibleTo(app, login); got != want {
				t.Errorf("access %v, %q: got %v, want %v", access, login, got, want)
			}
			app.Disabled = true
			if cfg.visibleTo(app, login) {
				t.Errorf("disabled app accessible to %q", login)
			}
			app.Disabled = false
		}
	}
	cfg.Notice = &Notice{Message: "Family only", Access: []string{"family"}}
	if cfg.noticeFor("administrator") != nil || cfg.noticeFor("member") == nil {
		t.Fatal("application override changed announcement audiences")
	}
}
