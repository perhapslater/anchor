package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Status struct {
	State    string    `json:"state"` // up, down, unknown
	Detail   string    `json:"detail,omitempty"`
	Checked  time.Time `json:"checked"`
	fails    int
	delivery alertDelivery
}

// Checker polls every app in the background so page loads stay instant.
type Checker struct {
	ctx      context.Context // assigned before RunContext starts; also cancels probes
	store    *ConfigStore
	client   *http.Client
	notifier Notifier // nil: no alerts
	send     func(Notifier, Notification, func(bool))
	now      func() time.Time

	// probe checks one app. It's a field so tests can fake the network.
	probe func(App) probeResult

	// changes tells open pages when a dot or the admin note changes.
	changes *Changes
	// recheck is how often apps shown as down are checked between full
	// rounds, so a recovered app turns green within seconds.
	recheck time.Duration

	mu     sync.RWMutex
	status map[string]*Status
	kick   chan struct{}

	// "Anchor can't reach anything" tracking (see round).
	allDownRounds   int
	allDownDelivery alertDelivery
	problem         string
}

// probeResult is the outcome of checking one app once.
type probeResult struct {
	state   string // up, down, unknown
	detail  string
	failure bool // counts toward turning the app red
	dns     bool // Anchor couldn't even look up the name
	skipped bool // checks are off or the check URL is invalid
}

func NewChecker(store *ConfigStore) *Checker {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	// Private LAN/tailnet services and owned loopback remain supported, while
	// the actual resolved socket address cannot target restricted networks.
	tr.DialContext = (&net.Dialer{Timeout: 5 * time.Second, Control: guardAddr(true)}).DialContext
	tr.ResponseHeaderTimeout = 8 * time.Second
	// Open a fresh connection for every check instead of reusing one.
	// A kept-alive connection across the tailnet can silently die when
	// Tailscale changes paths (e.g. relay -> direct), and Go would keep
	// sending checks down the dead HTTP/2 connection until they time out,
	// showing a working app as "Not responding". A handful of new
	// connections a minute costs nothing.
	forceHTTP1(tr)
	c := &Checker{
		ctx:   context.Background(),
		store: store,
		send:  sendAsync,
		now:   time.Now,
		client: &http.Client{
			Transport: tr,
			Timeout:   10 * time.Second,
			// A redirect (e.g. to a login page) already proves the app is up.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		status:  map[string]*Status{},
		kick:    make(chan struct{}, 1),
		changes: NewChanges(),
		recheck: 10 * time.Second,
	}
	c.probe = c.probeHTTP
	return c
}

// Problem is non-empty while Anchor can't reach any app (admins see it).
func (c *Checker) Problem() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.problem
}

func (c *Checker) Get(id string) Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if s, ok := c.status[id]; ok {
		return *s
	}
	return Status{State: "unknown"}
}

// Kick triggers an immediate round, e.g. after the config changes.
func (c *Checker) Kick() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

func (c *Checker) Run() {
	c.RunContext(context.Background())
}

func (c *Checker) RunContext(ctx context.Context) {
	c.ctx = ctx
	for ctx.Err() == nil {
		cfg, _ := c.store.Get()
		c.round(cfg)
		c.waitForNextRoundContext(ctx, cfg.interval)
	}
}

// waitForNextRound waits until the next full round is due, or a Kick.
// Meanwhile, apps shown as down are checked again every few seconds, so a
// recovered app doesn't stay red for up to a whole interval.
func (c *Checker) waitForNextRound(interval time.Duration) {
	c.waitForNextRoundContext(context.Background(), interval)
}

func (c *Checker) waitForNextRoundContext(ctx context.Context, interval time.Duration) {
	next := time.NewTimer(interval)
	defer next.Stop()
	tick := time.NewTicker(c.recheck)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-next.C:
			return
		case <-c.kick:
			return
		case <-tick.C:
			cfg, _ := c.store.Get()
			c.recheckDown(cfg)
		}
	}
}

