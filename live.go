package main

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

const (
	eventMax          = 128
	eventsPerPerson   = 8
	eventsAnonymous   = 4
	eventWriteTimeout = 10 * time.Second
)

// Live updates. Each open page keeps one connection to /api/events, and
// Anchor sends it a short "changed" message the moment something on the
// page changes (an app goes down or comes back, anchor.yaml is edited).
// The page then fetches /api/me again. The message itself carries no
// data, so each person still only ever sees their own apps.

// Changes tells open pages that something changed.
type Changes struct {
	mu     sync.Mutex
	subs   map[chan struct{}]struct{}
	people map[string]int
}

func NewChanges() *Changes {
	return &Changes{subs: map[chan struct{}]struct{}{}, people: map[string]int{}}
}

// Subscribe returns a channel that receives a value after each change, and
// a function to call when the page goes away. Changes that happen before
// the page catches up arrive as one.
func (c *Changes) Subscribe() (<-chan struct{}, func()) {
	ch, stop, _ := c.subscribe("internal", false)
	return ch, stop
}

func (c *Changes) subscribe(login string, limited bool) (<-chan struct{}, func(), bool) {
	ch := make(chan struct{}, 1)
	c.mu.Lock()
	limit := eventsPerPerson // several tabs/devices per known person
	if login == "" {
		// One shared anonymous allowance, never keyed by the loopback proxy IP.
		limit = eventsAnonymous
	}
	if limited && (len(c.subs) >= eventMax || c.people[login] >= limit) {
		c.mu.Unlock()
		return nil, nil, false
	}
	c.subs[ch] = struct{}{}
	c.people[login]++
	c.mu.Unlock()
	return ch, func() {
		c.mu.Lock()
		if _, exists := c.subs[ch]; exists {
			delete(c.subs, ch)
			c.people[login]--
			if c.people[login] == 0 {
				delete(c.people, login)
			}
		}
		c.mu.Unlock()
	}, true
}

// Notify tells every open page that something changed. It never blocks.
func (c *Changes) Notify() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for ch := range c.subs {
		select {
		case ch <- struct{}{}:
		default: // already has one waiting
		}
	}
}

// keepAlive is how often an idle stream sends a comment line, so
// connections that died quietly (a phone that walked out of Wi-Fi) are
// noticed and cleaned up.
const keepAlive = 25 * time.Second

func (h *handler) events(w http.ResponseWriter, r *http.Request) {
	cfg, _ := h.store.Get()
	login := h.identify(r).Login
	if _, known := cfg.person(login); !known {
		login = ""
	}
	ch, stop, ok := h.checker.changes.subscribe(login, true)
	if !ok {
		w.Header().Set("Retry-After", "5")
		jsonReply(w, http.StatusTooManyRequests, "Too many live connections. Please try again shortly.")
		return
	}
	defer stop()
	rc := http.NewResponseController(w)
	if rc.SetWriteDeadline(time.Now().Add(eventWriteTimeout)) != nil {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if rc.Flush() != nil {
		return
	}

	ping := time.NewTicker(keepAlive)
	defer ping.Stop()
	for {
		var err error
		select {
		case <-h.stopping:
			return
		case <-r.Context().Done():
			return
		case <-ch:
			if rc.SetWriteDeadline(time.Now().Add(eventWriteTimeout)) != nil {
				return
			}
			_, err = fmt.Fprint(w, "data: changed\n\n")
		case <-ping.C:
			if rc.SetWriteDeadline(time.Now().Add(eventWriteTimeout)) != nil {
				return
			}
			_, err = fmt.Fprint(w, ": ping\n\n")
		}
		if err != nil || rc.Flush() != nil {
			return
		}
	}
}
