package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	ebookCatalogTTL    = 5 * time.Minute
	ebookFailBackoff   = time.Minute
	ebookRefreshLimit  = 90 * time.Second
	ebookPageSize      = 200
	ebookMaxPages      = 100
	ebookMaxPageBytes  = 16 << 20
	ebookCoverMax      = 512 << 10
	ebookForcedGap     = 30 * time.Second
	ebookForcedPerHour = 60
)

var ebookItemID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var ebookFileIno = regexp.MustCompile(`^[0-9]{1,32}$`)

type ebook struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Author  string `json:"author,omitempty"`
	Year    string `json:"year,omitempty"`
	Format  string `json:"format"`
	AddedAt int64  `json:"-"`
	FileIno string `json:"-"`
}

type ebookSnapshot struct {
	Rows []ebook
	At   time.Time
	sig  string
}

type ebookCall struct {
	done     chan struct{}
	sig      string
	snapshot *ebookSnapshot
	stale    bool
	err      error
}

type ebookService struct {
	ctx           context.Context
	key           string
	allowLoopback bool
	readClient    *http.Client
	streamClient  *http.Client
	downloads     *ebookDownloadLimiter
	transfer      ebookTransferLimits

	mu          sync.Mutex
	snapshot    *ebookSnapshot
	flight      *ebookCall
	lastAttempt time.Time
	lastErr     error
	lastSig     string
	forced      *rateLimiter
	now         func() time.Time
	covers      *posterCache // shared bounded/single-flight image policy, independent entries
}

func newEbookService(key string, allowLoopback bool) *ebookService {
	s := &ebookService{
		ctx:           context.Background(),
		key:           strings.TrimSpace(key),
		allowLoopback: allowLoopback,
		readClient:    newEbookHTTPClient(allowLoopback, 30*time.Second),
		streamClient:  newEbookHTTPClient(allowLoopback, ebookDownloadTimeout),
		downloads:     newEbookDownloadLimiter(),
		transfer:      ebookTransferLimits{maxBytes: ebookDownloadMax, total: ebookDownloadTimeout, idle: ebookDownloadIdle},
		covers:        newPosterCache(),
		forced:        newRateLimiter(ebookForcedGap, ebookForcedPerHour),
		now:           time.Now,
	}
	s.forced.now = func() time.Time { return s.now() }
	s.covers.now = func() time.Time { return s.now() }
	return s
}

func newEbookHTTPClient(allowLoopback bool, timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: guardAddr(allowLoopback)}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.DialContext = dialer.DialContext
	tr.ResponseHeaderTimeout = 10 * time.Second
	forceHTTP1(tr)
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (c *EbooksConfig) sig(key string) string {
	h := sha256.Sum256([]byte(key))
	return strings.Join([]string{c.App, c.Server, c.LibraryID, hex.EncodeToString(h[:])}, "\n")
}

func (s *ebookService) get(ctx context.Context, cfg *Config, force bool) (*ebookSnapshot, bool, error) {
	if cfg.Ebooks == nil {
		return nil, false, errors.New("ebook catalog is not configured")
	}
	if s.keyFor(cfg.Ebooks) == "" {
		if cfg.Ebooks.secretMode {
			return nil, false, errors.New("Audiobookshelf API key is missing; edit this library in Administration")
		}
		return nil, false, errors.New("ANCHOR_ABS_KEY is empty")
	}
	sig := cfg.Ebooks.sig(s.keyFor(cfg.Ebooks))
	for {
		s.mu.Lock()
		now := s.now()
		current := s.snapshot
		if current != nil && current.sig != sig {
			current = nil
		}
		failed := s.lastSig == sig && s.lastErr != nil
		if failed && now.Sub(s.lastAttempt) < ebookFailBackoff {
			err := s.lastErr
			s.mu.Unlock()
			return current, current != nil, err
		}
		if !force && current != nil && now.Sub(current.At) < ebookCatalogTTL {
			var err error
			if failed {
				err = s.lastErr
			}
			s.mu.Unlock()
			return current, failed, err
		}
		if call := s.flight; call != nil {
			s.mu.Unlock()
			select {
			case <-call.done:
				if call.sig == sig {
					return call.snapshot, call.stale, call.err
				}
				force = false // a concurrent request already performed the refresh
				continue
			case <-ctx.Done():
				return nil, false, ctx.Err()
			}
		}
		if force && ((s.lastSig == sig && now.Sub(s.lastAttempt) < ebookForcedGap) || !s.forced.allow("refresh")) {
			s.mu.Unlock()
			return current, failed, errors.New("ebook refresh is cooling down; please try again shortly")
		}
		call := &ebookCall{done: make(chan struct{}), sig: sig}
		s.flight = call
		s.mu.Unlock()
		go s.refresh(cfg.Ebooks, sig, call)
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-call.done:
			return call.snapshot, call.stale, call.err
		}
	}
}