// recheckDown checks only the apps shown as down. Apps with a single
// failure wait for the next full round as before, so turning red still
// takes two failed rounds and a quick restart doesn't flash red.
func (c *Checker) recheckDown(cfg *Config) {
	var apps []App
	c.mu.RLock()
	for _, a := range cfg.Apps {
		if s, ok := c.status[a.ID]; ok && s.State == "down" {
			apps = append(apps, a)
		}
	}
	c.mu.RUnlock()

	results := make([]probeResult, len(apps))
	var wg sync.WaitGroup
	for i := range apps {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = c.probe(apps[i])
		}(i)
	}
	wg.Wait()

	changed := false
	for i, r := range results {
		if c.set(apps[i], r.state, r.detail, r.failure) {
			changed = true
		}
	}
	if changed {
		c.changes.Notify()
	}
}

// round checks every app, then decides what the results mean.
//
// If every checked app fails at the same moment, the problem is almost
// certainly on Anchor's side (its network, its Tailscale, a bug) or
// everything restarted at once. Showing every app as down would lock the
// family out of apps that work fine, so instead: grey dots, nothing
// locked, and one "Anchor can't reach any apps" alert to the admin
// (after two rounds in a row, so a blip doesn't page anyone).
func (c *Checker) round(cfg *Config) {
	results := make([]probeResult, len(cfg.Apps))
	var wg sync.WaitGroup
	for i := range cfg.Apps {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = c.probe(cfg.Apps[i])
		}(i)
	}
	wg.Wait()

	checked, failed := 0, 0
	var sample string
	for _, r := range results {
		if r.skipped {
			continue
		}
		checked++
		if r.failure || r.dns {
			failed++
			if sample == "" {
				sample = r.detail
			}
		}
	}
	allDown := checked >= 2 && failed == checked

	var alert *Notification
	var published func(bool)
	c.mu.Lock()
	oldProblem := c.problem
	if allDown {
		c.allDownRounds++
		c.problem = "Anchor can't reach any of your apps right now, so the dots are paused and nothing is locked. " +
			"Usually this is Anchor's side (its network or Tailscale), or everything restarted at once. First error: " + sample
		if c.allDownRounds >= 2 && c.alertsOn(cfg) && c.allDownDelivery.start(c.now()) {
			published = c.deliveryResult(&c.allDownDelivery)
			alert = &Notification{
				Title:    "Anchor can't reach any apps",
				delivery: c.allDownDelivery.receipts,
				Message:  "Every check is failing at once, so this is probably Anchor's side (network or Tailscale) or everything restarted. Tiles are NOT locked for the family.\n" + sample,
				Priority: 4,
				Tags:     []string{"warning"},
			}
		}
	} else {
		if (c.allDownDelivery.sent || c.allDownDelivery.pending || c.allDownDelivery.receipts.anySent()) && c.alertsOn(cfg) {
			alert = &Notification{
				Title:    "Anchor can reach your apps again",
				Message:  "Checks are working again.",
				Priority: 3,
				Tags:     []string{"white_check_mark"},
			}
		}
		c.allDownRounds, c.problem = 0, ""
		c.allDownDelivery.reset()
	}
	changed := c.problem != oldProblem
	c.mu.Unlock()
	if alert != nil {
		log.Printf("alert: %s", alert.Title)
		c.send(c.notifier, *alert, published)
	}

	for i, r := range results {
		a := cfg.Apps[i]
		if allDown && !r.skipped {
			if c.pause(a, "Paused: Anchor can't reach any apps right now. "+r.detail) {
				changed = true
			}
			continue
		}
		if c.set(a, r.state, r.detail, r.failure) {
			changed = true
		}
	}
	if changed {
		c.changes.Notify()
	}
}

func (c *Checker) alertsOn(cfg *Config) bool {
	return c.notifier != nil && cfg.alertsEnabled()
}

// Called under c.mu; only the matching outage attempt may update delivery.
func (c *Checker) deliveryResult(d *alertDelivery) func(bool) {
	generation := d.generation
	return func(sent bool) {
		c.mu.Lock()
		if d.generation == generation {
			d.pending, d.sent = false, sent
		}
		c.mu.Unlock()
	}
}

// pause shows an app as unknown (grey, not locked) without touching its
// failure count or alert state, so nothing is lost when Anchor recovers.
// It reports whether the dot changed.
func (c *Checker) pause(a App, detail string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.status[a.ID]
	if !ok {
		s = &Status{}
		c.status[a.ID] = s
	}
	was := s.State
	s.State, s.Detail, s.Checked = "unknown", detail, time.Now()
	return s.State != was
}

