package main

import (
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var iconName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

var iconExts = []string{".svg", ".png", ".webp"}

var iconTypes = map[string]string{
	".svg":  "image/svg+xml",
	".png":  "image/png",
	".webp": "image/webp",
}

// IconServer finds an icon by name, in this order:
//  1. your own file in /config/icons/<name>.svg|png|webp
//  2. icons bundled with Anchor
//  3. a cached copy downloaded from the dashboard-icons project
//  4. download it now from dashboard-icons (once) and cache it
//
// Only icon names used by an app in the saved dashboard are served. Anything else
// is "not found" straight away, so visitors can't make Anchor fetch
// arbitrary names from the internet.
//
// If nothing is found the page shows a coloured letter instead.
type IconServer struct {
	allowed  func(name string) bool
	userDir  string
	cacheDir string
	bundled  fs.FS
	client   *http.Client

	mu     sync.Mutex
	missed map[string]time.Time
}

func NewIconServer(configDir string, bundled fs.FS, allowed func(string) bool) *IconServer {
	s := &IconServer{
		allowed:  allowed,
		userDir:  filepath.Join(configDir, "icons"),
		cacheDir: filepath.Join(configDir, ".icon-cache"),
		bundled:  bundled,
		client:   &http.Client{Timeout: 10 * time.Second},
		missed:   map[string]time.Time{},
	}
	_ = os.MkdirAll(s.userDir, 0o755)
	_ = os.MkdirAll(s.cacheDir, 0o755)
	return s
}

func (s *IconServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !iconName.MatchString(name) || !s.allowed(name) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	// SVGs are images here, never documents: no scripts, ever.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")

	for _, ext := range iconExts {
		ctype := iconTypes[ext]
		if data, err := os.ReadFile(filepath.Join(s.userDir, name+ext)); err == nil {
			s.write(w, ctype, data)
			return
		}
	}
	for _, ext := range iconExts {
		ctype := iconTypes[ext]
		if data, err := fs.ReadFile(s.bundled, name+ext); err == nil {
			s.write(w, ctype, data)
			return
		}
	}
	for _, ext := range iconExts {
		ctype := iconTypes[ext]
		if data, err := os.ReadFile(filepath.Join(s.cacheDir, name+ext)); err == nil {
			s.write(w, ctype, data)
			return
		}
	}
	if data, ctype, ok := s.download(name, ".svg", ".png"); ok {
		s.write(w, ctype, data)
		return
	}
	http.NotFound(w, r)
}

func (s *IconServer) write(w http.ResponseWriter, ctype string, data []byte) {
	w.Header().Set("Content-Type", ctype)
	_, _ = w.Write(data)
}

// download fetches the icon from dashboard-icons, trying each extension in
// turn, and caches what it finds.
func (s *IconServer) download(name string, exts ...string) ([]byte, string, bool) {
	key := name + strings.Join(exts, "")
	s.mu.Lock()
	if t, ok := s.missed[key]; ok && time.Since(t) < time.Hour {
		s.mu.Unlock()
		return nil, "", false
	}
	s.mu.Unlock()

	for _, ext := range exts {
		url := "https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons/" + ext[1:] + "/" + name + ext
		resp, err := s.client.Get(url)
		if err != nil {
			log.Printf("icon %s: %v", name, err)
			break
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && len(data) > 0 {
			_ = os.WriteFile(filepath.Join(s.cacheDir, name+ext), data, 0o644)
			return data, iconTypes[ext], true
		}
	}
	s.mu.Lock()
	s.missed[key] = time.Now()
	s.mu.Unlock()
	return nil, "", false
}

// ServeHome serves /icons/{name}/home.png: the icon as an opaque square PNG
// for a phone's home screen (see homeIcon). The rules are the same as for
// ServeHTTP, and only names used in the saved dashboard are served. With no PNG to
// work from, Anchor's own icon is sent, so a shortcut never gets a broken one.
func (s *IconServer) ServeHome(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !iconName.MatchString(name) || !s.allowed(name) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	w.Header().Set("Content-Type", "image/png")
	data, ok := s.homePNG(name)
	if ok {
		w.Header().Set("Cache-Control", "public, max-age=86400")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=300") // so a real icon replaces this soon
		data = fallbackHomeIcon
	}
	_, _ = w.Write(data)
}

// homePNG looks for a PNG of the icon in the same order as ServeHTTP (yours,
// bundled, cached, downloaded) and makes a home-screen icon of it.
func (s *IconServer) homePNG(name string) ([]byte, bool) {
	file := name + ".png"
	src, err := os.ReadFile(filepath.Join(s.userDir, file))
	if err != nil {
		src, err = fs.ReadFile(s.bundled, file)
	}
	if err != nil {
		src, err = os.ReadFile(filepath.Join(s.cacheDir, file))
	}
	if err != nil {
		var ok bool
		if src, _, ok = s.download(name, ".png"); !ok {
			return nil, false
		}
	}
	return homeIcon(src)
}
