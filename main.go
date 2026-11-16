// Anchor is a per-person dashboard for private self-hosted applications.
// SQLite stores settings and accounts; Tailscale and local sessions identify users.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web
var webFiles embed.FS

//go:embed anchor.example.yaml
var exampleConfig []byte

var version = "dev"

// Registered only by the anchordev build. Production binaries have no identity picker.
var developmentIdentities func(http.Handler, *authentication, string) (http.Handler, error)

const shutdownGrace = 25 * time.Second

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Printf("Anchor %s (%s %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		healthcheck()
		return
	}
	configDir := env("ANCHOR_CONFIG_DIR", "/config")
	listen := env("ANCHOR_LISTEN", defaultListen)

	if err := os.MkdirAll(configDir, 0o700); err != nil {
		log.Fatalf("config dir: %v", err)
	}
	if len(os.Args) == 3 && os.Args[1] == "recover-admin" {
		if err := recoverAdministrator(configDir, os.Args[2]); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) == 4 && os.Args[1] == "link-tailscale" {
		if err := linkTailscaleUser(configDir, os.Args[2], os.Args[3]); err != nil {
			log.Fatal(err)
		}
		return
	}
	store, err := newPersistentStore(configDir)
	if err != nil {
		log.Fatalf("%v", err)
	}
	defer store.persistent.db.Close()
	auth, err := newAuthentication(store.persistent)
	if err != nil {
		log.Fatal(err)
	}
	if store.persistent.needsSetup() {
		if err := printSetupInstructions(store.persistent, log.Default(), auth.publicURL); err != nil {
			log.Fatal(err)
		}
	}
	notifier := newNotificationRouter(store)
	checker := NewChecker(store)
	checker.notifier = notifier
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	background, cancelBackground := context.WithCancel(ctx)
	defer cancelBackground()
	work, cancelWork := context.WithCancel(context.Background())
	defer cancelWork()
	var loops, notifications sync.WaitGroup
	checker.send = func(nt Notifier, msg Notification, done func(bool)) {
		notifications.Add(1)
		sendAsyncContext(background, nt, msg, func(sent bool) {
			defer notifications.Done()
			if done != nil {
				done(sent)
			}
		})
	}

	web, _ := fs.Sub(webFiles, "web")
	bundledIcons, _ := fs.Sub(web, "icons")

	h := &handler{auth: auth, store: store, checker: checker, notifier: notifier,
		reports: newRateLimiter(10*time.Minute, 30),
		// Integration requests retain the upstream address protections.
		requests: newRequestStatus(store, requestKeys{}, false),
		reqReads: newRateLimiter(2*time.Second, 3600),
		// A screenful of covers arrives at once, then a steady trickle as the
		// list is scrolled: 60 straight away, then 4 a second.
		posterReads: newBucketLimiter(60, 250*time.Millisecond),
		booksReads:  newRateLimiter(2*time.Second, 3600),
		bookCovers:  newBucketLimiter(60, 250*time.Millisecond),
		bookFiles:   newRateLimiter(time.Second, 1200),
		ebooks:      newEbookService("", false)}
	h.stopping = background.Done()
	h.requests.ctx, h.requests.posters.ctx = work, work
	h.ebooks.ctx, h.ebooks.covers.ctx = work, work
	mux := http.NewServeMux()
	auth.routes(mux)
	h.adminRoutes(mux)
	mux.HandleFunc("GET /api/me", h.me)
	mux.HandleFunc("GET /api/events", h.events)
	mux.HandleFunc("POST /api/report", h.report)
	mux.HandleFunc("GET /api/requests", h.requestsList)
	mux.HandleFunc("POST /api/requests/refresh", h.requestsRefresh)
	mux.HandleFunc("GET /api/requests/poster/{kind}/{id}", h.requestsPoster)
	mux.HandleFunc("GET /api/ebooks", h.ebooksList)
	mux.HandleFunc("POST /api/ebooks/refresh", h.ebooksList)
	mux.HandleFunc("GET /api/ebooks/{id}/cover", h.ebookCover)
	mux.HandleFunc("GET /api/ebooks/{id}/download", h.ebookDownload)
	mux.HandleFunc("GET /open/{id}", h.open)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	iconAllowed := func(name string) bool { cfg, _ := store.Get(); return cfg.usesIcon(name) }
	icons := NewIconServer(configDir, bundledIcons, iconAllowed)
	mux.Handle("GET /icons/{name}", icons)
	mux.HandleFunc("GET /icons/{name}/home.png", icons.ServeHome)
	mux.Handle("GET /", staticHandler(web))

	// The explicit deployment mode supplies the external scheme.
	scheme := "https"
	if auth.mode == "lan" {
		scheme = "http"
	}
	appHandler := auth.middleware(mux)
	if os.Getenv("ANCHOR_DEV_IDENTITIES") != "" {
		if os.Getenv("ANCHOR_DEV_IDENTITIES") != "1" || developmentIdentities == nil {
			log.Fatal("identity simulation requires the development build: use dev.ps1 -SimulateTailscale")
		}
		appHandler, err = developmentIdentities(appHandler, auth, listen)
		if err != nil {
			log.Fatal(err)
		}
		scheme = "http" // The simulator accepts only the exact localhost listener.
	}
	srv := newHTTPServer(blockFunnel(securityHeaders(externalScheme(scheme, appHandler))))
	srv.BaseContext = func(net.Listener) context.Context { return work }
	loops.Add(2)
	go func() { defer loops.Done(); checker.RunContext(background) }()
	go func() {
		defer loops.Done()
		store.WatchContext(background, func() {
			checker.Kick()
			checker.changes.Notify()
		})
	}()
	err = serveContext(ctx, srv, listen, shutdownGrace)
	cancelBackground()
	cancelWork()
	loops.Wait()
	notifications.Wait()
	if err != nil {
		log.Fatalf("server stopped: %v", err)
	}
}

