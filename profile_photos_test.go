package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func photoFixture(format string) []byte {
	im := image.NewNRGBA(image.Rect(0, 0, 12, 20))
	im.Set(3, 4, color.NRGBA{R: 255, A: 255})
	var out bytes.Buffer
	if format == "jpeg" {
		_ = jpeg.Encode(&out, im, nil)
	} else {
		_ = png.Encode(&out, im)
	}
	return out.Bytes()
}

func TestPhotoNormalizationDiscardsOriginalContent(t *testing.T) {
	for _, format := range []string{"png", "jpeg"} {
		input := append(photoFixture(format), []byte(`<script>alert('payload')</script><?php evil();?>`)...)
		out, err := normalizeProfilePhoto(input)
		if err != nil {
			t.Fatal(err)
		}
		cfg, kind, err := image.DecodeConfig(bytes.NewReader(out))
		if err != nil || kind != "png" || cfg.Width != 256 || cfg.Height != 256 || len(out) > 300000 || bytes.Contains(out, []byte("payload")) || bytes.Contains(out, []byte("<?php")) {
			t.Fatal("unsafe or unbounded normalized output")
		}
	}
}

func TestPhotoNormalizationRejectsNonImagesAndDecompressionBombDimensions(t *testing.T) {
	large := func(w, h uint32) []byte {
		data := photoFixture("png")
		binary.BigEndian.PutUint32(data[16:20], w)
		binary.BigEndian.PutUint32(data[20:24], h)
		binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
		return data
	}
	for name, data := range map[string][]byte{
		"SVG":  []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"><script>evil()</script></svg>`),
		"HTML": []byte(`<html><script>evil()</script></html>`), "GIF": []byte("GIF89a"), "ZIP": []byte("PK\x03\x04"),
		"spoofed PNG":      append([]byte("\x89PNG\r\n\x1a\n"), []byte("<script>evil()</script>")...),
		"truncated raster": photoFixture("png")[:40], "huge dimensions": large(5000, 2), "pixel bomb": large(4000, 3000),
		"byte limit": bytes.Repeat([]byte{'x'}, maxPhotoBytes+1), "empty": nil,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeProfilePhoto(data); err == nil {
				t.Fatal("malicious/unsupported image accepted")
			}
		})
	}
}

func photoResult(t *testing.T, w *httptest.ResponseRecorder) localUser {
	t.Helper()
	var result struct{ User localUser }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatalf("photo response: %d %s", w.Code, w.Body.String())
	}
	return result.User
}

func loginPhotoMember(t *testing.T, f *accountFixture, user localUser) {
	t.Helper()
	user, _ = f.state.user(user.ID)
	w := httptest.NewRecorder()
	if err := f.auth.issue(w, user); err != nil {
		t.Fatal(err)
	}
	f.cookie = w.Result().Cookies()[0]
	var status struct{ CSRF string }
	_ = json.Unmarshal(f.request("GET", "/api/auth/status", nil, false).Body.Bytes(), &status)
	f.csrf = status.CSRF
}

func TestPhotoAuthorizationPrivateServingAndSessionPreservation(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	f.auth.photoLimits = newBucketLimiter(1000, time.Second)
	member := localUser{ID: "u_photo_member", Username: "photo-member", Name: "Member", Groups: []string{"family"}, Enabled: true}
	if err := f.state.saveUser(member, "password", f.user.ID); err != nil {
		t.Fatal(err)
	}
	adminCookie, adminCSRF := f.cookie, f.csrf
	revision, _ := f.state.revision()
	before, _ := f.state.user(f.user.ID)
	body := map[string]any{"image": photoFixture("png"), "revision": 0}
	if w := f.request("POST", "/api/auth/photo", body, false); w.Code != 403 {
		t.Fatal("upload bypassed CSRF")
	}
	u := photoResult(t, f.request("POST", "/api/auth/photo", body, true))
	if !u.CustomPicture || !strings.HasPrefix(u.Picture, "/api/people/"+f.user.ID+"/photo?v=") {
		t.Fatal("upload not stored as protected application image")
	}
	w := f.request("GET", u.Picture, nil, false)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Cross-Origin-Resource-Policy") != "same-origin" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("unsafe serving: %d %v", w.Code, w.Header())
	}
	if _, err := png.Decode(bytes.NewReader(w.Body.Bytes())); err != nil {
		t.Fatal("not canonical PNG")
	}
	if stale := f.request("POST", "/api/auth/photo", body, true); stale.Code != 409 {
		t.Fatal("stale write overwrote photo")
	}
	after, _ := f.state.user(f.user.ID)
	currentRevision, _ := f.state.revision()
	if after.authVersion != before.authVersion || after.password != before.password || currentRevision != revision {
		t.Fatal("cosmetic change affected authentication/settings")
	}
	if f.request("GET", "/api/admin/state", nil, false).Code != 200 {
		t.Fatal("photo change ended current session")
	}
	loginPhotoMember(t, f, member)
	if f.request("GET", u.Picture, nil, false).Code != 404 {
		t.Fatal("member read another user's private photo")
	}
	if f.request("POST", "/api/admin/users/"+member.ID+"/photo", body, true).Code != 403 {
		t.Fatal("member reached administrator upload route")
	}
	if f.request("DELETE", "/api/admin/users/"+f.user.ID+"/photo", map[string]int{"revision": 1}, true).Code != 403 {
		t.Fatal("member removed another user's photo")
	}
	m := photoResult(t, f.request("POST", "/api/auth/photo", body, true))
	if m.ID != member.ID {
		t.Fatal("self-service wrote another user's image")
	}
	if f.request("GET", m.Picture, nil, false).Code != 200 {
		t.Fatal("member cannot read own photo")
	}
	f.cookie, f.csrf = nil, ""
	if f.request("GET", m.Picture, nil, false).Code != 404 || f.request("POST", "/api/auth/photo", body, false).Code != 403 {
		t.Fatal("anonymous photo access")
	}
	f.cookie, f.csrf = adminCookie, adminCSRF
	if f.request("GET", m.Picture, nil, false).Code != 200 {
		t.Fatal("administrator cannot view member photo")
	}
	photoResult(t, f.request("DELETE", "/api/admin/users/"+member.ID+"/photo", map[string]int64{"revision": m.PhotoRevision}, true))
	if f.request("GET", m.Picture, nil, false).Code != 404 {
		t.Fatal("default action retained old uploaded bytes")
	}
}

