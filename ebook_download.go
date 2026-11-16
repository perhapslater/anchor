package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	ebookDownloadMax       = int64(2 << 30) // room for large PDFs and comics
	ebookDownloadTimeout   = 2 * time.Hour
	ebookDownloadIdle      = time.Minute
	ebookDownloadSlots     = 6
	ebookDownloadsPerLogin = 2
)

// These bounds apply to active transfers, independently of the start-rate
// limiter. Failed or canceled transfers always return their slots.
type ebookDownloadLimiter struct {
	mu        sync.Mutex
	active    map[string]int
	total     int
	global    int
	perPerson int
}

func newEbookDownloadLimiter() *ebookDownloadLimiter {
	return &ebookDownloadLimiter{active: map[string]int{}, global: ebookDownloadSlots, perPerson: ebookDownloadsPerLogin}
}

func (l *ebookDownloadLimiter) acquire(login string) (release func(), ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if login == "" || l.total >= l.global || l.active[login] >= l.perPerson {
		return nil, false
	}
	l.total++
	l.active[login]++
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.total--
		l.active[login]--
		if l.active[login] == 0 {
			delete(l.active, login)
		}
	}, true
}

type ebookTransferLimits struct {
	maxBytes int64
	total    time.Duration
	idle     time.Duration
}

// Reset only after actual progress. Canceling the HTTP request interrupts a
// stalled upstream body read, including the initial error-page sniff.
type ebookProgressReader struct {
	r     io.Reader
	timer *time.Timer
	idle  time.Duration
}

func (r ebookProgressReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.timer.Reset(r.idle)
	}
	return n, err
}

// A canceled upstream alone cannot interrupt a blocked downstream Write.
// Rolling write deadlines bound slow readers; the context deadline also bounds
// a transfer that keeps making progress indefinitely.
type ebookDownloadWriter struct {
	ctx   context.Context
	w     http.ResponseWriter
	rc    *http.ResponseController
	idle  time.Duration
	timer *time.Timer
}

func (w ebookDownloadWriter) deadline() error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	until := time.Now().Add(w.idle)
	if end, ok := w.ctx.Deadline(); ok && end.Before(until) {
		until = end
	}
	return w.rc.SetWriteDeadline(until)
}

func (w ebookDownloadWriter) Write(p []byte) (int, error) {
	if err := w.deadline(); err != nil {
		return 0, err
	}
	n, err := w.w.Write(p)
	if n > 0 {
		w.timer.Reset(w.idle)
	}
	return n, err
}

// Reject obvious login/API error responses even when the upstream sends 200.
// This is not archive unpacking or a claim that the book itself is trustworthy.
func ebookErrorPage(contentType string, prefix []byte) bool {
	kind, _, _ := mime.ParseMediaType(contentType)
	if kind == "text/html" || kind == "application/xhtml+xml" || kind == "application/json" || strings.HasSuffix(kind, "+json") {
		return true
	}
	prefix = bytes.TrimSpace(bytes.TrimPrefix(prefix, []byte{0xef, 0xbb, 0xbf}))
	lower := bytes.ToLower(prefix)
	return bytes.HasPrefix(lower, []byte("<!doctype html")) || bytes.HasPrefix(lower, []byte("<html")) ||
		(len(prefix) > 0 && (prefix[0] == '{' || prefix[0] == '[') && json.Valid(prefix))
}
