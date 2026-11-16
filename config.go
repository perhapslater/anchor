package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
)

// Config is the dashboard settings projection used by SQLite persistence and
// schema-versioned YAML import/export. The legacy file parser also uses it.
type Config struct {
	credentials   map[string]string
	Title         string            `yaml:"title"`
	Subtitle      string            `yaml:"subtitle"`
	Contact       string            `yaml:"contact"`
	CheckInterval string            `yaml:"check_interval"`
	Sections      []string          `yaml:"sections"`
	People        map[string]Person `yaml:"people"`
	Apps          []App             `yaml:"apps"`
	// Notice is an optional banner at the top of the page.
	Notice *Notice `yaml:"notice"`
	// Notify is optional: alerts and problem reports sent through ntfy.
	Notify  *Notify        `yaml:"notify"`
	Webhook *WebhookConfig `yaml:"webhook,omitempty"`
	// Requests is optional: the "Request status" section on one app's tile.
	Requests *Requests `yaml:"requests"`
	// Ebooks enables the EPUB catalog on one app's tile.
	Ebooks *EbooksConfig `yaml:"ebooks"`
	// Each connection belongs to a tile. The singular fields remain compatible
	// with existing installations and YAML exports.
	RequestConnections []Requests     `yaml:"request_connections,omitempty"`
	EbookLibraries     []EbooksConfig `yaml:"ebook_libraries,omitempty"`

	interval time.Duration
	scope    string // opaque revision used to clear browser data after a valid reload
}

// Requests turns on the "Request status" section of one tile (normally
// Seerr's). The addresses are where Anchor looks things up; API credentials
// are managed separately in Administration and loaded from SQLite into the
// private runtime projection. Anchor only ever contacts these four addresses.
type Requests struct {
	scoped   bool
	revision string
	keys     *requestKeys
	App      string `yaml:"app"` // id of the tile that gets the section
	Seerr    string `yaml:"seerr"`
	Plex     string `yaml:"plex"`
	Sonarr   string `yaml:"sonarr"` // optional
	Radarr   string `yaml:"radarr"` // optional
}

// EbooksConfig connects the family-facing ebook tile to one Audiobookshelf library.
type EbooksConfig struct {
	scoped     bool
	secret     string
	secretMode bool
	App        string `yaml:"app"`
	Server     string `yaml:"server"`
	LibraryID  string `yaml:"library_id"`
}

// sig changes whenever any setting does, so Anchor drops what it looked up
// under the old settings.
func (r *Requests) sig() string {
	return strings.Join([]string{r.App, r.Seerr, r.Plex, r.Sonarr, r.Radarr, r.revision}, "\n")
}

// validUpstream checks an address Anchor will call and returns it without a
// trailing slash. Only http or https, a host, and nothing that could smuggle
// in credentials or a different request (user info, query, fragment).
func validUpstream(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || strings.ContainsAny(raw, "?# \t\r\n") {
		return "", errors.New("should look like http://192.168.1.10:5055")
	}
	return strings.TrimRight(raw, "/"), nil
}

// Notice is the optional announcement banner. YAML imports accept a plain
// string or a map with message/style/access.
type Notice struct {
	Message string   `yaml:"message"`
	Style   string   `yaml:"style"`  // "info" (default) or "warning"
	Access  []string `yaml:"access"` // who sees it; empty means everyone
}

func (n *Notice) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		if value.Tag != "!!str" {
			return errors.New("notice should be text or a message/style/access map")
		}
		n.Message = value.Value
		return nil
	}
	if value.Kind != yaml.MappingNode {
		return errors.New("notice should be text or a message/style/access map")
	}
	for i := 0; i < len(value.Content); i += 2 {
		switch value.Content[i].Value {
		case "message", "style", "access":
		default:
			return fmt.Errorf("unknown notice field %q", value.Content[i].Value)
		}
	}
	type plain Notice
	var p plain
	if err := value.Decode(&p); err != nil {
		return err
	}
	*n = Notice(p)
	return nil
}

// Notify sends messages to an ntfy topic. The access token is managed separately
// in Administration and loaded from SQLite, never included in dashboard YAML.
type Notify struct {
	Server  string `yaml:"server"`
	Topic   string `yaml:"topic"`
	Alerts  *bool  `yaml:"alerts"`  // down/recovered alerts; default true
	Reports *bool  `yaml:"reports"` // "Not working?" button; default true
}

