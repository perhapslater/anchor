package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type apiEbooks struct {
	Rows    []ebook `json:"rows"`
	Updated string  `json:"updated"`
	Age     int     `json:"age"`
	Stale   bool    `json:"stale,omitempty"`
	Message string  `json:"message,omitempty"`
}

func (h *handler) ebooksGate(w http.ResponseWriter, r *http.Request) (*Config, visitor, bool) {
	w.Header().Set("Cache-Control", "no-store")
	cfg, _ := h.store.Get()
	v := h.identify(r)
	if selected := cfg.ebookFor(r.URL.Query().Get("app")); h.ebooks != nil && selected != nil {
		if app := cfg.appByID(selected.App); app != nil && cfg.visibleTo(app, v.Login) {
			scoped := *cfg
			scoped.Ebooks = selected
			return &scoped, v, true
		}
	}
	jsonReply(w, http.StatusNotFound, "Not found.")
	return nil, v, false
}

func (h *handler) ebooksList(w http.ResponseWriter, r *http.Request) {
	cfg, v, ok := h.ebooksGate(w, r)
	if !ok {
		return
	}
	force := r.Method == http.MethodPost
	if force {
		if !sameOrigin(r) {
			jsonReply(w, http.StatusForbidden, "This button only works from Anchor itself.")
			return
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			jsonReply(w, http.StatusUnsupportedMediaType, "Expected JSON.")
			return
		}
		_, _ = io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, 1024))
	}
	if !h.booksReads.allow(v.Login + "\n" + cfg.Ebooks.App) {
		w.Header().Set("Retry-After", "2")
		jsonReply(w, http.StatusTooManyRequests, "Please wait a moment and try again.")
		return
	}
	snap, stale, err := h.ebookService(cfg).get(r.Context(), cfg, force)
	if snap == nil {
		message := "The ebook library isn't available right now. Please try again in a minute."
		if cfg.isAdmin(v.Login) && err != nil {
			message += " (" + err.Error() + ")"
		}
		jsonReply(w, http.StatusServiceUnavailable, message)
		return
	}
	out := apiEbooks{
		Rows:    snap.Rows,
		Updated: snap.At.UTC().Format(time.RFC3339),
		Age:     max(0, int(time.Since(snap.At).Seconds())),
		Stale:   stale,
	}
	if stale && cfg.isAdmin(v.Login) && err != nil {
		out.Message = "The last refresh failed; showing the previous book list. " + err.Error()
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

func (h *handler) ebookCover(w http.ResponseWriter, r *http.Request) {
	cfg, v, ok := h.ebooksGate(w, r)
	if !ok {
		return
	}
	if !h.bookCovers.allow(v.Login) {
		w.Header().Set("Retry-After", "1")
		jsonReply(w, http.StatusTooManyRequests, "Please wait a moment and try again.")
		return
	}
	data, ctype, err := h.ebookService(cfg).cover(r.Context(), cfg, r.PathValue("id"))
	if err != nil {
		jsonReply(w, http.StatusNotFound, "Not found.")
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	_, _ = w.Write(data)
}

func (h *handler) ebookDownload(w http.ResponseWriter, r *http.Request) {
	cfg, v, ok := h.ebooksGate(w, r)
	if !ok {
		return
	}
	if !h.bookFiles.allow(v.Login) {
		w.Header().Set("Retry-After", "2")
		jsonReply(w, http.StatusTooManyRequests, "Please wait a moment before downloading another book.")
		return
	}
	service := h.ebookService(cfg)
	book, err := service.book(r.Context(), cfg, r.PathValue("id"))
	if err != nil {
		message := "That book isn't available to download right now. Please try again in a minute."
		if cfg.isAdmin(v.Login) {
			message += " (" + err.Error() + ")"
		}
		jsonReply(w, http.StatusNotFound, message)
		return
	}
	started, err := service.download(r.Context(), cfg.Ebooks, book, v.Login, w)
	if err != nil {
		if started {
			logEbookDownload(err)
			// In particular, unknown-length responses must not end as a clean,
			// successful chunked download after a limit or upstream failure.
			panic(http.ErrAbortHandler)
		}
		if errors.Is(err, errEbookDownloadsBusy) {
			w.Header().Set("Retry-After", "2")
			jsonReply(w, http.StatusTooManyRequests, "Please wait for an active download to finish before starting another.")
			return
		}
		status := http.StatusBadGateway
		message := "Couldn't download that book right now. Please try again in a minute."
		if cfg.isAdmin(v.Login) {
			message += " (" + err.Error() + ")"
		}
		jsonReply(w, status, message)
	}
}

func logEbookDownload(err error) {
	// Deliberately avoid logging a title or the downstream URL; errors are
	// already reduced to a service name and a safe reason.
	if err != nil {
		log.Printf("ebook download interrupted: %s", err.Error())
	}
}