func (s *ebookService) refresh(cfg *EbooksConfig, sig string, call *ebookCall) {
	ctx, cancel := context.WithTimeout(s.ctx, ebookRefreshLimit)
	defer cancel()
	var rows []ebook
	var err error
	func() {
		defer func() {
			if recover() != nil {
				err = errors.New("unexpected ebook refresh failure")
			}
		}()
		rows, err = s.fetchCatalog(ctx, cfg)
	}()
	s.mu.Lock()
	s.lastAttempt, s.lastSig, s.lastErr = s.now(), sig, err
	if err == nil {
		s.snapshot = &ebookSnapshot{Rows: rows, At: s.lastAttempt, sig: sig}
	}
	call.err = err
	if current := s.snapshot; current != nil && current.sig == sig {
		call.snapshot, call.stale = current, err != nil
	}
	s.flight = nil
	close(call.done)
	s.mu.Unlock()
}

type absLibraryItems struct {
	Results []absLibraryItem `json:"results"`
	Total   int              `json:"total"`
}

type absLibraryItem struct {
	ID        string  `json:"id"`
	AddedAt   int64   `json:"addedAt"`
	MediaType string  `json:"mediaType"`
	IsMissing bool    `json:"isMissing"`
	IsInvalid bool    `json:"isInvalid"`
	Media     absBook `json:"media"`
}

type absBook struct {
	Metadata    absBookMetadata `json:"metadata"`
	EbookFormat string          `json:"ebookFormat"`
	EbookFile   *absEbookFile   `json:"ebookFile"`
}

type absBookMetadata struct {
	Title         string `json:"title"`
	AuthorName    string `json:"authorName"`
	PublishedYear string `json:"publishedYear"`
}

type absEbookFile struct {
	Ino         string `json:"ino"`
	EbookFormat string `json:"ebookFormat"`
}

func (s *ebookService) client(cfg *EbooksConfig, client *http.Client) (*upstream, error) {
	return newUpstream("Audiobookshelf", cfg.Server, "Authorization", "Bearer "+s.keyFor(cfg), client, ebookMaxPageBytes,
		`^/api/libraries/[A-Za-z0-9_-]{1,64}/items$`,
		`^/api/items/[A-Za-z0-9_-]{1,64}/cover$`,
		`^/api/items/[A-Za-z0-9_-]{1,64}/ebook$`,
		`^/api/items/[A-Za-z0-9_-]{1,64}/file/[0-9]{1,32}/download$`)
}

