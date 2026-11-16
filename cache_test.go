package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// The page and its scripts change together, so a phone must never keep an old
// copy of one next to a new copy of the other.
func TestPageAndCodeAreAlwaysRevalidated(t *testing.T) {
	web := fstest.MapFS{
		"index.html":       {Data: []byte("<!doctype html>")},
		"app.js":           {Data: []byte("//")},
		"open.js":          {Data: []byte("//")},
		"style.css":        {Data: []byte("/**/")},
		"sw.js":            {Data: []byte("//")},
		"account.html":     {Data: []byte("<!doctype html>")},
		"account.js":       {Data: []byte("//")},
		"session.js":       {Data: []byte("//")},
		"manage.html":      {Data: []byte("<!doctype html>")},
		"manage.js":        {Data: []byte("//")},
		"admin.css":        {Data: []byte("/**/")},
		"brand/anchor.svg": {Data: []byte("<svg/>")},
	}
	h := staticHandler(web)
	for path, want := range map[string]string{
		"/":                 "no-cache",
		"/sw.js":            "no-cache",
		"/app.js":           "no-cache",
		"/open.js":          "no-cache",
		"/style.css":        "no-cache",
		"/brand/anchor.svg": "public, max-age=3600",
		"/account.html":     "no-cache", "/account.js": "no-cache", "/session.js": "no-cache",
		"/manage.html": "no-cache", "/manage.js": "no-cache", "/admin.css": "no-cache",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if got := rec.Header().Get("Cache-Control"); got != want {
			t.Errorf("%s: Cache-Control %q, want %q", path, got, want)
		}
	}
}

func TestAdminAssetURLsChangeWithRebuiltSource(t *testing.T) {
	source := fstest.MapFS{
		"manage.html": {Data: []byte(`<link rel="stylesheet" href="/admin.css"><script src="/session.js"></script><script src="/manage.js"></script>`)},
		"manage.js":   {Data: []byte("first editor build")},
	}
	render := func() string {
		t.Helper()
		handler := staticHandler(source)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "/manage.html", nil))
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-cache" {
			t.Fatal("editor HTML must revalidate")
		}
		for _, asset := range []string{"admin.css", "session.js", "manage.js"} {
			if !strings.Contains(w.Body.String(), "/"+asset+"?v=") {
				t.Fatalf("missing asset version for %s", asset)
			}
		}
		return w.Body.String()
	}
	first := render()
	source["manage.js"] = &fstest.MapFile{Data: []byte("second editor build")}
	second := render()
	if first == second {
		t.Fatal("rebuild retained stale editor asset URLs")
	}
}