// checkOne checks a single app on its own (used by tests).
func (c *Checker) checkOne(a App) {
	r := c.probe(a)
	c.set(a, r.state, r.detail, r.failure)
}

func (c *Checker) probeHTTP(a App) probeResult {
	if a.Disabled {
		return probeResult{state: "unknown", detail: "tile disabled", skipped: true}
	}
	target := a.Check
	if target == "" {
		target = a.URL
	}
	if strings.EqualFold(target, "off") {
		return probeResult{state: "unknown", detail: "checks turned off", skipped: true}
	}
	// Go may rewrite an unspecified literal to loopback before the socket
	// control hook sees it. Reject the original spelling as well.
	if parsed, err := url.Parse(target); err == nil {
		if ip := net.ParseIP(parsed.Hostname()); ip != nil && ip.IsUnspecified() {
			return probeResult{state: "down", detail: "address not allowed", failure: true}
		}
	}

	ctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return probeResult{state: "unknown", detail: "bad check url: " + err.Error(), skipped: true}
	}
	req.Header.Set("User-Agent", "Anchor-status-check")
	resp, err := c.client.Do(req)
	if err != nil && !isDNSError(err) && c.ctx.Err() == nil {
		// One quick retry on a brand-new connection before counting a failure.
		if !waitContext(c.ctx, 2*time.Second) {
			return probeResult{state: "unknown", skipped: true}
		}
		retry, rerr := http.NewRequestWithContext(c.ctx, http.MethodGet, target, nil)
		if rerr == nil {
			retry.Header = req.Header.Clone()
			resp, err = c.client.Do(retry)
		}
	}
	if err != nil {
		if isDNSError(err) {
			// Anchor can't even resolve the name: most likely a Anchor
			// networking setup issue, not the app being down. Don't alarm
			// the family with a red dot.
			return probeResult{state: "unknown", detail: "Anchor can't look up this address: " + err.Error(), dns: true}
		}
		return probeResult{state: "down", detail: err.Error(), failure: true}
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return probeResult{state: "down", detail: resp.Status, failure: true}
	}
	return probeResult{state: "up", detail: resp.Status}
}

// set records a result. An app is only shown as down after two failures in
// a row (each already retried once), so one slow response doesn't flash red.
// When an app turns red, or comes back, the admin gets an alert.
// It reports whether the dot changed.
func (c *Checker) set(a App, state, detail string, failure bool) bool {
	c.mu.Lock()
	s, ok := c.status[a.ID]
	if !ok {
		s = &Status{State: "unknown"}
		c.status[a.ID] = s
	}
	was := s.State
	s.Checked = time.Now()
	s.Detail = detail
	if failure {
		s.fails++
		if s.fails >= 2 {
			s.State = state
		}
	} else {
		s.fails = 0
		s.State = state
	}
	var alert *Notification
	var published func(bool)
	wantAlerts := false
	if c.notifier != nil && c.store != nil {
		cfg, _ := c.store.Get()
		wantAlerts = cfg.alertsEnabled() && a.alertsOn()
	}
	switch {
	case s.State == "down" && wantAlerts && s.delivery.start(c.now()):
		published = c.deliveryResult(&s.delivery)
		alert = &Notification{
			Title:    a.Name + " is down",
			delivery: s.delivery.receipts,
			Message:  fmt.Sprintf("Anchor can't reach %s.\n%s", a.Name, detail),
			Priority: 4,
			Tags:     []string{"rotating_light"},
			Click:    a.URL,
		}
	case s.State == "up" && (s.delivery.sent || s.delivery.pending || s.delivery.receipts.anySent()):
		if wantAlerts {
			alert = &Notification{
				Title:    a.Name + " is back",
				Message:  a.Name + " is responding again.",
				Priority: 3,
				Tags:     []string{"white_check_mark"},
				Click:    a.URL,
			}
		}
	}
	if s.State == "up" {
		s.delivery.reset()
	}
	changed := s.State != was
	c.mu.Unlock()
	if alert != nil {
		log.Printf("alert: %s", alert.Title)
		c.send(c.notifier, *alert, published)
	}
	return changed
}

func isDNSError(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr)
}
