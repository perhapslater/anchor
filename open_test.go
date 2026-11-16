package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

// openConfig is featureConfig plus a few apps for the home-screen tests.
const openConfig = featureConfig + `  - {name: Hidden, url: https://hidden.test/, access: [family], check: "off", homescreen: false}
  - {name: Odd, url: "https://odd.test/", access: [family], check: "off"}
  - {name: "<b>Bold</b> & Co", url: https://bold.test/, access: [family], check: "off"}
  - {name: Remote, url: https://remote.test/, access: [family], check: "off", icon: "https://cdn.test/remote.png"}
`

func TestHomeScreenIsOnUnlessTurnedOff(t *testing.T) {
	h, _ := newFeatureHandler(t, openConfig)
	m := get(t, h, loopback, "mom@example.com", "")
	if a := findApp(m, "Photos"); a == nil || !a.HomeScreen {
		t.Fatalf("home screen should be on by default: %+v", a)
	}
	if a := findApp(m, "Hidden"); a == nil || a.HomeScreen {
		t.Fatalf("homescreen: false should turn it off: %+v", a)
	}
}

func doOpen(h *handler, login, target string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /open/{id}", h.open)
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.RemoteAddr = loopback
	if login != "" {
		req.Header.Set("Tailscale-User-Login", login)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func wantRedirect(t *testing.T, name string, rec *httptest.ResponseRecorder, to string) {
	t.Helper()
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != to {
		t.Errorf("%s: got %d to %q, want 302 to %q", name, rec.Code, rec.Header().Get("Location"), to)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("%s: Cache-Control %q, a phone must never keep a redirect", name, cc)
	}
}

func TestOpenSendsThemToTheApp(t *testing.T) {
	h, _ := newFeatureHandler(t, openConfig)
	cfg, _ := h.store.Get()
	cfg.appByID("cameras").Access = []string{"wife"}
	cfg.appByID("odd").Access = []string{"mom@example.com"}
	wantRedirect(t, "mom to Photos", doOpen(h, "mom@example.com", "/open/photos"), "https://photos.test/")
	wantRedirect(t, "admin to Cameras", doOpen(h, "admin@example.com", "/open/cameras"), "https://nvr.test/")
	wantRedirect(t, "admin to direct-user app", doOpen(h, "admin@example.com", "/open/odd"), "https://odd.test/")
	cfg.appByID("cameras").Disabled = true
	wantRedirect(t, "admin to disabled app", doOpen(h, "admin@example.com", "/open/cameras"), "/")
}

// Whether the app doesn't exist, isn't theirs, or the visitor is unknown,
// the answer is the same, so nothing leaks.
func TestOpenGivesNothingAway(t *testing.T) {
	h, _ := newFeatureHandler(t, openConfig)
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"not theirs":       doOpen(h, "mom@example.com", "/open/cameras"),
		"not theirs (add)": doOpen(h, "mom@example.com", "/open/cameras?add=1"),
		"no such app":      doOpen(h, "mom@example.com", "/open/nope"),
		"stranger":         doOpen(h, "stranger@x.com", "/open/photos"),
		"nobody":           doOpen(h, "", "/open/photos"),
	} {
		wantRedirect(t, name, rec, "/")
	}
	// A login header from outside the container isn't believed.
	req := httptest.NewRequest(http.MethodGet, "/open/photos", nil)
	req.RemoteAddr = "192.168.1.9:5000"
	req.Header.Set("Tailscale-User-Login", "mom@example.com")
	req.SetPathValue("id", "photos")
	rec := httptest.NewRecorder()
	h.open(rec, req)
	wantRedirect(t, "identity from LAN", rec, "/")
}

func TestOpenNeverRedirectsToAnOddAddress(t *testing.T) {
	h, _ := newFeatureHandler(t, openConfig)
	// Config validation now rejects this URL. Keep the handler's independent
	// protection covered with deliberately malformed in-memory settings.
	cfg, _ := h.store.Get()
	cfg.appByID("odd").URL = "javascript:alert(1)"
	wantRedirect(t, "javascript: url", doOpen(h, "mom@example.com", "/open/odd"), "/")
}

func TestOpenGuide(t *testing.T) {
	h, _ := newFeatureHandler(t, openConfig)
	rec := doOpen(h, "mom@example.com", "/open/photos?add=1")
	body := rec.Body.String()
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("guide: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("Location") != "" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("the guide is a page, not a redirect, and not cached")
	}
	// A phone reads the title and icon from the first HTML when it makes the shortcut.
	for _, want := range []string{
		`<title>Photos</title>`,
		`rel="apple-touch-icon" href="/icons/photos/home.png"`,
		`data-mode="guide"`,
		`src="/open.js"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("guide is missing %s", want)
		}
	}
}

// Inside the installed Anchor app the guide can't add the shortcut, and on
// Android it can't even send the person to a browser: any Anchor address that
// isn't typed into a browser opens the installed app again. So the guide has
// to walk them through copying the address and pasting it into the browser.
func TestOpenGuideHasCopyAndPasteSteps(t *testing.T) {
	h, _ := newFeatureHandler(t, openConfig)
	body := doOpen(h, "mom@example.com", "/open/photos?add=1").Body.String()
	for _, want := range []string{
		`id="in-app"`,
		`id="browser-btn"`,
		`id="browser-manual"`,
		`id="browser-url"`,
		`id="copy-address"`,
		`Paste the address`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("guide is missing %s", want)
		}
	}
	// The address must not be a link: tapping it would open the Anchor app again.
	if strings.Contains(body, `<a href="https://`) || strings.Contains(body, `id="browser-url" href`) {
		t.Errorf("the address to paste must be plain text, not a link")
	}
}

// The page scripts find their elements by id. If the page drops or renames
// one, the script stops half way on a phone, where nobody can see the error.
func TestScriptsFindTheirElements(t *testing.T) {
	h, _ := newFeatureHandler(t, openConfig)
	index, err := os.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for script, page := range map[string]string{
		"web/open.js": doOpen(h, "mom@example.com", "/open/photos?add=1").Body.String(),
		"web/app.js":  string(index),
	} {
		src, err := os.ReadFile(script)
		if err != nil {
			t.Fatal(err)
		}
		ids := regexp.MustCompile(`\$\("([\w-]+)"\)`).FindAllStringSubmatch(string(src), -1)
		if len(ids) < 10 {
			t.Fatalf("expected %s to look up its elements by id, found %d", script, len(ids))
		}
		for _, m := range ids {
			if !strings.Contains(page, `id="`+m[1]+`"`) {
				t.Errorf("%s looks for #%s but its page has no such element", script, m[1])
			}
		}
	}
}

func TestOpenGuideUsesAnchorsIconForURLIcons(t *testing.T) {
	h, _ := newFeatureHandler(t, openConfig)
	body := doOpen(h, "mom@example.com", "/open/remote?add=1").Body.String()
	if !strings.Contains(body, `rel="apple-touch-icon" href="/brand/apple-touch-icon.png"`) {
		t.Errorf("an https:// icon isn't fetched by Anchor, so the guide should use Anchor's icon:\n%s", body)
	}
}

func TestOpenGuideEscapesAppNames(t *testing.T) {
	h, _ := newFeatureHandler(t, openConfig)
	body := doOpen(h, "mom@example.com", "/open/"+slug("<b>Bold</b> & Co")+"?add=1").Body.String()
	if strings.Contains(body, "<b>Bold</b>") || !strings.Contains(body, "&lt;b&gt;Bold&lt;/b&gt; &amp; Co") {
		t.Fatalf("app name wasn't escaped:\n%s", body)
	}
}

// homescreen: false hides the guide; shortcuts people already made keep working.
func TestOpenHonoursHomeScreenOff(t *testing.T) {
	h, _ := newFeatureHandler(t, openConfig)
	wantRedirect(t, "guide", doOpen(h, "mom@example.com", "/open/hidden?add=1"), "/")
	wantRedirect(t, "old shortcut", doOpen(h, "mom@example.com", "/open/hidden"), "https://hidden.test/")
}

func TestOpenTellsThemWhenAnAppIsNotWorking(t *testing.T) {
	h, _ := newFeatureHandler(t, openConfig)

	// Maintenance: the admin's message, and no redirect.
	rec := doOpen(h, "mom@example.com", "/open/books")
	if rec.Code != 200 || rec.Header().Get("Location") != "" ||
		!strings.Contains(rec.Body.String(), "Being upgraded, back tonight") ||
		!strings.Contains(rec.Body.String(), `data-mode="gone"`) {
		t.Fatalf("maintenance page: %d\n%s", rec.Code, rec.Body)
	}
	wantRedirect(t, "admin, maintenance", doOpen(h, "admin@example.com", "/open/books"), "https://books.test/")

	// Down: two failed rounds. Photos alerts the admin, Quiet doesn't.
	for _, id := range []string{"photos", "quiet"} {
		app := *h.store.cfg.appByID(id)
		h.checker.set(app, "down", "boom", true)
		h.checker.set(app, "down", "boom", true)
	}
	body := doOpen(h, "mom@example.com", "/open/photos").Body.String()
	if !strings.Contains(body, "Not working right now. Alerts to Alex are enabled.") || strings.Contains(body, "has been told") {
		t.Errorf("down page must describe configured alerts without claiming delivery:\n%s", body)
	}
	quiet := doOpen(h, "mom@example.com", "/open/quiet").Body.String()
	if !strings.Contains(quiet, "Not working right now.") || strings.Contains(quiet, "has been told") {
		t.Errorf("an app with alerts: false mustn't claim anyone was told:\n%s", quiet)
	}
	wantRedirect(t, "admin, down", doOpen(h, "admin@example.com", "/open/photos"), "https://photos.test/")

	// Back up: straight through again.
	h.checker.set(*h.store.cfg.appByID("photos"), "up", "200 OK", false)
	wantRedirect(t, "recovered", doOpen(h, "mom@example.com", "/open/photos"), "https://photos.test/")
}