func (n *Notify) alertsOn() bool  { return n != nil && (n.Alerts == nil || *n.Alerts) }
func (n *Notify) reportsOn() bool { return n != nil && (n.Reports == nil || *n.Reports) }

// The endpoint is a credential in SQLite, never part of this exportable config.
type WebhookConfig struct {
	Format  string `yaml:"format"`
	Alerts  *bool  `yaml:"alerts"`
	Reports *bool  `yaml:"reports"`
}

func (c *Config) alertsEnabled() bool {
	return c.Notify.alertsOn() || c.Webhook != nil && (c.Webhook.Alerts == nil || *c.Webhook.Alerts)
}
func (c *Config) reportsEnabled() bool {
	return c.Notify.reportsOn() || c.Webhook != nil && (c.Webhook.Reports == nil || *c.Webhook.Reports)
}

var ntfyTopic = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var absLibraryID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Person is an enabled account's permission projection, keyed by stable user
// ID in the running application (login email in legacy parser fixtures).
type Person struct {
	Name   string   `yaml:"name"`
	Groups []string `yaml:"groups"`
}

// App is one tile on the dashboard.
type App struct {
	Disabled    bool     `yaml:"disabled,omitempty"`
	ID          string   `yaml:"id"`
	Name        string   `yaml:"name"`
	URL         string   `yaml:"url"`
	Description string   `yaml:"description"`
	Section     string   `yaml:"section"`
	Icon        string   `yaml:"icon"`
	Access      []string `yaml:"access"`
	// Check overrides the URL used for the up/down dot. "off" disables it.
	Check  string  `yaml:"check"`
	Mobile *Mobile `yaml:"mobile"`
	// Guide makes this tile open an instruction sheet before following its URL.
	Guide *Guide `yaml:"guide"`
	// Maintenance, when set, greys out the tile with this message.
	Maintenance string `yaml:"maintenance"`
	// Alerts: false stops down/recovered alerts for this app.
	Alerts *bool `yaml:"alerts"`
	// HomeScreen: false hides "Add to home screen" for this app.
	HomeScreen *bool `yaml:"homescreen"`
}

func (a *App) alertsOn() bool     { return a.Maintenance == "" && (a.Alerts == nil || *a.Alerts) }
func (a *App) homeScreenOn() bool { return a.HomeScreen == nil || *a.HomeScreen }

// Mobile describes the "Get the app" helper for an app.
type Mobile struct {
	IOS        string `yaml:"ios" json:"ios,omitempty"`
	Android    string `yaml:"android" json:"android,omitempty"`
	IOSApp     string `yaml:"ios_app" json:"iosApp,omitempty"`
	AndroidApp string `yaml:"android_app" json:"androidApp,omitempty"`
	OpenLabel  string `yaml:"open_label" json:"openLabel,omitempty"`
	Server     string `yaml:"server" json:"server"`
	Note       string `yaml:"note" json:"note,omitempty"`
}

// Guide is a short, optional set of instructions shown from an app tile.
type Guide struct {
	Intro       string   `yaml:"intro" json:"intro,omitempty"`
	Steps       []string `yaml:"steps" json:"steps,omitempty"`
	IOS         []string `yaml:"ios" json:"ios,omitempty"`
	Android     []string `yaml:"android" json:"android,omitempty"`
	ActionLabel string   `yaml:"action_label" json:"actionLabel,omitempty"`
	Note        string   `yaml:"note" json:"note,omitempty"`
}

var idCleaner = regexp.MustCompile(`[^a-z0-9]+`)
var appID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func validWebURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") &&
		u.Hostname() != "" && u.User == nil && strings.IndexFunc(raw, unicode.IsSpace) < 0 &&
		strings.IndexFunc(raw, unicode.IsControl) < 0
}

// App links are administrator-authored. Custom native schemes and Android
// intents are supported, but browser script/document and local-file URLs are not.
func validAppLaunch(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || strings.IndexFunc(raw, unicode.IsSpace) >= 0 || strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "javascript", "data", "vbscript", "file", "blob", "about", "filesystem":
		return false
	case "http", "https":
		return validWebURL(raw)
	}
	return true
}