func (s *ebookService) fetchCatalog(ctx context.Context, cfg *EbooksConfig) ([]ebook, error) {
	u, err := s.client(cfg, s.readClient)
	if err != nil {
		return nil, err
	}
	out := make([]ebook, 0)
	read := 0
	for page := 0; page < ebookMaxPages; page++ {
		q := url.Values{
			"limit":    {fmt.Sprint(ebookPageSize)},
			"page":     {fmt.Sprint(page)},
			"sort":     {"media.metadata.title"},
			"minified": {"0"},
		}
		var answer absLibraryItems
		path := "/api/libraries/" + cfg.LibraryID + "/items"
		if err := u.getJSON(ctx, path, q, &answer); err != nil {
			return nil, err
		}
		for _, item := range answer.Results {
			if item.MediaType != "book" || item.IsMissing || item.IsInvalid || !ebookItemID.MatchString(item.ID) {
				continue
			}
			format := strings.TrimSpace(item.Media.EbookFormat)
			fileIno := ""
			if item.Media.EbookFile != nil {
				if item.Media.EbookFile.EbookFormat != "" {
					format = item.Media.EbookFile.EbookFormat
				}
				fileIno = item.Media.EbookFile.Ino
			}
			format = strings.ToLower(format)
			if !supportedEbookFormat(format) {
				continue
			}
			title := cleanText(item.Media.Metadata.Title, 300)
			if title == "" {
				title = "Untitled book"
			}
			out = append(out, ebook{
				ID:      item.ID,
				Title:   title,
				Author:  cleanText(item.Media.Metadata.AuthorName, 200),
				Year:    cleanText(item.Media.Metadata.PublishedYear, 8),
				Format:  format,
				AddedAt: item.AddedAt,
				FileIno: fileIno,
			})
		}
		// Total includes audio-only, invalid and unsupported items filtered
		// above. Pagination must count all upstream items, not just ebooks.
		read += len(answer.Results)
		if len(answer.Results) == 0 || read >= answer.Total && answer.Total > 0 {
			sortEbooksByRecentlyAdded(out)
			return out, nil
		}
		if len(answer.Results) < ebookPageSize {
			sortEbooksByRecentlyAdded(out)
			return out, nil
		}
	}
	return nil, errors.New("Audiobookshelf library has too many books to list")
}

func sortEbooksByRecentlyAdded(books []ebook) {
	sort.SliceStable(books, func(i, j int) bool {
		if books[i].AddedAt != books[j].AddedAt {
			return books[i].AddedAt > books[j].AddedAt
		}
		return strings.ToLower(books[i].Title) < strings.ToLower(books[j].Title)
	})
}

func supportedEbookFormat(format string) bool {
	switch format {
	case "epub", "pdf", "cbr", "cbz", "azw3", "mobi":
		return true
	default:
		return false
	}
}

func (s *ebookService) book(ctx context.Context, cfg *Config, id string) (*ebook, error) {
	if !ebookItemID.MatchString(id) {
		return nil, errors.New("book not found")
	}
	snap, _, err := s.get(ctx, cfg, false)
	if err != nil && snap == nil {
		return nil, err
	}
	for i := range snap.Rows {
		if snap.Rows[i].ID == id {
			return &snap.Rows[i], nil
		}
	}
	return nil, errors.New("book not found")
}

func (s *ebookService) cover(ctx context.Context, cfg *Config, id string) ([]byte, string, error) {
	if _, err := s.book(ctx, cfg, id); err != nil {
		return nil, "", err
	}
	sig := cfg.Ebooks.sig(s.keyFor(cfg.Ebooks))
	data, ctype, ok := s.covers.get(ctx, posterKey{sig: sig, item: id}, func(ctx context.Context) ([]byte, string, error) {
		u, err := s.client(cfg.Ebooks, s.readClient)
		if err != nil {
			return nil, "", err
		}
		return u.getImage(ctx, "/api/items/"+id+"/cover", url.Values{"width": {"180"}}, ebookCoverMax)
	})
	if !ok {
		return nil, "", errors.New("ebook cover unavailable")
	}
	return data, ctype, nil
}

var errEbookDownloadsBusy = errors.New("too many active ebook downloads")