func TestPhotoRequestLimitsAndStrictInput(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	f.auth.photoLimits = newBucketLimiter(1000, time.Second)
	for _, body := range []any{
		map[string]any{"image": "https://internal.test/secret", "revision": 0},
		map[string]any{"image": photoFixture("png"), "revision": 0, "filename": "../../web/evil.svg"},
		map[string]any{"image": photoFixture("png"), "revision": 0, "id": "another-user"},
		map[string]any{"image": photoFixture("png")}, map[string]any{"image": "not base64", "revision": 0},
	} {
		if w := f.request("POST", "/api/auth/photo", body, true); w.Code != 400 {
			t.Fatalf("unexpected input accepted: %d %s", w.Code, w.Body.String())
		}
	}
	if w := f.request("POST", "/api/auth/photo", map[string]any{"image": bytes.Repeat([]byte{'x'}, maxPhotoBytes+1), "revision": 0}, true); w.Code != 413 {
		t.Fatalf("byte limit not enforced: %d", w.Code)
	}
	if w := f.request("POST", "/api/auth/photo", map[string]any{"image": strings.Repeat("A", maxPhotoJSON+100), "revision": 0}, true); w.Code != 413 {
		t.Fatalf("HTTP body limit not enforced: %d", w.Code)
	}
	f.auth.photoLimits = newBucketLimiter(1, time.Hour)
	if f.request("POST", "/api/auth/photo", map[string]any{"image": photoFixture("png"), "revision": 0}, true).Code != 200 {
		t.Fatal("first upload rejected")
	}
	if f.request("DELETE", "/api/auth/photo", map[string]int{"revision": 1}, true).Code != 429 {
		t.Fatal("write rate limit bypassed")
	}
	f.auth.photoLimits = newBucketLimiter(1000, time.Second)
	f.auth.photoSlots <- struct{}{}
	f.auth.photoSlots <- struct{}{}
	if f.request("POST", "/api/auth/photo", map[string]any{"image": photoFixture("png"), "revision": 1}, true).Code != 429 {
		t.Fatal("processing concurrency unbounded")
	}
	<-f.auth.photoSlots
	<-f.auth.photoSlots
	f.auth.photoReads = newBucketLimiter(1, time.Hour)
	u, _ := f.state.user(f.user.ID)
	if f.request("GET", u.Picture, nil, false).Code != 200 || f.request("GET", u.Picture, nil, false).Code != 429 {
		t.Fatal("read rate limit bypassed")
	}
}

