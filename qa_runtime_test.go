package main

import (
	"bytes"
	"encoding/json"
	"image"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestQADeploymentModeConfiguration(t *testing.T) {
	store, err := newPersistentStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.persistent.db.Close()
	for _, tc := range []struct {
		name, mode, public, allowHTTP string
		valid                         bool
	}{
		{"tailscale default", "", "", "", true},
		{"unknown mode", "internet", "", "", false},
		{"proxy requires origin", "proxy", "", "", false},
		{"proxy HTTPS origin", "proxy", "https://anchor.example.test", "", true},
		{"proxy rejects HTTP", "proxy", "http://anchor.example.test", "", false},
		{"LAN requires opt in", "lan", "http://anchor.example.test", "", false},
		{"LAN requires origin", "lan", "", "1", false},
		{"LAN explicit", "lan", "http://anchor.example.test:18080", "1", true},
		{"LAN rejects HTTPS", "lan", "https://anchor.example.test", "1", false},
		{"no credentials", "proxy", "https://user:password@anchor.example.test", "", false},
		{"no subpaths", "proxy", "https://anchor.example.test/anchor", "", false},
		{"no query", "proxy", "https://anchor.example.test/?key=value", "", false},
		{"no fragment", "proxy", "https://anchor.example.test/#fragment", "", false},
		{"no host", "proxy", "https://:443", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ANCHOR_MODE", tc.mode)
			t.Setenv("ANCHOR_PUBLIC_URL", tc.public)
			t.Setenv("ANCHOR_ALLOW_HTTP", tc.allowHTTP)
			_, err := newAuthentication(store.persistent)
			if (err == nil) != tc.valid {
				t.Fatalf("deployment mode valid=%v; error=%v", tc.valid, err)
			}
		})
	}
}

func TestQACompleteBackupRestoresAccountsAndAppIDs(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	cfg, _ := f.state.store.Get()
	copyCfg := *cfg
	copyCfg.Apps = []App{{ID: "stable-backup-app", Name: "Renamed app", URL: "https://example.invalid/", Access: []string{f.user.ID}, Check: "off"}}
	revision, _ := f.state.revision()
	if err := f.state.saveConfig(&copyCfg, revision, f.user.ID); err != nil {
		t.Fatal(err)
	}
	backup := f.request("POST", "/api/admin/backup", map[string]any{}, true)
	if backup.Code != 200 {
		t.Fatalf("backup status: %d", backup.Code)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "anchor.db"), backup.Body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	restored, err := newPersistentStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.persistent.db.Close()
	user, ok := restored.persistent.user(f.user.ID)
	if !ok || !user.Enabled || !groupAdmin(user.Groups) || !checkPassword(user.password, "correct-password-123") {
		t.Fatal("backup did not preserve the administrator and password")
	}
	restoredConfig, _ := restored.Get()
	app := restoredConfig.appByID("stable-backup-app")
	if restored.persistent.needsSetup() || app == nil || app.Name != "Renamed app" || !restoredConfig.visibleTo(app, user.ID) {
		t.Fatal("restored accounts, stable app IDs or user grants changed")
	}
	var sessions int
	if err := restored.persistent.db.QueryRow(`SELECT count(*) FROM sessions WHERE user_id=?`, user.ID).Scan(&sessions); err != nil || sessions != 1 {
		t.Fatal("full backup did not preserve its intentionally sensitive session state")
	}
}

func TestQAManifestAssetsHaveDeclaredDimensions(t *testing.T) {
	var manifest struct {
		StartURL string `json:"start_url"`
		Scope    string `json:"scope"`
		Icons    []struct{ Src, Sizes, Type string }
	}
	raw, err := fs.ReadFile(webFiles, "web/manifest.webmanifest")
	if err != nil || json.Unmarshal(raw, &manifest) != nil || manifest.StartURL != "/" || manifest.Scope != "/" {
		t.Fatal("invalid manifest or application scope")
	}
	for _, icon := range manifest.Icons {
		data, err := fs.ReadFile(webFiles, "web/"+strings.TrimPrefix(icon.Src, "/"))
		if err != nil {
			t.Fatal("manifest asset missing")
		}
		if icon.Type == "image/png" {
			cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
			if err != nil || format != "png" || cfg.Width != cfg.Height ||
				(icon.Sizes == "192x192" && cfg.Width != 192) || (icon.Sizes == "512x512" && cfg.Width != 512) {
				t.Fatalf("incorrect manifest icon dimensions for %s", icon.Src)
			}
		}
	}
}

func TestQAForwardedIdentityIsIgnoredOutsideTailscale(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	u := f.user
	u.Tailscale = "linked@example.test"
	if err := f.state.saveUser(u, "", u.ID); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"lan", "proxy"} {
		f.auth.mode = mode
		r := httptest.NewRequest("GET", "http://anchor.test/api/auth/status", nil)
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("Tailscale-User-Login", u.Tailscale)
		r.Header.Set("X-Forwarded-User", u.Tailscale)
		r.Header.Set("X-Forwarded-Proto", "https")
		if got := f.auth.identify(r); got.user.ID != "" {
			t.Fatalf("%s trusted forwarded identity", mode)
		}
	}
}
