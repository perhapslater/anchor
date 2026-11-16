package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxPhotoBytes = 2 * 1024 * 1024
const maxPhotoJSON = (maxPhotoBytes+2)/3*4 + 1024
const photoSize = 256

var errPhotoAccess = errors.New("photo access denied")

func profilePhotoPath(id, hash string) string {
	return "/api/people/" + url.PathEscape(id) + "/photo?v=" + hash
}

// Provider URLs are observed only on trusted Tailscale requests. They are never
// fetched by Anchor; uploaded images have no URL/filename input at all.
func validProfileURL(raw string) bool {
	if len(raw) > 2048 {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Hostname() == "" || strings.HasSuffix(u.Host, ":") {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil && (!ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()) {
		return false
	}
	if u.Port() != "" {
		port, e := strconv.Atoi(u.Port())
		if e != nil || port < 1 || port > 65535 {
			return false
		}
	}
	return true
}

func (p *persistentState) rememberTailscalePhoto(observed localUser, picture string) (localUser, error) {
	if !validProfileURL(picture) {
		return observed, errors.New("invalid provider photo")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// Bind the cached photo to the same enabled identity/version that was observed.
	_, err := p.db.Exec(`INSERT INTO user_photos(user_id,tailscale_url)
 SELECT id,? FROM users WHERE id=? AND tailscale=? AND enabled=1 AND auth_version=?
 ON CONFLICT(user_id) DO UPDATE SET tailscale_url=excluded.tailscale_url WHERE user_photos.tailscale_url!=excluded.tailscale_url`, picture, observed.ID, observed.Tailscale, observed.authVersion)
	if err != nil {
		return observed, err
	}
	if user, ok := p.user(observed.ID); ok && user.Enabled && user.Tailscale == observed.Tailscale && user.authVersion == observed.authVersion {
		return user, nil
	}
	return observed, errPhotoAccess
}

// Only decode allowlisted raster formats, and inspect dimensions before pixel
// allocation. Re-encoding a new fixed-size bitmap discards metadata, animation,
// appended payloads and the original representation. No subprocesses are used.
func normalizeProfilePhoto(data []byte) ([]byte, error) {
	if len(data) == 0 || len(data) > maxPhotoBytes {
		return nil, errors.New("choose a PNG or JPEG photo up to 2 MB")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg") {
		return nil, errors.New("choose a valid PNG or JPEG photo")
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 4096 || cfg.Height > 4096 || int64(cfg.Width)*int64(cfg.Height) > 8_000_000 {
		return nil, errors.New("photo dimensions must be at most 4096 pixels per side and 8 million pixels total")
	}
	src, decodedFormat, err := image.Decode(bytes.NewReader(data))
	if err != nil || decodedFormat != format || src.Bounds().Dx() != cfg.Width || src.Bounds().Dy() != cfg.Height {
		return nil, errors.New("the photo could not be decoded")
	}
	bounds := src.Bounds()
	side := min(bounds.Dx(), bounds.Dy())
	left, top := bounds.Min.X+(bounds.Dx()-side)/2, bounds.Min.Y+(bounds.Dy()-side)/2
	dst := image.NewNRGBA(image.Rect(0, 0, photoSize, photoSize))
	for y := 0; y < photoSize; y++ {
		for x := 0; x < photoSize; x++ {
			dst.SetNRGBA(x, y, color.NRGBAModel.Convert(src.At(left+x*side/photoSize, top+y*side/photoSize)).(color.NRGBA))
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, dst); err != nil {
		return nil, errors.New("the photo could not be prepared")
	}
	return out.Bytes(), nil
}

func (a *authentication) photoActor(r *http.Request, target string) (localUser, bool) {
	who := authFrom(r)
	user, ok := a.state.user(who.user.ID)
	return user, ok && user.Enabled && user.authVersion == who.user.authVersion && (target == user.ID || groupAdmin(user.Groups))
}

func (a *authentication) profilePhoto(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	target := r.PathValue("id")
	actor, allowed := a.photoActor(r, target)
	if !allowed {
		jsonReply(w, 404, "Not found.")
		return
	}
	if !a.photoReads.allow(actor.ID) {
		w.Header().Set("Retry-After", "1")
		jsonReply(w, 429, "Please wait a moment before loading more photos.")
		return
	}
	var data []byte
	if err := a.state.db.QueryRow(`SELECT png FROM user_photos WHERE user_id=? AND png IS NOT NULL`, target).Scan(&data); err != nil || len(data) == 0 {
		jsonReply(w, 404, "Not found.")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Disposition", `inline; filename="profile.png"`)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
}

func (a *authentication) changeProfilePhoto(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	target := r.PathValue("id")
	adminRoute := target != ""
	if !adminRoute {
		target = authFrom(r).user.ID
	}
	actor, allowed := a.photoActor(r, target)
	if !allowed || (adminRoute && !groupAdmin(actor.Groups)) {
		jsonReply(w, 403, "You cannot change this profile photo.")
		return
	}
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		jsonReply(w, 415, "Expected JSON.")
		return
	}
	if !a.photoLimits.allow(actor.ID) {
		w.Header().Set("Retry-After", "180")
		jsonReply(w, 429, "Too many photo changes. Wait a few minutes and try again.")
		return
	}
	select {
	case a.photoSlots <- struct{}{}:
		defer func() { <-a.photoSlots }()
	default:
		w.Header().Set("Retry-After", "2")
		jsonReply(w, 429, "Photo processing is busy. Try again shortly.")
		return
	}
	var body struct {
		Image    []byte `json:"image"`
		Revision *int64 `json:"revision"`
	}
	control := http.NewResponseController(w)
	_ = control.SetReadDeadline(time.Now().Add(15 * time.Second))
	defer control.SetReadDeadline(time.Time{})
	r.Body = http.MaxBytesReader(w, r.Body, maxPhotoJSON)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&body)
	if err == nil {
		tail := decoder.Decode(new(any))
		if tail == nil {
			err = errors.New("expected one JSON object")
		} else if tail != io.EOF {
			err = tail
		}
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			jsonReply(w, 413, "Choose a photo up to 2 MB.")
		} else {
			jsonReply(w, 400, "Invalid photo request.")
		}
		return
	}
	if body.Revision == nil || *body.Revision < 0 {
		jsonReply(w, 400, "Reload the account before changing its photo.")
		return
	}
	var normalized []byte
	if r.Method == "DELETE" {
		if len(body.Image) != 0 {
			jsonReply(w, 400, "Default photo requests cannot include an image.")
			return
		}
	} else {
		if len(body.Image) > maxPhotoBytes {
			jsonReply(w, 413, "Choose a photo up to 2 MB.")
			return
		}
		normalized, err = normalizeProfilePhoto(body.Image)
		if err != nil {
			jsonReply(w, 400, err.Error())
			return
		}
	}
	user, err := a.state.saveProfilePhotoChecked(actor, target, *body.Revision, normalized, a.state.authorizationGuard(authFrom(r), adminRoute))
	if errors.Is(err, errPhotoAccess) || errors.Is(err, errAuthorization) {
		jsonReply(w, 403, "Your account changed. Sign in again.")
		return
	}
	if errors.Is(err, errConflict) {
		jsonReply(w, 409, "This photo changed elsewhere. Reload the account and try again.")
		return
	}
	if err != nil {
		jsonReply(w, 500, "Could not save the profile photo.")
		return
	}
	writeJSON(w, map[string]any{"user": user, "message": "Profile photo saved."})
}

func (p *persistentState) saveProfilePhoto(actor localUser, target string, revision int64, data []byte) (localUser, error) {
	return p.saveProfilePhotoChecked(actor, target, revision, data, nil)
}

func (p *persistentState) saveProfilePhotoChecked(actor localUser, target string, revision int64, data []byte, check authorizationCheck) (localUser, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if check != nil {
		if err := check(); err != nil {
			return localUser{}, err
		}
	}
	tx, err := p.db.Begin()
	if err != nil {
		return localUser{}, err
	}
	defer tx.Rollback()
	var enabled bool
	var version int
	var groups string
	if err = tx.QueryRow(`SELECT enabled,auth_version,groups_json FROM users WHERE id=?`, actor.ID).Scan(&enabled, &version, &groups); err != nil {
		return localUser{}, errPhotoAccess
	}
	var membership []string
	if json.Unmarshal([]byte(groups), &membership) != nil || !enabled || version != actor.authVersion || (target != actor.ID && !groupAdmin(membership)) {
		return localUser{}, errPhotoAccess
	}
	var exists int
	if err = tx.QueryRow(`SELECT count(*) FROM users WHERE id=?`, target).Scan(&exists); err != nil || exists != 1 {
		return localUser{}, errPhotoAccess
	}
	if _, err = tx.Exec(`INSERT OR IGNORE INTO user_photos(user_id) VALUES(?)`, target); err != nil {
		return localUser{}, err
	}
	hash := ""
	var payload any
	if len(data) > 0 {
		sum := sha256.Sum256(data)
		hash = fmt.Sprintf("%x", sum)
		payload = data
	}
	result, err := tx.Exec(`UPDATE user_photos SET png=?,hash=?,revision=revision+1 WHERE user_id=? AND revision=?`, payload, hash, target, revision)
	if err != nil {
		return localUser{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return localUser{}, err
	}
	if n != 1 {
		return localUser{}, errConflict
	}
	if _, err = tx.Exec(`INSERT INTO audit(at,actor,action) VALUES(?,?,?)`, time.Now().Unix(), actor.ID, "profile photo changed: "+target); err != nil {
		return localUser{}, err
	}
	if err = tx.Commit(); err != nil {
		return localUser{}, err
	}
	user, ok := p.user(target)
	if !ok {
		return localUser{}, sql.ErrNoRows
	}
	return user, nil
}