func TestTailscalePhotoCacheOverrideAndIdentityBinding(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	f.auth.photoLimits = newBucketLimiter(1000, time.Second)
	member := localUser{ID: "u_photo_ts", Username: "ts-photo", Name: "Member", Tailscale: "photo@example.test", Groups: []string{"family"}, Enabled: true}
	if err := f.state.saveUser(member, "password", f.user.ID); err != nil {
		t.Fatal(err)
	}
	f.auth.mode, f.auth.publicURL = "tailscale", ""
	observe := func(picture, peer string) {
		r := httptest.NewRequest("GET", "http://anchor.test/api/auth/status", nil)
		r.RemoteAddr = peer
		r.Header.Set("Tailscale-User-Login", member.Tailscale)
		r.Header.Set("Tailscale-User-Profile-Pic", picture)
		f.handler.ServeHTTP(httptest.NewRecorder(), r)
	}
	before, _ := f.state.revision()
	original, _ := f.state.user(member.ID)
	observe("https://profiles.example.test/first.png", loopback)
	u, _ := f.state.user(member.ID)
	if u.Picture != "https://profiles.example.test/first.png" || u.CustomPicture || u.authVersion != original.authVersion {
		t.Fatal("trusted provider photo not remembered safely")
	}
	if revision, _ := f.state.revision(); revision != before {
		t.Fatal("observed photo invalidated settings/wizard")
	}
	loginPhotoMember(t, f, member)
	u = photoResult(t, f.request("POST", "/api/auth/photo", map[string]any{"image": photoFixture("png"), "revision": u.PhotoRevision}, true))
	custom := u.Picture
	observe("https://profiles.example.test/second.png", loopback)
	for _, invalid := range []string{"http://bad.test/a", "https://user:pass@bad.test/a", "https://127.0.0.1/a", "https://localhost/a", "data:image/svg+xml,evil"} {
		observe(invalid, loopback)
	}
	observe("https://profiles.example.test/spoof.png", "192.0.2.10:1234")
	u, _ = f.state.user(member.ID)
	if u.Picture != custom || u.tailscalePicture != "https://profiles.example.test/second.png" {
		t.Fatal("provider refresh/spoofing overwrote explicit photo or trusted fallback")
	}
	u = photoResult(t, f.request("DELETE", "/api/auth/photo", map[string]int64{"revision": u.PhotoRevision}, true))
	if u.CustomPicture || u.Picture != "https://profiles.example.test/second.png" {
		t.Fatal("default did not restore cached provider photo")
	}
	stale := u
	u.Tailscale = "new@example.test"
	if err := f.state.saveUser(u, "", f.user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.state.rememberTailscalePhoto(stale, "https://profiles.example.test/stale.png"); err == nil {
		t.Fatal("stale identity wrote a cached photo")
	}
	u, _ = f.state.user(member.ID)
	if u.Picture != "" || u.tailscalePicture != "" {
		t.Fatal("old identity's photo survived relinking")
	}
}

func TestPhotoBackupDeletionAndStaleAuthorization(t *testing.T) {
	f := newAccountFixture(t)
	f.setup(t)
	member := localUser{ID: "u_photo_backup", Username: "photo-backup", Name: "Member", Groups: []string{"family"}, Enabled: true}
	if err := f.state.saveUser(member, "password", f.user.ID); err != nil {
		t.Fatal(err)
	}
	u := photoResult(t, f.request("POST", "/api/admin/users/"+member.ID+"/photo", map[string]any{"image": photoFixture("jpeg"), "revision": 0}, true))
	backup := f.request("POST", "/api/admin/backup", map[string]any{}, true)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "anchor.db"), backup.Body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := newPersistentStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.persistent.db.Close()
	restored, ok := store.persistent.user(member.ID)
	if !ok || restored.Picture != u.Picture {
		t.Fatal("backup lost photo metadata")
	}
	var stored []byte
	if err := store.persistent.db.QueryRow(`SELECT png FROM user_photos WHERE user_id=?`, member.ID).Scan(&stored); err != nil || len(stored) == 0 {
		t.Fatal("backup lost photo bytes")
	}
	actor, _ := f.state.user(member.ID)
	changed := actor
	changed.Enabled = false
	if err := f.state.saveUser(changed, "", f.user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.state.saveProfilePhoto(actor, member.ID, u.PhotoRevision, stored); err != errPhotoAccess {
		t.Fatal("in-flight request from paused user changed photo")
	}
	revision, _ := f.state.revision()
	if err := f.state.removeUser(member.ID, revision, f.user.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.state.db.QueryRow(`SELECT count(*) FROM user_photos WHERE user_id=?`, member.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("deleted account retained image data")
	}
}

func FuzzNormalizeProfilePhoto(f *testing.F) {
	f.Add(photoFixture("png"))
	f.Add(photoFixture("jpeg"))
	f.Add([]byte(`<svg onload="evil()"/>`))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 16384 {
			return
		}
		out, err := normalizeProfilePhoto(data)
		if err != nil {
			return
		}
		cfg, format, err := image.DecodeConfig(bytes.NewReader(out))
		if err != nil || format != "png" || cfg.Width != 256 || cfg.Height != 256 || len(out) > 300000 {
			t.Fatal("unsafe normalized image")
		}
	})
}
