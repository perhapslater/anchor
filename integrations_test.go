package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const multiConnectionConfig = `
people:
  mom@example.com: {name: Mom, groups: [family]}
  admin@example.com: {name: Admin, groups: [admin, family]}
apps:
  - {id: media-a, name: Media A, url: https://a.test, access: [family], check: "off"}
  - {id: media-b, name: Media B, url: https://b.test, access: [admin], check: "off"}
  - {id: books-a, name: Books A, url: https://a.test, access: [family], check: "off"}
  - {id: books-b, name: Books B, url: https://b.test, access: [admin], check: "off"}
request_connections:
  - {app: media-a, seerr: https://a.test, plex: https://plex-a.test}
  - {app: media-b, seerr: https://b.test, plex: https://plex-b.test}
ebook_libraries:
  - {app: books-a, server: https://a.test, library_id: library-a}
  - {app: books-b, server: https://b.test, library_id: library-b}
`

func TestMultipleConnectionSelectionAndValidation(t *testing.T) {
	cfg, err := parseConfig([]byte(multiConnectionConfig))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.requestFor("") != nil || cfg.ebookFor("") != nil || cfg.requestFor("media-b").Seerr != "https://b.test" || cfg.ebookFor("books-a").LibraryID != "library-a" {
		t.Fatal("ambiguous or incorrect source selection")
	}
	if !cfg.requestFor("media-a").scoped || !cfg.ebookFor("books-a").scoped {
		t.Fatal("new sources must use independent credentials")
	}
	for _, invalid := range []string{
		strings.Replace(multiConnectionConfig, "app: media-b", "app: media-a", 1),
		strings.Replace(multiConnectionConfig, "app: books-b", "app: books-a", 1),
		strings.Replace(multiConnectionConfig, "app: books-b", "app: missing", 1),
	} {
		if _, err := parseConfig([]byte(invalid)); err == nil {
			t.Fatal("invalid or duplicate connection accepted")
		}
	}
}

func TestMultipleConnectionsKeepCredentialsSeparateAndRemoveOrphans(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	cfg, err := parseConfig([]byte(multiConnectionConfig))
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]string{"requests/media-a/seerr": "source-a", "requests/media-b/seerr": "source-b", "ebooks/books-a/abs": "library-a-key", "ebooks/books-b/abs": "library-b-key"}
	rev, _ := f.state.revision()
	if err := f.state.saveConfigAndSecrets(cfg, rev, f.user.ID, keys); err != nil {
		t.Fatal(err)
	}
	loaded, _ := f.state.store.Get()
	if loaded.requestFor("media-a").keys.Seerr != "source-a" || loaded.requestFor("media-b").keys.Seerr != "source-b" || loaded.ebookFor("books-a").secret != "library-a-key" || loaded.ebookFor("books-b").secret != "library-b-key" {
		t.Fatal("credentials crossed source boundaries")
	}
	state := f.request("GET", "/api/admin/state", nil, false).Body.String()
	exported := f.request("GET", "/api/admin/export", nil, false).Body.String()
	for _, secret := range keys {
		if strings.Contains(state, secret) || strings.Contains(exported, secret) {
			t.Fatal("credential value exposed in admin state or YAML")
		}
	}
	copyCfg := *loaded
	copyCfg.RequestConnections = append([]Requests(nil), loaded.RequestConnections[1:]...)
	copyCfg.EbookLibraries = append([]EbooksConfig(nil), loaded.EbookLibraries[1:]...)
	rev, _ = f.state.revision()
	if err := f.state.saveConfig(&copyCfg, rev, f.user.ID); err != nil {
		t.Fatal(err)
	}
	loaded, _ = f.state.store.Get()
	if loaded.credentials["requests/media-a/seerr"] != "" || loaded.credentials["ebooks/books-a/abs"] != "" || loaded.requestFor("media-b").keys.Seerr != "source-b" || loaded.ebookFor("books-b").secret != "library-b-key" {
		t.Fatal("removing one connection affected another or retained orphaned credentials")
	}
}