func newHTTPServer(h http.Handler) *http.Server {
	return &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 15 * time.Second, IdleTimeout: time.Minute}
}

// defaultListen is loopback only: Tailscale Serve runs inside the container
// and connects over localhost, so nothing else ever needs to reach Anchor.
// IPv6 loopback is included in case Serve dials "localhost" as ::1; it is
// skipped quietly if the container has no IPv6.
const defaultListen = "127.0.0.1:8080,[::1]:8080"

// serveContext listens on each address, closes live streams on cancellation,
// and gives active requests a bounded drain window before closing sockets.
func serveContext(ctx context.Context, srv *http.Server, listen string, drain time.Duration) error {
	lns, err := listenAll(listen)
	if err != nil {
		return err
	}
	return serveListenersContext(ctx, srv, lns, drain)
}

func serveListenersContext(ctx context.Context, srv *http.Server, lns []net.Listener, drain time.Duration) error {
	var err error
	errs := make(chan error, len(lns))
	for _, ln := range lns {
		log.Printf("Anchor %s listening on %s", version, ln.Addr())
		go func(ln net.Listener) { errs <- srv.Serve(ln) }(ln)
	}
	select {
	case <-ctx.Done():
	case err = <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
	}
	draining, cancel := context.WithTimeout(context.Background(), drain)
	defer cancel()
	if shutdownErr := srv.Shutdown(draining); shutdownErr != nil {
		srv.Close()
		if err == nil {
			err = shutdownErr
		}
	}
	return err
}

func waitContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func listenAll(listen string) ([]net.Listener, error) {
	var addrs []string
	for _, a := range strings.Split(listen, ",") {
		if a = strings.TrimSpace(a); a != "" {
			addrs = append(addrs, a)
		}
	}
	var lns []net.Listener
	for _, addr := range addrs {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			if strings.HasPrefix(addr, "[::1]") && len(addrs) > 1 {
				log.Printf("not listening on %s (no IPv6 in this container): %v", addr, err)
				continue
			}
			for _, l := range lns {
				l.Close()
			}
			return nil, fmt.Errorf("listen on %s: %w", addr, err)
		}
		lns = append(lns, ln)
	}
	if len(lns) == 0 {
		return nil, errors.New("no address to listen on")
	}
	return lns, nil
}

