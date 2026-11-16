package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestProfilePhotoOnlyFromAuthenticatedServeIdentity(t *testing.T) {
	member := localUser{ID: "u_photo", Name: "Photo member", Tailscale: "photo@example.test", Groups: []string{"family"}, Enabled: true}
	const photo = "https://profiles.example.test/avatar.png"
	for _, tc := range []struct {
		name, mode, remote, login, picture, want string
	}{
		{"Serve", "tailscale", "127.0.0.1:1234", member.Tailscale, photo, photo},
		{"IPv6 Serve", "tailscale", "[::1]:1234", member.Tailscale, photo, photo},
		{"direct LAN spoof", "tailscale", "192.0.2.10:1234", member.Tailscale, photo, ""},
		{"unlinked identity", "tailscale", "127.0.0.1:1234", "unknown@example.test", photo, ""},
		{"local login", "lan", "127.0.0.1:1234", member.Tailscale, photo, ""},
		{"proxy login", "proxy", "127.0.0.1:1234", member.Tailscale, photo, ""},
		{"insecure image", "tailscale", "127.0.0.1:1234", member.Tailscale, "http://profiles.example.test/photo", ""},
		{"non-URL", "tailscale", "127.0.0.1:1234", member.Tailscale, "javascript:alert(1)", ""},
		{"URL credentials", "tailscale", "127.0.0.1:1234", member.Tailscale, "https://user:secret@profiles.example.test/photo", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Each trust-boundary check starts without a remembered provider photo.
			f := newAccountFixture(t)
			f.setup(t)
			if err := f.state.saveUser(member, "", f.user.ID); err != nil {
				t.Fatal(err)
			}
			f.auth.mode = tc.mode
			r := httptest.NewRequest("GET", "http://anchor.test/api/me", nil)
			r.RemoteAddr = tc.remote
			r.AddCookie(f.cookie)
			r.Header.Set("Tailscale-User-Login", tc.login)
			r.Header.Set("Tailscale-User-Profile-Pic", tc.picture)
			w := httptest.NewRecorder()
			f.handler.ServeHTTP(w, r)
			var body apiMe
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 {
				t.Fatalf("profile response: %d %s (%v)", w.Code, w.Body.String(), err)
			}
			if body.Picture != tc.want {
				t.Fatalf("picture = %q, want %q", body.Picture, tc.want)
			}
		})
	}
}
