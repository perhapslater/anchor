package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Opt-in seed for visual tests. Authentication is real; every address and
// account is synthetic. The caller must own a new directory under OS temp.
func TestUIVisualSeed(t *testing.T) {
	if os.Getenv("ANCHOR_UI_SEED") != "1" {
		t.Skip("opt-in disposable visual fixture seed")
	}
	dir := os.Getenv("ANCHOR_CONFIG_DIR")
	rel, err := filepath.Rel(os.TempDir(), dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || !strings.HasPrefix(filepath.Base(dir), "anchor-ui-fixture-") {
		t.Fatal("visual seed requires its own new OS temporary directory")
	}
	if _, err := os.Stat(filepath.Join(dir, "anchor.db")); !os.IsNotExist(err) {
		t.Fatal("visual seed must not open an existing database")
	}
	store, err := newPersistentStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.persistent.db.Close()
	for _, user := range []localUser{
		{ID: "u_visual_administrator", Username: "visual-admin", Name: "Alex O’Connor — 家族", Tailscale: "alex@example.test", Groups: []string{"admin", "family"}, Enabled: true},
		{ID: "u_visual_member", Username: "visual-member", Name: "Sam Rivera", Groups: []string{"family"}, Enabled: true},
	} {
		if err := store.persistent.saveUser(user, "synthetic-visual-password", "setup"); err != nil {
			t.Fatal(err)
		}
	}
	f := false
	base := func(id, name, icon string) App {
		return App{ID: id, Name: name, URL: "https://apps.example.test/" + id, Section: "Media & reading", Icon: icon, Access: []string{"family"}, Check: "off"}
	}
	media := base("media", "Seerr", "seerr")
	media.Description = "Your requests, ready to watch and coming soon."
	books := base("books", "Ebooks", "audiobookshelf")
	books.Description = "Browse the library — books, authors, and EPUB downloads."
	mobile := base("mobile", "Listen on your phone", "audiobookshelf")
	mobile.Mobile = &Mobile{IOS: "https://store.example.test/iphone", Android: "https://store.example.test/android", Server: "https://reader.example.test/" + strings.Repeat("library", 15), Note: "Sign in with your own account. Ask your administrator if you need help. " + strings.Repeat("Long setup instructions should remain readable. ", 6)}
	guide := base("guide", "Getting started", "home-assistant")
	guide.Guide = &Guide{Intro: "Keep these instructions handy when connecting a new device.", Steps: []string{"Open the application on your device.", strings.Repeat("Read the complete setup instructions before continuing. ", 8), "Use your own account and keep your password private."}, ActionLabel: "Open the application", Note: "You can return to these steps any time."}
	maintenance := base("maintenance", "Backup service", "syncthing")
	maintenance.Maintenance = "Scheduled maintenance — your files are safe. Please check back later."
	maintenance.Section = "Tools & utilities"
	long := base("long", strings.Repeat("LongApplicationName", 6), "sonarr")
	long.Description = strings.Repeat("A long description with Unicode — café, 日本語, and O’Connor’s library. ", 6)
	long.Section = "Tools & utilities"
	disabled := base("disabled", "Hidden application", "plex")
	disabled.Disabled = true
	cfg := &Config{Title: "Anchor", Subtitle: "Everything we run at home, in one place", Contact: "the household administrator", CheckInterval: "60s", Sections: []string{"Media & reading", "Tools & utilities"},
		Apps:     []App{media, books, mobile, guide, maintenance, long, disabled},
		Notice:   &Notice{Message: "Welcome aboard. Scheduled maintenance is shown on each affected tile.\nKeep your account details private.", Style: "info"},
		Notify:   &Notify{Server: "https://notifications.example.test", Topic: "synthetic-visual-only", Alerts: &f},
		Requests: &Requests{App: "media", Seerr: "https://seerr.example.test", Plex: "https://plex.example.test"},
		Ebooks:   &EbooksConfig{App: "books", Server: "https://books.example.test", LibraryID: "synthetic-library"},
	}
	revision, err := store.persistent.revision()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.persistent.saveConfig(cfg, revision, "u_visual_administrator"); err != nil {
		t.Fatal(err)
	}
	// This fixture represents a configured installation. Unclaimed/setup
	// screens have their own separate empty installation in the runner.
	if _, err := store.persistent.db.Exec(`UPDATE onboarding SET step=4,completed=1,revision=revision+1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "setup-token")); err != nil {
		t.Fatal(err)
	}
}