// blockFunnel refuses traffic that Tailscale Funnel brought in from the
// public internet. Anchor is for the tailnet only; if Funnel is ever turned
// on by mistake, this keeps it closed. Tailscale sets this header itself on
// Funnel requests and strips any copy a visitor sends, so it can't be faked
// away.
func blockFunnel(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Tailscale-Funnel-Request") != "" {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "Anchor is configured for private-network access.", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// healthcheck is used by Docker's HEALTHCHECK so the image needs no curl.
func healthcheck() {
	endpoint, err := healthcheckURL(env("ANCHOR_LISTEN", defaultListen))
	if err != nil {
		os.Exit(1)
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(endpoint)
	if err != nil || resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
	resp.Body.Close()
}

func healthcheckURL(listen string) (string, error) {
	first := ""
	for _, address := range strings.Split(listen, ",") {
		if first = strings.TrimSpace(address); first != "" {
			break
		}
	}
	host, port, err := net.SplitHostPort(first)
	if err != nil {
		return "", err
	}
	// Wildcard listeners are reached through loopback in the same family.
	// Explicit bind addresses must be preserved, including IPv6-only listeners.
	if host == "" {
		host = "127.0.0.1"
	} else if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		if ip.To4() != nil {
			host = "127.0.0.1"
		} else {
			host = "::1"
		}
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type handler struct {
	auth              *authentication
	stopping          <-chan struct{} // nil outside the managed server lifecycle
	store             *ConfigStore
	checker           *Checker
	devUser           string
	notifier          Notifier
	reports           *rateLimiter
	notificationTests *rateLimiter
	tailscaleTests    *tailscaleTestRuns
	requests          *requestStatus // the Request status sheet's data
	reqReads          *rateLimiter   // one read per person every 2 seconds
	// posterReads limits covers: a burst, then a trickle, per person.
	posterReads     *bucketLimiter
	booksReads      *rateLimiter
	bookCovers      *bucketLimiter
	bookFiles       *rateLimiter
	ebooks          *ebookService
	integrationMu   sync.Mutex
	requestServices map[string]*requestStatus
	ebookServices   map[string]*ebookService
}

type visitor struct {
	Login   string
	Name    string
	Picture string
}

var wordDecoder = new(mime.WordDecoder)

func decodeHeader(v string) string {
	if d, err := wordDecoder.DecodeHeader(v); err == nil {
		return d
	}
	return v
}

// identify works out who is visiting. Tailscale Serve adds the
// Tailscale-User-* headers (and strips any the visitor tried to send). We
// only trust them on connections from inside the container (loopback),
// which is where Serve connects from, so nobody on the LAN can fake them.
func (h *handler) identify(r *http.Request) visitor {
	if h.auth != nil {
		a := authFrom(r)
		return visitor{Login: a.user.ID, Name: a.user.Name, Picture: a.picture}
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		if login := r.Header.Get("Tailscale-User-Login"); login != "" {
			return visitor{
				Login:   strings.ToLower(decodeHeader(login)),
				Name:    decodeHeader(r.Header.Get("Tailscale-User-Name")),
				Picture: r.Header.Get("Tailscale-User-Profile-Pic"),
			}
		}
	}
	if h.devUser != "" {
		return visitor{Login: h.devUser}
	}
	return visitor{}
}

type apiApp struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	URL          string  `json:"url"`
	Description  string  `json:"description"`
	Icon         string  `json:"icon"`
	Status       string  `json:"status"`
	StatusDetail string  `json:"statusDetail,omitempty"`
	Mobile       *Mobile `json:"mobile,omitempty"`
	EbookLibrary bool    `json:"ebookLibrary,omitempty"`
	Guide        *Guide  `json:"guide,omitempty"`
	Maintenance  string  `json:"maintenance,omitempty"`
	// Locked: the tile can't be opened (marked in maintenance, or down).
	Locked bool `json:"locked"`
	// AdminOpen: locked for everyone else, but this admin can still open
	// it to fix it. Never set while previewing as someone else.
	AdminOpen bool `json:"adminOpen"`
	// Alerts: down alerts are configured. This does not assert publication or
	// human receipt; a notification attempt may fail.
	Alerts bool `json:"alerts"`
	// HomeScreen: offer "Add to home screen" for this app.
	HomeScreen bool `json:"homeScreen"`
	// RequestStatus: this tile gets the "Request status" section.
	RequestStatus bool `json:"requestStatus,omitempty"`
}

type apiNotice struct {
	Message string `json:"message"`
	Style   string `json:"style"`
}

type apiSection struct {
	Name string   `json:"name"`
	Apps []apiApp `json:"apps"`
}

type apiPerson struct {
	Login string `json:"login"`
	Name  string `json:"name"`
}

type apiMe struct {
	Scope       string      `json:"scope"` // opaque config/effective-person revision
	Title       string      `json:"title"`
	Subtitle    string      `json:"subtitle"`
	Contact     string      `json:"contact"`
	Known       bool        `json:"known"`
	Login       string      `json:"login"`
	Name        string      `json:"name"`
	Picture     string      `json:"picture,omitempty"`
	Admin       bool        `json:"admin"`
	ViewingAs   string      `json:"viewingAs,omitempty"`
	People      []apiPerson `json:"people,omitempty"`
	ConfigError string      `json:"configError,omitempty"`
	// CheckProblem: admin-only note that Anchor can't reach any app.
	CheckProblem string       `json:"checkProblem,omitempty"`
	Notice       *apiNotice   `json:"notice,omitempty"`
	Alerts       bool         `json:"alerts"`  // down alerts are configured
	Reports      bool         `json:"reports"` // show the "Not working?" button
	Sections     []apiSection `json:"sections"`
}

func (h *handler) me(w http.ResponseWriter, r *http.Request) {
	cfg, cfgErr := h.store.Get()
	v := h.identify(r)

	out := apiMe{
		Title:    cfg.Title,
		Subtitle: cfg.Subtitle,
		Contact:  cfg.Contact,
		Login:    v.Login,
		Picture:  v.Picture,
		Sections: []apiSection{},
	}
	person, known := cfg.person(v.Login)
	out.Known = known
	out.Name = person.Name
	if out.Name == "" {
		out.Name = v.Name
	}

	// Admins can preview the page as someone else with ?as=email.
	login := v.Login
	if known && cfg.isAdmin(v.Login) {
		out.Admin = true
		out.ConfigError = cfgErr
		out.CheckProblem = h.checker.Problem()
		for email, p := range cfg.People {
			out.People = append(out.People, apiPerson{Login: email, Name: p.Name})
		}
		sort.Slice(out.People, func(i, j int) bool { return out.People[i].Name < out.People[j].Name })
		if as := strings.ToLower(r.URL.Query().Get("as")); as != "" && as != v.Login {
			if p, ok := cfg.People[as]; ok {
				login = as
				out.ViewingAs = p.Name
				if out.ViewingAs == "" {
					out.ViewingAs = as
				}
			}
		}
	}

	if cfgErr != "" && len(cfg.People) == 0 {
		// Nothing usable loaded yet: say so to everyone.
		out.ConfigError = cfgErr
	}
	if known {
		// While previewing, locked tiles stay locked, exactly as that
		// person sees them.
		out.Sections = h.sectionsFor(cfg, login, out.Admin, out.Admin && out.ViewingAs == "")
		if n := cfg.noticeFor(login); n != nil {
			style := n.Style
			if style == "" {
				style = "info"
			}
			out.Notice = &apiNotice{Message: n.Message, Style: style}
		}
		out.Alerts = cfg.alertsEnabled()
		out.Reports = cfg.reportsEnabled()
	}
	out.Scope = fmt.Sprintf("%x", sha256.Sum256([]byte(cfg.scope+"\n"+v.Login+"\n"+login)))

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *handler) sectionsFor(cfg *Config, login string, admin, openLocked bool) []apiSection {
	order := map[string]int{}
	var sections []apiSection
	add := func(name string) int {
		if i, ok := order[strings.ToLower(name)]; ok {
			return i
		}
		order[strings.ToLower(name)] = len(sections)
		sections = append(sections, apiSection{Name: name})
		return len(sections) - 1
	}
	for _, s := range cfg.Sections {
		add(s)
	}
	for i := range cfg.Apps {
		a := &cfg.Apps[i]
		if !cfg.visibleTo(a, login) {
			continue
		}
		st := h.checker.Get(a.ID)
		icon := a.Icon
		if !strings.HasPrefix(icon, "http://") && !strings.HasPrefix(icon, "https://") {
			icon = "/icons/" + icon
		}
		locked := appLocked(a, st)
		app := apiApp{
			ID:           a.ID,
			Name:         a.Name,
			URL:          a.URL,
			Description:  a.Description,
			Icon:         icon,
			Status:       st.State,
			Mobile:       a.Mobile,
			EbookLibrary: cfg.ebookFor(a.ID) != nil,
			Guide:        a.Guide,
			Maintenance:  a.Maintenance,
			Locked:       locked,
			AdminOpen:    locked && openLocked,
			Alerts:       cfg.alertsEnabled() && a.alertsOn(),
			HomeScreen:   a.homeScreenOn(),

			RequestStatus: cfg.requestFor(a.ID) != nil,
		}
		if admin {
			app.StatusDetail = st.Detail
		}
		idx := add(a.Section)
		sections[idx].Apps = append(sections[idx].Apps, app)
	}
	// Drop empty sections.
	out := sections[:0]
	for _, s := range sections {
		if len(s.Apps) > 0 {
			out = append(out, s)
		}
	}
	return out
}

// revalidated are the files that must never be a stale copy. The page and its
// code change together, so a phone must not run new HTML with old scripts.
var revalidated = map[string]bool{
	"/": true, "/index.html": true, "/sw.js": true,
	"/app.js": true, "/request-view.js": true, "/open.js": true, "/style.css": true,
	"/account.html": true, "/account.js": true, "/session.js": true,
	"/manage.html": true, "/manage.js": true, "/app-order.js": true, "/admin.css": true,
	"/tailscale-test.html": true, "/tailscale-test.js": true, "/tailscale-test-view.js": true,
	"/confirm.js": true, "/avatar.js": true,
}

func staticHandler(web fs.FS) http.Handler {
	files := http.FileServerFS(web)
	// Cache identity follows the actual embedded files, including worker policy,
	// even in local builds whose application version is still "dev".
	hash := sha256.New()
	for _, path := range []string{"index.html", "style.css", "request-view.js", "app.js", "avatar.js", "session.js", "account.html", "account.js", "manage.html", "manage.js", "app-order.js", "admin.css", "manifest.webmanifest", "brand/anchor.svg", "sw.js"} {
		body, _ := fs.ReadFile(web, path)
		fmt.Fprintf(hash, "%s\x00%d\x00", path, len(body))
		hash.Write(body)
	}
	assetVersion := fmt.Sprintf("%x", hash.Sum(nil))
	worker, workerErr := fs.ReadFile(web, "sw.js")
	worker = bytes.ReplaceAll(worker, []byte("__ANCHOR_SHELL_VERSION__"), []byte(assetVersion))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No folder listings (e.g. /icons/ or /brand/).
		if r.URL.Path != "/" && strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		switch {
		case revalidated[r.URL.Path]:
			w.Header().Set("Cache-Control", "no-cache")
		case strings.HasSuffix(r.URL.Path, ".webmanifest"):
			w.Header().Set("Content-Type", "application/manifest+json")
		default:
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		// Old builds may have given these assets a long browser cache lifetime.
		// Content-derived query versions make a rebuilt editor load fresh code
		// even when that old response is still cached on a returning browser.
		if r.URL.Path == "/account.html" || r.URL.Path == "/manage.html" {
			path := strings.TrimPrefix(r.URL.Path, "/")
			if body, err := fs.ReadFile(web, path); err == nil {
				for _, asset := range []string{"session.js", "account.js", "manage.js", "app-order.js", "admin.css"} {
					body = bytes.ReplaceAll(body, []byte("/"+asset+"\""), []byte("/"+asset+"?v="+assetVersion+"\""))
				}
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				http.ServeContent(w, r, path, time.Time{}, bytes.NewReader(body))
				return
			}
		}
		if r.URL.Path == "/sw.js" && workerErr == nil {
			w.Header().Set("Content-Type", "application/javascript")
			http.ServeContent(w, r, "sw.js", time.Time{}, bytes.NewReader(worker))
			return
		}
		files.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("X-Frame-Options", "DENY")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			// Everything under /api/ depends on who is asking and when, and
			// this also covers answers the router itself gives (404, 405).
			hd.Set("Cache-Control", "no-store")
		}
		if !strings.HasPrefix(r.URL.Path, "/icons/") {
			hd.Set("Content-Security-Policy",
				"default-src 'self'; img-src 'self' https: data:; style-src 'self'; script-src 'self'; "+
					"connect-src 'self'; manifest-src 'self'; worker-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		}
		next.ServeHTTP(w, r)
	})
}
