package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Exercise the actual startup/router in a disposable child process. Every
// health check is off and integrations/credentials are absent: localhost only.
func TestAnchorMainProcess(t *testing.T) {
	if os.Getenv("ANCHOR_MAIN_TEST_CHILD") == "1" {
		main()
		return
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "anchor.yaml"), []byte(testConfig), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := newPersistentStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []localUser{
		{ID: "u_admin", Username: "admin", Name: "Admin", Tailscale: "admin@example.com", Groups: []string{"admin", "family"}, Enabled: true},
		{ID: "u_member", Username: "member", Name: "Member", Tailscale: "mom@example.com", Groups: []string{"family"}, Enabled: true},
	} {
		if err = store.persistent.saveUser(u, "test-password-12345", "setup"); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := parseConfig([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	revision, err := store.persistent.revision()
	if err != nil {
		t.Fatal(err)
	}
	if err = store.persistent.saveConfig(cfg, revision, "test"); err != nil {
		t.Fatal(err)
	}
	store.persistent.db.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestAnchorMainProcess$")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(entry), "ANCHOR_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "ANCHOR_MAIN_TEST_CHILD=1", "ANCHOR_CONFIG_DIR="+dir, "ANCHOR_LISTEN="+addr)
	var logs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer func() {
		if runtime.GOOS != "windows" {
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Errorf("SIGTERM: %v", err)
			}
			// Allow Anchor's complete drain window plus process/race-runtime
			// teardown; a fixed five seconds is shorter than valid shutdown.
			select {
			case err := <-exited:
				if err != nil {
					t.Errorf("main did not exit cleanly on SIGTERM: %v", err)
				}
			case <-time.After(shutdownGrace + 5*time.Second):
				t.Error("main did not exit within its shutdown grace period after SIGTERM")
				cmd.Process.Kill()
				<-exited
			}
		} else {
			// Windows does not support sending SIGTERM to a child. In-process
			// lifecycle tests exercise drain/cancellation on this platform.
			cmd.Process.Kill()
			<-exited
		}
		if strings.Contains(logs.String(), "WARNING: DATA RACE") {
			t.Error("race detector reported a race in the startup child process")
		}
		if t.Failed() {
			// Wait has completed, so the output buffer is no longer being written.
			t.Logf("startup child output:\n%s", logs.String())
		}
	}()
	client := &http.Client{Timeout: time.Second}
	base := "http://" + addr
	ready := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		resp, err := client.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		t.Fatal("isolated main server did not start")
	}
	for _, login := range []string{"", "unknown@example.com", "mom@example.com", "admin@example.com"} {
		req, _ := http.NewRequest("GET", base+"/api/me?as=admin@example.com", nil)
		req.Header.Set("Tailscale-User-Login", login)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var me apiMe
		err = json.NewDecoder(resp.Body).Decode(&me)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if me.Known != (login == "mom@example.com" || login == "admin@example.com") || me.Admin != (login == "admin@example.com") {
			t.Fatalf("main identity/preview: %+v", me)
		}
	}
	for _, path := range []string{"/api/requests", "/api/requests/poster/movie/1", "/api/ebooks", "/api/ebooks/book1/cover", "/api/ebooks/book1/download"} {
		req, _ := http.NewRequest("GET", base+path, nil)
		req.Header.Set("Tailscale-User-Login", "mom@example.com")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("disabled feature %s: %d", path, resp.StatusCode)
		}
	}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", base+"/api/events", nil)
	req.Header.Set("Tailscale-Funnel-Request", "true")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatal("main funnel boundary missing")
	}

	if runtime.GOOS != "windows" {
		// A browser/proxy can open a connection before sending any headers.
		// Keep one through SIGTERM: net/http may legitimately spend more
		// than five seconds draining it before the race runtime also exits.
		pending, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		// Test cleanup runs after the process-shutdown defer, so this socket
		// cannot accidentally be released before exercising the drain.
		t.Cleanup(func() { pending.Close() })
		// This later request is an accept-loop barrier: it proves the pending
		// connection was accepted before shutdown begins. Use a fresh transport
		// so it cannot reuse a connection accepted before the pending one.
		barrierClient := &http.Client{Timeout: time.Second,
			Transport: &http.Transport{DisableKeepAlives: true}}
		barrier, err := barrierClient.Get(base + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		barrier.Body.Close()
	}
}