func slug(s string) string {
	return strings.Trim(idCleaner.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// parseConfig validates settings persisted as YAML in SQLite and YAML import
// documents, as well as the legacy file-based parser fixtures.
func parseConfig(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("anchor.yaml could not be read: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("anchor.yaml must contain exactly one YAML document")
	}
	c.scope = fmt.Sprintf("%x", sha256.Sum256(data))
	if c.Title == "" {
		c.Title = "Anchor"
	}
	if c.Contact == "" {
		c.Contact = "the person who runs this"
	}
	c.interval = 60 * time.Second
	if c.CheckInterval != "" {
		d, err := time.ParseDuration(c.CheckInterval)
		if err != nil || d < 10*time.Second {
			return nil, fmt.Errorf("check_interval %q should look like 60s or 2m (minimum 10s)", c.CheckInterval)
		}
		c.interval = d
	}

	var problems []string
	// Normalise logins, but never let a blank key become the anonymous identity
	// or let two spellings silently overwrite one person's permissions.
	people := make(map[string]Person, len(c.People))
	for email, p := range c.People {
		login := strings.ToLower(strings.TrimSpace(email))
		if login == "" {
			problems = append(problems, "people contains an empty login")
			continue
		}
		if _, exists := people[login]; exists {
			problems = append(problems, fmt.Sprintf("people contains duplicate normalized login %q", login))
			continue
		}
		people[login] = p
	}
	c.People = people

	seen := map[string]bool{}
	for i := range c.Apps {
		a := &c.Apps[i]
		if a.Name == "" {
			problems = append(problems, fmt.Sprintf("app #%d has no name", i+1))
			continue
		}
		if !validWebURL(a.URL) {
			problems = append(problems, fmt.Sprintf("%s url must be an absolute http or https address without credentials", a.Name))
		}
		if a.Check != "" && !strings.EqualFold(a.Check, "off") && !validWebURL(a.Check) {
			problems = append(problems, fmt.Sprintf("%s check must be off or an absolute http or https address without credentials", a.Name))
		}
		if len(a.Access) == 0 {
			problems = append(problems, fmt.Sprintf("%s has no access list; use admin for administrators only, or choose other people or groups", a.Name))
		}
		if a.ID == "" {
			a.ID = slug(a.Name)
		}
		if !appID.MatchString(a.ID) {
			problems = append(problems, fmt.Sprintf("%s id must contain lowercase letters, numbers, hyphens or underscores (1Ã¢â‚¬â€œ64 characters)", a.Name))
		}
		if seen[a.ID] {
			problems = append(problems, fmt.Sprintf("two apps are both called %q", a.Name))
		}
		seen[a.ID] = true
		if a.Section == "" {
			a.Section = "Apps"
		}
		if a.Icon == "" {
			a.Icon = defaultServiceIcon(a.Name)
			if a.Icon == "" {
				a.Icon = a.ID
			}
		}
		if !iconName.MatchString(a.Icon) && !validWebURL(a.Icon) {
			problems = append(problems, fmt.Sprintf("%s icon must be a simple icon name or an absolute http or https image address", a.Name))
		}
		if a.Mobile != nil && a.Mobile.Server == "" {
			// The phone app wants the server's address, not the page the
			// tile opens (which may be a login link like
			// /auth/login?autoLaunch=1), so use just scheme://host.
			if u, err := url.Parse(a.URL); err == nil && u.Host != "" {
				a.Mobile.Server = u.Scheme + "://" + u.Host
			} else {
				a.Mobile.Server = strings.TrimRight(a.URL, "/")
			}
		}
		if m := a.Mobile; m != nil {
			for _, raw := range []string{m.IOS, m.Android, m.Server} {
				if raw != "" && !validWebURL(raw) {
					problems = append(problems, fmt.Sprintf("%s mobile store/server addresses must use http or https without credentials", a.Name))
				}
			}
			for _, raw := range []string{m.IOSApp, m.AndroidApp} {
				if raw != "" && !validAppLaunch(raw) {
					problems = append(problems, fmt.Sprintf("%s mobile app link must use an app, http, https or intent scheme; executable and local-file schemes are refused", a.Name))
				}
			}
		}
	}
	if c.Notice != nil {
		c.Notice.Message = strings.TrimSpace(c.Notice.Message)
		switch c.Notice.Style {
		case "", "info", "warning":
		default:
			problems = append(problems, fmt.Sprintf("notice style %q should be info or warning", c.Notice.Style))
		}
		if c.Notice.Message == "" {
			c.Notice = nil // an empty notice just means "no banner"
		}
	}
	for i := range c.RequestConnections {
		c.RequestConnections[i].scoped = true
	}
	for i := range c.EbookLibraries {
		c.EbookLibraries[i].scoped = true
	}
	requestApps, ebookApps := map[string]bool{}, map[string]bool{}
	for _, r := range c.requestConnections() {
		r.App = strings.ToLower(strings.TrimSpace(r.App))
		if requestApps[r.App] {
			problems = append(problems, "only one request connection can belong to a tile")
		}
		requestApps[r.App] = true
		if r.App == "" || !seen[r.App] {
			problems = append(problems, fmt.Sprintf("requests app %q is not an app in this file (use the app's id, such as seerr)", r.App))
		}
		for _, f := range []struct {
			name     string
			addr     *string
			required bool
		}{{"seerr", &r.Seerr, true}, {"plex", &r.Plex, true}, {"sonarr", &r.Sonarr, false}, {"radarr", &r.Radarr, false}} {
			if *f.addr == "" && !f.required {
				continue
			}
			clean, err := validUpstream(*f.addr)
			if err != nil {
				problems = append(problems, fmt.Sprintf("requests %s %s", f.name, err))
				continue
			}
			*f.addr = clean
		}
	}
	for _, e := range c.ebookLibraries() {
		e.App = strings.ToLower(strings.TrimSpace(e.App))
		if ebookApps[e.App] {
			problems = append(problems, "only one ebook library can belong to a tile")
		}
		ebookApps[e.App] = true
		if e.App == "" || !seen[e.App] {
			problems = append(problems, fmt.Sprintf("ebooks app %q is not an app in this file (use the app's id, such as ebooks)", e.App))
		}
		clean, err := validUpstream(e.Server)
		if err != nil {
			problems = append(problems, fmt.Sprintf("ebooks server %s", err))
		} else {
			e.Server = clean
		}
		e.LibraryID = strings.TrimSpace(e.LibraryID)
		if !absLibraryID.MatchString(e.LibraryID) {
			problems = append(problems, "ebooks library_id should be the Audiobookshelf library ID")
		}
	}
	if n := c.Notify; n != nil {
		clean, err := validUpstream(n.Server)
		if err != nil {
			problems = append(problems, "notify server should look like https://ntfy.your-tailnet.ts.net")
		} else {
			n.Server = clean
		}
		if !ntfyTopic.MatchString(n.Topic) {
			problems = append(problems, "notify topic should be letters, numbers, - or _ (for example anchor_alert)")
		}
	}
	if c.Webhook != nil && c.Webhook.Format != "discord" && c.Webhook.Format != "json" {
		problems = append(problems, "webhook format must be discord or json")
	}
	if len(problems) > 0 {
		return nil, errors.New("anchor.yaml has problems: " + strings.Join(problems, "; "))
	}
	return &c, nil
}

// visibleTo gives administrators access to every enabled app. Other accounts
// need an Access entry: "everyone", a stable user ID, or a group name.
func (c *Config) visibleTo(a *App, login string) bool {
	return !a.Disabled && (c.isAdmin(login) || c.allowed(a.Access, login))
}

// person keeps unidentified visitors outside every permission boundary, even
// if an invalid config is constructed without going through parseConfig.
func (c *Config) person(login string) (Person, bool) {
	if strings.TrimSpace(login) == "" {
		return Person{}, false
	}
	p, ok := c.People[login]
	return p, ok
}

// noticeFor returns the banner this person should see, if any.
func (c *Config) noticeFor(login string) *Notice {
	if c.Notice == nil {
		return nil
	}
	if _, ok := c.person(login); !ok {
		return nil
	}
	if len(c.Notice.Access) == 0 || c.allowed(c.Notice.Access, login) {
		return c.Notice
	}
	return nil
}

func (c *Config) appByID(id string) *App {
	for i := range c.Apps {
		if c.Apps[i].ID == id {
			return &c.Apps[i]
		}
	}
	return nil
}

func (c *Config) allowed(access []string, login string) bool {
	p, ok := c.person(login)
	if !ok {
		return false
	}
	for _, rule := range access {
		rule = strings.ToLower(strings.TrimSpace(rule))
		if rule == "everyone" || rule == login {
			return true
		}
		for _, g := range p.Groups {
			if strings.EqualFold(g, rule) {
				return true
			}
		}
	}
	return false
}

// usesIcon reports whether some app in the config uses this icon name.
func (c *Config) usesIcon(name string) bool {
	for i := range c.Apps {
		if c.Apps[i].Icon == name {
			return true
		}
	}
	return false
}

func (c *Config) isAdmin(login string) bool {
	p, ok := c.person(login)
	if !ok {
		return false
	}
	for _, g := range p.Groups {
		if strings.EqualFold(g, "admin") {
			return true
		}
	}
	return false
}

// ConfigStore holds the immutable runtime settings projection. SQLite-backed
// writes refresh it directly; legacy file stores poll their source file.
type ConfigStore struct {
	persistent *persistentState
	path       string

	mu      sync.RWMutex
	cfg     *Config
	lastErr string
	modTime time.Time
	size    int64
	fileErr bool
}

func NewConfigStore(path string) (*ConfigStore, error) {
	s := &ConfigStore{path: path}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		log.Printf("no config at %s, writing a starter one", path)
		if err := os.WriteFile(path, exampleConfig, 0o644); err != nil {
			return nil, fmt.Errorf("writing starter config: %w", err)
		}
	}
	if err := s.reload(); err != nil {
		// Keep running with an empty config so that fixing the file is
		// picked up automatically instead of the container crash-looping.
		log.Printf("ERROR %v", err)
		s.mu.Lock()
		s.cfg = &Config{Title: "Anchor", Contact: "the person who runs this", interval: 60 * time.Second}
		s.lastErr = err.Error()
		s.mu.Unlock()
	}
	return s, nil
}

func (s *ConfigStore) Get() (*Config, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg, s.lastErr
}

func (s *ConfigStore) reload() error {
	st, err := os.Stat(s.path)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	cfg, perr := parseConfig(data)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.modTime, s.size = st.ModTime(), st.Size()
	s.fileErr = false
	if perr != nil {
		s.lastErr = perr.Error()
		if s.cfg == nil {
			return perr
		}
		log.Printf("config not reloaded, keeping the previous one: %v", perr)
		return nil
	}
	s.cfg, s.lastErr = cfg, ""
	log.Printf("config loaded: %d apps, %d people", len(cfg.Apps), len(cfg.People))
	return nil
}

// Watch polls the file. Polling (rather than inotify) is reliable on
// Unraid's user shares.
func (s *ConfigStore) Watch(onChange func()) {
	s.WatchContext(context.Background(), onChange)
}

func (s *ConfigStore) WatchContext(ctx context.Context, onChange func()) {
	if s.persistent != nil {
		<-ctx.Done()
		return
	}
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if s.poll() {
				onChange()
			}
		}
	}
}

func (s *ConfigStore) poll() bool {
	st, err := os.Stat(s.path)
	if err != nil {
		return s.recordFileError(err)
	}
	s.mu.RLock()
	changed := s.fileErr || !st.ModTime().Equal(s.modTime) || st.Size() != s.size
	s.mu.RUnlock()
	if !changed {
		return false
	}
	if err := s.reload(); err != nil {
		return s.recordFileError(err)
	}
	return true
}

func (s *ConfigStore) recordFileError(err error) bool {
	message := "anchor.yaml could not be read; check the config share and file permissions"
	if errors.Is(err, os.ErrNotExist) {
		message = "anchor.yaml is missing; keeping the last good settings"
	}
	s.mu.Lock()
	changed := !s.fileErr || s.lastErr != message
	s.fileErr, s.lastErr = true, message
	s.mu.Unlock()
	if changed {
		log.Printf("config not reloaded: %s", message)
	}
	return changed
}
