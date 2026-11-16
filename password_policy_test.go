package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShortPasswordAccountLifecycle(t *testing.T) {
	f := newAccountFixture(t)
	token, err := os.ReadFile(filepath.Join(f.state.dir, "setup-token"))
	if err != nil {
		t.Fatal(err)
	}
	setup := map[string]string{"Token": string(token), "Username": "admin", "Name": "Administrator", "Password": ""}
	if w := f.request("POST", "/api/auth/setup", setup, false); w.Code != 400 || !f.state.needsSetup() {
		t.Fatal("empty setup password was accepted")
	}
	setup["Password"] = "a"
	w := f.request("POST", "/api/auth/setup", setup, false)
	if w.Code != 200 {
		t.Fatalf("short setup password rejected: %d %s", w.Code, w.Body.String())
	}
	f.cookie = w.Result().Cookies()[0]
	refreshSession := func() {
		t.Helper()
		var status struct {
			CSRF string    `json:"csrf"`
			User localUser `json:"user"`
		}
		w := f.request("GET", "/api/auth/status", nil, false)
		if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		f.csrf, f.user = status.CSRF, status.User
	}
	refreshSession()
	user, ok := f.state.user(f.user.ID)
	if !ok || !checkPassword(user.password, "a") {
		t.Fatal("short setup password not stored as a working hash")
	}
	for _, invalid := range []string{"", strings.Repeat("x", 1025)} {
		if w := f.request("POST", "/api/auth/password", map[string]string{"Current": "a", "Password": invalid}, true); w.Code != 400 {
			t.Fatal("invalid password change accepted")
		}
	}
	w = f.request("POST", "/api/auth/password", map[string]string{"Current": "a", "Password": "tiny"}, true)
	if w.Code != 200 {
		t.Fatalf("short password change rejected: %d %s", w.Code, w.Body.String())
	}
	if w := f.request("GET", "/api/admin/state", nil, false); w.Code != 403 {
		t.Fatal("password change did not revoke the old session")
	}
	w = f.request("POST", "/api/auth/login", map[string]string{"Username": "admin", "Password": "tiny"}, false)
	if w.Code != 200 {
		t.Fatalf("short-password login failed: %d %s", w.Code, w.Body.String())
	}
	f.cookie = w.Result().Cookies()[0]
	refreshSession()
	revision, err := f.state.revision()
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"revision": revision, "user": localUser{Username: "member", Name: "Member", Groups: []string{"family"}, Enabled: true}, "password": "x"}
	w = f.request("POST", "/api/admin/users", body, true)
	if w.Code != 200 {
		t.Fatalf("short administrator-assigned password rejected: %d %s", w.Code, w.Body.String())
	}
	users, err := f.state.users()
	if err != nil {
		t.Fatal(err)
	}
	var member localUser
	for _, u := range users {
		if u.Username == "member" {
			member = u
		}
	}
	if member.ID == "" || !checkPassword(member.password, "x") {
		t.Fatal("short assigned password not stored")
	}
	// Blank optional password edits must still retain the existing credential.
	revision, err = f.state.revision()
	if err != nil {
		t.Fatal(err)
	}
	member.Name = "Renamed member"
	w = f.request("POST", "/api/admin/users", map[string]any{"revision": revision, "user": member, "password": ""}, true)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	member, ok = f.state.user(member.ID)
	if !ok || !checkPassword(member.password, "x") {
		t.Fatal("blank edit removed the saved short password")
	}
}

func TestPasswordHashStillRejectsEmptyAndOversizedInput(t *testing.T) {
	for _, password := range []string{"", strings.Repeat("x", 1025)} {
		if _, err := hashPassword(password); err == nil {
			t.Fatal("invalid password accepted")
		}
	}
}