func (s *ebookService) download(ctx context.Context, cfg *EbooksConfig, book *ebook, login string, w http.ResponseWriter) (bool, error) {
	if book == nil || !ebookItemID.MatchString(book.ID) || (book.FileIno != "" && !ebookFileIno.MatchString(book.FileIno)) {
		return false, errors.New("book not found")
	}
	release, ok := s.downloads.acquire(login)
	if !ok {
		return false, errEbookDownloadsBusy
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, s.transfer.total)
	defer cancel()
	u, err := s.client(cfg, s.streamClient)
	if err != nil {
		return false, err
	}
	path := "/api/items/" + book.ID + "/ebook"
	if book.FileIno != "" {
		path = "/api/items/" + book.ID + "/file/" + book.FileIno + "/download"
	}
	if !u.pathAllowed(path) {
		return false, errors.New("download address is not allowed")
	}
	target := *u.base
	target.Path = strings.TrimRight(u.base.Path, "/") + path
	if !u.sameHost(&target) {
		return false, errors.New("wrong Audiobookshelf host")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return false, errors.New("could not create download request")
	}
	req.Header.Set("Authorization", "Bearer "+s.keyFor(cfg))
	req.Header.Set("User-Agent", "Anchor")
	resp, err := s.streamClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("Audiobookshelf could not be reached: %s", reason(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("Audiobookshelf answered %s", resp.Status)
	}
	if resp.ContentLength > s.transfer.maxBytes {
		return false, errors.New("ebook exceeds the download size limit")
	}
	idle := time.AfterFunc(s.transfer.idle, cancel)
	defer idle.Stop()
	body := bufio.NewReader(ebookProgressReader{r: resp.Body, timer: idle, idle: s.transfer.idle})
	prefix, err := body.Peek(512)
	if err != nil && err != io.EOF {
		return false, fmt.Errorf("could not read ebook: %s", reason(err))
	}
	if len(prefix) == 0 || ebookErrorPage(resp.Header.Get("Content-Type"), prefix) {
		return false, errors.New("Audiobookshelf did not return an ebook")
	}
	rc := http.NewResponseController(w)
	writer := ebookDownloadWriter{ctx: ctx, w: w, rc: rc, idle: s.transfer.idle, timer: idle}
	if err := writer.deadline(); err != nil {
		return false, errors.New("could not set download write deadline")
	}
	// Keep the deadline through net/http's final buffered/chunked response
	// writes. The server clears it after finishing this response, before reuse.
	filename := ebookFilename(book.Title, book.Format)
	w.Header().Set("Content-Type", ebookMIME(book.Format))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	if resp.ContentLength >= 0 {
		w.Header().Set("Content-Length", fmt.Sprint(resp.ContentLength))
	}
	w.WriteHeader(http.StatusOK)
	n, err := io.Copy(writer, io.LimitReader(body, s.transfer.maxBytes))
	if err == nil && n == s.transfer.maxBytes {
		// Probe for overflow without ever forwarding an extra byte. A stalled
		// probe is bounded by the same idle and total limits as the transfer.
		if _, readErr := body.ReadByte(); readErr == nil {
			err = errors.New("ebook exceeds the download size limit")
		} else if readErr != io.EOF {
			err = readErr
		}
	}
	if err == nil && resp.ContentLength >= 0 && n != resp.ContentLength {
		err = io.ErrUnexpectedEOF
	}
	if err == nil {
		if err = writer.deadline(); err == nil {
			err = rc.Flush()
		}
	}
	if err != nil {
		return true, fmt.Errorf("download interrupted: %s", reason(err))
	}
	return true, nil
}

func ebookMIME(format string) string {
	switch format {
	case "epub":
		return "application/epub+zip"
	case "pdf":
		return "application/pdf"
	case "cbr":
		return "application/vnd.comicbook-rar"
	case "cbz":
		return "application/vnd.comicbook+zip"
	case "azw3":
		return "application/vnd.amazon.ebook"
	case "mobi":
		return "application/x-mobipocket-ebook"
	default:
		return "application/octet-stream"
	}
}

func ebookFilename(title, format string) string {
	name := strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`<>:"/\\|?*`, r) {
			return '-'
		}
		return r
	}, title)
	name = strings.Trim(strings.TrimSpace(name), ".")
	if name == "" {
		name = "ebook"
	}
	name = filepath.Base(name)
	return name + "." + format
}

func (s *ebookService) keyFor(cfg *EbooksConfig) string {
	if cfg != nil && cfg.secretMode {
		return cfg.secret
	}
	return s.key
}