func TestMultipleConnectionAPIsIsolateRowsDownloadsAndAccess(t *testing.T) {
	store := storeFor(t, multiConnectionConfig)
	cfg, _ := store.Get()
	h := &handler{store: store, requests: newRequestStatus(store, requestKeys{}, false), ebooks: newEbookService("", false), reqReads: newRateLimiter(0, 1000), booksReads: newRateLimiter(0, 1000), bookFiles: newRateLimiter(0, 1000)}
	for _, connection := range cfg.requestConnections() {
		local := *cfg
		local.Requests = connection
		service := h.requestService(&local)
		service.build = func(_ context.Context, selected *Config) (*snapshot, error) {
			return &snapshot{Rows: []requestRow{{Title: selected.Requests.App, Kind: "movie", State: "ready"}}, PlexOK: true}, nil
		}
	}
	for _, library := range cfg.ebookLibraries() {
		library.secret, library.secretMode = library.LibraryID+"-key", true
		local := *cfg
		local.Ebooks = library
		service := h.ebookService(&local)
		service.snapshot = &ebookSnapshot{Rows: []ebook{{ID: "same-book-id", Title: library.App, Format: "epub"}}, At: time.Now(), sig: library.sig(library.secret)}
		service.streamClient.Transport = ebookDownloadTransport(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("Authorization") != "Bearer "+library.secret || r.URL.Host != strings.TrimPrefix(library.Server, "https://") {
				t.Error("download used another library's credentials or server")
			}
			return ebookReply(200, []byte("PK\x03\x04"+library.App)), nil
		})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/requests", h.requestsList)
	mux.HandleFunc("POST /api/requests/refresh", h.requestsRefresh)
	mux.HandleFunc("GET /api/requests/poster/{kind}/{id}", h.requestsPoster)
	mux.HandleFunc("GET /api/ebooks", h.ebooksList)
	mux.HandleFunc("POST /api/ebooks/refresh", h.ebooksList)
	mux.HandleFunc("GET /api/ebooks/{id}/cover", h.ebookCover)
	mux.HandleFunc("GET /api/ebooks/{id}/download", h.ebookDownload)
	request := func(login, method, path string) *ebookDeadlineRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader("{}"))
		r.RemoteAddr = loopback
		r.Header.Set("Tailscale-User-Login", login)
		r.Header.Set("Content-Type", "application/json")
		w := &ebookDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		mux.ServeHTTP(w, r)
		return w
	}
	for _, source := range []string{"media-a", "media-b", "media-a"} {
		w := request("admin@example.com", "GET", "/api/requests?app="+source)
		var body apiRequests
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Rows) != 1 || body.Rows[0].Title != source {
			t.Fatalf("wrong request data for %s: %s", source, w.Body.String())
		}
	}
	for _, source := range []string{"books-a", "books-b", "books-a"} {
		w := request("admin@example.com", "GET", "/api/ebooks?app="+source)
		var body apiEbooks
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Rows) != 1 || body.Rows[0].Title != source {
			t.Fatalf("wrong catalog for %s: %s", source, w.Body.String())
		}
		w = request("admin@example.com", "GET", "/api/ebooks/same-book-id/download?app="+source)
		if w.Code != 200 || w.Body.String() != "PK\x03\x04"+source {
			t.Fatalf("wrong download for %s: %d %s", source, w.Code, w.Body.String())
		}
	}
	for _, path := range []string{"/api/requests?app=media-b", "/api/requests/refresh?app=media-b", "/api/requests/poster/movie/1?app=media-b", "/api/ebooks?app=books-b", "/api/ebooks/refresh?app=books-b", "/api/ebooks/same-book-id/cover?app=books-b", "/api/ebooks/same-book-id/download?app=books-b", "/api/ebooks", "/api/requests", "/api/ebooks?app=missing"} {
		method := "GET"
		if strings.Contains(path, "refresh") {
			method = "POST"
		}
		if w := request("mom@example.com", method, path); w.Code != 404 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("source access gate failed for %s: %d", path, w.Code)
		}
	}
}
