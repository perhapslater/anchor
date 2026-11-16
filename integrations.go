package main

import "strings"

func (c *Config) requestConnections() []*Requests {
	var out []*Requests
	if c.Requests != nil {
		out = append(out, c.Requests)
	}
	for i := range c.RequestConnections {
		out = append(out, &c.RequestConnections[i])
	}
	return out
}

func (c *Config) ebookLibraries() []*EbooksConfig {
	var out []*EbooksConfig
	if c.Ebooks != nil {
		out = append(out, c.Ebooks)
	}
	for i := range c.EbookLibraries {
		out = append(out, &c.EbookLibraries[i])
	}
	return out
}

// Old unqualified URLs continue to work when there is exactly one source.
// With several sources, the caller must name a tile, then pass its access gate.
func (c *Config) requestFor(app string) *Requests {
	connections := c.requestConnections()
	for _, r := range connections {
		if r.App == app || app == "" && len(connections) == 1 {
			return r
		}
	}
	return nil
}

func (c *Config) ebookFor(app string) *EbooksConfig {
	libraries := c.ebookLibraries()
	for _, e := range libraries {
		if e.App == app || app == "" && len(libraries) == 1 {
			return e
		}
	}
	return nil
}

func requestSecretName(r *Requests, provider string) string {
	if r.scoped {
		return "requests/" + r.App + "/" + provider
	}
	return provider
}

func ebookSecretName(e *EbooksConfig) string {
	if e.scoped {
		return "ebooks/" + e.App + "/abs"
	}
	return "abs"
}

func validSecretName(name string) bool {
	for _, legacy := range secretNames {
		if name == legacy {
			return true
		}
	}
	parts := strings.Split(name, "/")
	if len(parts) != 3 || !appID.MatchString(parts[1]) {
		return false
	}
	return parts[0] == "ebooks" && parts[2] == "abs" || parts[0] == "requests" &&
		(parts[2] == "seerr" || parts[2] == "plex" || parts[2] == "sonarr" || parts[2] == "radarr")
}

func (c *Config) usesScopedSecret(name string) bool {
	for _, r := range c.requestConnections() {
		for _, provider := range []string{"seerr", "plex", "sonarr", "radarr"} {
			if r.scoped && requestSecretName(r, provider) == name {
				return true
			}
		}
	}
	for _, e := range c.ebookLibraries() {
		if e.scoped && ebookSecretName(e) == name {
			return true
		}
	}
	return false
}

// New connections use independent caches, refresh limits, and credentials.
// Legacy services stay intact for existing installations and test fixtures.
func (h *handler) requestService(cfg *Config) *requestStatus {
	if !cfg.Requests.scoped {
		return h.requests
	}
	h.integrationMu.Lock()
	defer h.integrationMu.Unlock()
	current, _ := h.store.Get()
	active := map[string]bool{}
	for _, r := range current.requestConnections() {
		active[r.sig()] = true
	}
	for key := range h.requestServices {
		if !active[key] {
			h.requestServices[key].client.CloseIdleConnections()
			delete(h.requestServices, key)
		}
	}
	key := cfg.Requests.sig()
	if h.requestServices == nil {
		h.requestServices = map[string]*requestStatus{}
	}
	if service := h.requestServices[key]; service != nil {
		return service
	}
	service := newRequestStatus(h.store, requestKeys{}, h.requests.allowLoopback)
	service.ctx, service.posters.ctx = h.requests.ctx, h.requests.ctx
	// A request authorized just before a configuration change may finish, but
	// must not put an obsolete service back in the retained cache.
	if active[key] {
		h.requestServices[key] = service
	}
	return service
}

func (h *handler) ebookService(cfg *Config) *ebookService {
	if !cfg.Ebooks.scoped {
		return h.ebooks
	}
	h.integrationMu.Lock()
	defer h.integrationMu.Unlock()
	current, _ := h.store.Get()
	active := map[string]bool{}
	for _, e := range current.ebookLibraries() {
		active[e.sig(e.secret)] = true
	}
	for key := range h.ebookServices {
		if !active[key] {
			h.ebookServices[key].readClient.CloseIdleConnections()
			h.ebookServices[key].streamClient.CloseIdleConnections()
			delete(h.ebookServices, key)
		}
	}
	key := cfg.Ebooks.sig(cfg.Ebooks.secret)
	if h.ebookServices == nil {
		h.ebookServices = map[string]*ebookService{}
	}
	if service := h.ebookServices[key]; service != nil {
		return service
	}
	service := newEbookService("", h.ebooks.allowLoopback)
	service.ctx, service.covers.ctx = h.ebooks.ctx, h.ebooks.ctx
	// Download concurrency remains bounded across all libraries.
	service.downloads, service.transfer = h.ebooks.downloads, h.ebooks.transfer
	if active[key] {
		h.ebookServices[key] = service
	}
	return service
}
