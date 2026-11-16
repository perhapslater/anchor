package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Everything Anchor asks Seerr, Plex, Sonarr and Radarr goes through this
// file, and it is deliberately narrow:
//
//   - the only places it connects to are the four addresses in anchor.yaml;
//     nothing a visitor sends ever becomes part of an address
//   - GET only, and only paths on a fixed list for that service
//   - redirects are never followed (Go would forward X-Api-Key and
//     X-Plex-Token to wherever a redirect pointed)
//   - environment proxy settings are ignored
//   - connections to loopback (unless ANCHOR_DEV_USER is set), link-local
//     (cloud metadata), unspecified and multicast addresses are refused at
//     connection time, so a DNS answer can't steer Anchor there either
//   - keys travel in headers only and never appear in an error or a log line
//   - every response is size-limited
//   - a picture (a poster from Plex, Seerr, Sonarr or Radarr, see posters.go) is
//     accepted only if its first bytes say JPEG, PNG or WebP, whatever the
//     service calls it

// newUpstreamClient is the HTTP client for all four services.
func newUpstreamClient(allowLoopback bool) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: guardAddr(allowLoopback)}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = dialer.DialContext
	tr.ResponseHeaderTimeout = 8 * time.Second
	forceHTTP1(tr) // fresh connections, as for the status checks
	return &http.Client{
		Transport:     tr,
		Timeout:       8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// guardAddr runs for every connection, with the address actually being
// dialled. LAN, Docker and tailnet addresses are fine; the services Anchor
// talks to live there.
func guardAddr(allowLoopback bool) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		ip := net.ParseIP(host)
		if err != nil || ip == nil ||
			(ip.IsLoopback() && !allowLoopback) ||
			ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.Equal(net.IPv4bcast) {
			return errors.New("address not allowed")
		}
		return nil
	}
}

// upstream is one service Anchor may call.
type upstream struct {
	name    string // for messages: "Seerr"
	base    *url.URL
	header  string // where the key goes: X-Api-Key or X-Plex-Token
	key     string
	client  *http.Client
	paths   []*regexp.Regexp // the only paths that may be requested
	maxBody int64
}

func newUpstream(name, rawBase, header, key string, client *http.Client, maxBody int64, paths ...string) (*upstream, error) {
	clean, err := validUpstream(rawBase)
	if err != nil {
		return nil, fmt.Errorf("%s address %s", name, err)
	}
	base, err := url.Parse(clean)
	if err != nil {
		return nil, fmt.Errorf("%s address %s", name, err)
	}
	if key == "" {
		return nil, fmt.Errorf("%s has no key", name)
	}
	u := &upstream{name: name, base: base, header: header, key: key, client: client, maxBody: maxBody}
	for _, p := range paths {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("%s path pattern: %w", name, err)
		}
		u.paths = append(u.paths, re)
	}
	return u, nil
}

func (u *upstream) pathAllowed(path string) bool {
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#%\\") ||
		strings.Contains(path, "..") || strings.Contains(path, "//") {
		return false
	}
	for _, re := range u.paths {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}

// sameHost is a last check that a request really is for the configured
// service.
func (u *upstream) sameHost(t *url.URL) bool {
	return t.Scheme == u.base.Scheme && t.Host == u.base.Host && t.User == nil
}

// get fetches path (plus the query) from the service and returns the body,
// which is never longer than limit. Everything Anchor asks goes through here.
func (u *upstream) get(ctx context.Context, path string, q url.Values, accept string, limit int64) ([]byte, error) {
	if !u.pathAllowed(path) {
		return nil, fmt.Errorf("%s: that address is not on Anchor's list", u.name)
	}
	target := *u.base
	target.Path = strings.TrimRight(u.base.Path, "/") + path
	target.RawQuery = q.Encode()
	if !u.sameHost(&target) {
		return nil, fmt.Errorf("%s: wrong host", u.name)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("%s: bad request", u.name)
	}
	req.Header.Set(u.header, u.key)
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "Anchor")

	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s could not be reached: %s", u.name, reason(err))
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return nil, fmt.Errorf("%s redirected the request (Anchor doesn't follow redirects)", u.name)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%s answered %s", u.name, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%s: could not read the answer: %s", u.name, reason(err))
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%s: answer too large", u.name)
	}
	return body, nil
}

// getJSON fetches path (plus the query) from the service and decodes the
// JSON answer into out.
func (u *upstream) getJSON(ctx context.Context, path string, q url.Values, out any) error {
	body, err := u.get(ctx, path, q, "application/json", u.maxBody)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: answer was not what Anchor expected", u.name)
	}
	return nil
}

// getImage fetches a picture, at most limit bytes of it, and says what kind it
// is. The kind comes from the bytes themselves, never from what the service
// said it was, and only JPEG, PNG and WebP are accepted: anything else (a web
// page, an SVG that could carry a script) is refused and never passed on.
func (u *upstream) getImage(ctx context.Context, path string, q url.Values, limit int64) ([]byte, string, error) {
	body, err := u.get(ctx, path, q, "image/jpeg, image/png, image/webp", limit)
	if err != nil {
		return nil, "", err
	}
	kind := imageType(body)
	if kind == "" {
		return nil, "", fmt.Errorf("%s: answer was not a picture Anchor accepts", u.name)
	}
	return body, kind, nil
}

// imageType names the picture format from its first bytes, or "" if it isn't
// one of the three Anchor serves.
func imageType(b []byte) string {
	switch {
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg"
	case len(b) >= 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

// reason describes a network error without the address that was asked for,
// which for these calls includes the query.
func reason(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return err.Error()
}
