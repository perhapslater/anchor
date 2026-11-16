package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image/png"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestBundledServicesWorkWithoutIconDownloads(t *testing.T) {
	bundled, err := fs.Sub(webFiles, "web/icons")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, icon := range bundledServices {
		if names[icon.Name] || !iconName.MatchString(icon.Name) || icon.Label == "" || icon.Category == "" {
			t.Fatalf("invalid or duplicate catalogue entry: %+v", icon)
		}
		names[icon.Name] = true
		data, err := fs.ReadFile(bundled, icon.Name+".png")
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(data))
		if err != nil || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > maxIconSide || cfg.Height > maxIconSide {
			t.Fatalf("invalid home-screen source for %s: %+v %v", icon.Name, cfg, err)
		}
	}
	server := NewIconServer(t.TempDir(), bundled, func(name string) bool { return names[name] })
	transport := &countingTransport{}
	server.client.Transport = transport
	mux := http.NewServeMux()
	mux.Handle("GET /icons/{name}", server)
	for _, icon := range bundledServices {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/icons/"+icon.Name, nil))
		if w.Code != 200 || w.Body.Len() == 0 {
			t.Fatalf("bundled tile icon %s: %d", icon.Name, w.Code)
		}
	}
	for _, name := range []string{"radarr", "sonarr", "plex", "ntfy", "frigate"} {
		image, ok := server.homePNG(name)
		if !ok || bytes.Equal(image, fallbackHomeIcon) {
			t.Fatalf("home-screen icon unavailable: %s", name)
		}
	}
	if transport.n.Load() != 0 {
		t.Fatal("bundled icons attempted an internet download")
	}
}
func TestMajorServiceNamesSelectDefaultsWithoutOverridingCustomIcons(t *testing.T) {
	for _, test := range []struct{ name, want, explicit string }{
		{"Radarr", "radarr", ""}, {"SONARR", "sonarr", ""}, {"Home Assistant", "home-assistant", ""},
		{"qBittorrent", "qbittorrent", ""}, {"Pi-hole", "pi-hole", ""}, {"ntfy", "ntfy", ""},
		{"Plex", "my-custom-icon", "my-custom-icon"}, {"Something custom", "custom-id", ""},
	} {
		raw := fmt.Sprintf("apps:\n  - id: custom-id\n    name: %s\n    url: https://example.test/\n    access: [everyone]\n    icon: %s\n", test.name, test.explicit)
		cfg, err := parseConfig([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Apps[0].Icon != test.want {
			t.Errorf("%s: %s, want %s", test.name, cfg.Apps[0].Icon, test.want)
		}
	}
}
func TestIconProvenanceMatchesBundledFiles(t *testing.T) {
	raw, err := os.ReadFile("third_party/dashboard-icons/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	type asset struct{ File, SHA256 string }
	var manifest struct {
		Revision string
		Assets   []asset

	}
	if err = json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Revision) != 40 || len(manifest.Assets) == 0 {
		t.Fatal("missing pinned provenance")
	}
	for _, entry := range manifest.Assets {
		if !strings.HasPrefix(entry.File, "web/icons/") {
			t.Fatal("unexpected manifest path")
		}
		data, err := os.ReadFile(entry.File)
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		if hex.EncodeToString(hash[:]) != entry.SHA256 {
			t.Errorf("icon source hash mismatch: %s", entry.File)
		}
	}
}
func TestOnlyAdministratorsCanPreviewUnconfiguredIcons(t *testing.T) {
	f := newAccountFixture(t)
	if w := f.request("GET", "/api/admin/icons/radarr", nil, false); w.Code != 403 {
		t.Fatal("anonymous icon preview")
	}
	f.setup(t)
	w := f.request("GET", "/api/admin/icons/radarr", nil, false)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || !bytes.HasPrefix(w.Body.Bytes(), []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatal("admin could not preview an unconfigured bundled icon")
	}
	if w = f.request("GET", "/api/admin/icons/not-a-bundled-service", nil, false); w.Code != 404 {
		t.Fatal("unknown preview name accepted")
	}
	w = f.request("GET", "/api/admin/state", nil, false)
	var state struct {
		Icons []serviceIcon `json:"icons"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Icons) != len(bundledServices) {
		t.Fatal("editor catalogue missing services")
	}
}
