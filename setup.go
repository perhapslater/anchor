package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func printSetupInstructions(p *persistentState, logger *log.Logger, origin string) error {
	if !p.needsSetup() {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(p.dir, "setup-token"))
	if err != nil {
		return errors.New("could not read setup code")
	}
	token := strings.TrimSpace(string(raw))
	if !validSetupToken(token) {
		return errors.New("invalid setup code")
	}
	if origin == "" {
		origin = "Anchor's HTTPS Tailscale Serve address"
	}
	logger.Printf("\n=== Anchor first-run setup ===\nOpen %s and follow setup.\nOne-time setup code: %s\nThis code claims the administrator account and expires when setup succeeds.\nKeep container logs private until then.\n==============================", origin, token)
	return nil
}

type onboardingState struct {
	Step      int   `json:"step"`
	Completed bool  `json:"completed"`
	Skipped   []int `json:"skipped"`
	Revision  int64 `json:"revision"`
}

// Schema 3 moves People before Tailscale. Remap both the current step and
// skipped steps once, keeping existing users at the same named wizard stage.
func migratePeopleBeforeTailscale(tx *sql.Tx) error {
	var step int
	var raw string
	if err := tx.QueryRow(`SELECT step,skipped FROM onboarding WHERE id=1`).Scan(&step, &raw); err != nil {
		return err
	}
	var skipped []int
	if err := json.Unmarshal([]byte(raw), &skipped); err != nil {
		return errors.New("could not migrate setup progress")
	}
	remap := func(value int) int {
		if value == 1 {
			return 2
		}
		if value == 2 {
			return 1
		}
		return value
	}
	for i := range skipped {
		skipped[i] = remap(skipped[i])
	}
	if skipped == nil {
		skipped = []int{}
	}
	updated, err := json.Marshal(skipped)
	if err != nil {
		return err
	}
	// Old browser tabs must reload before saving indices from the former order.
	_, err = tx.Exec(`UPDATE onboarding SET step=?,skipped=?,revision=revision+1 WHERE id=1`, remap(step), string(updated))
	return err
}

func (p *persistentState) onboarding() (onboardingState, error) {
	var s onboardingState
	var skipped string
	err := p.db.QueryRow(`SELECT step,completed,skipped,revision FROM onboarding WHERE id=1`).Scan(&s.Step, &s.Completed, &skipped, &s.Revision)
	if err == nil {
		err = json.Unmarshal([]byte(skipped), &s.Skipped)
	}
	return s, err
}

// Schema 4 removes the informational connection stage. Its summary now lives
// in Review; existing current/skipped stages keep their meaning after the move.
func migrateConnectionToReview(tx *sql.Tx) error {
	var step int
	var raw string
	if err := tx.QueryRow(`SELECT step,skipped FROM onboarding WHERE id=1`).Scan(&step, &raw); err != nil {
		return err
	}
	var previous []int
	if err := json.Unmarshal([]byte(raw), &previous); err != nil {
		return errors.New("could not migrate setup progress")
	}
	if step > 0 {
		step--
	}
	skipped := []int{}
	for _, old := range previous {
		if old > 0 {
			skipped = append(skipped, old-1)
		}
	}
	updated, err := json.Marshal(skipped)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE onboarding SET step=?,skipped=?,revision=revision+1 WHERE id=1`, step, string(updated))
	return err
}

// Schema 5 puts Tailscale after Applications and Notifications. Preserve the
// named stage and invalidate old tabs before they can save the former indices.
func migrateTailscaleBeforeReview(tx *sql.Tx) error {
	var step int
	var raw string
	if err := tx.QueryRow(`SELECT step,skipped FROM onboarding WHERE id=1`).Scan(&step, &raw); err != nil {
		return err
	}
	var skipped []int
	if err := json.Unmarshal([]byte(raw), &skipped); err != nil {
		return errors.New("could not migrate setup progress")
	}
	remap := func(value int) int {
		switch value {
		case 1:
			return 3
		case 2, 3:
			return value - 1
		}
		return value
	}
	for i := range skipped {
		skipped[i] = remap(skipped[i])
	}
	if skipped == nil {
		skipped = []int{}
	}
	updated, err := json.Marshal(skipped)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE onboarding SET step=?,skipped=?,revision=revision+1 WHERE id=1`, remap(step), string(updated))
	return err
}

func (h *handler) setupProgress(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	var s onboardingState
	if !readJSON(w, r, &s) {
		return
	}
	if s.Step < 0 || s.Step > 4 || s.Completed && s.Step != 4 || len(s.Skipped) > 4 {
		jsonReply(w, 400, "Invalid setup step.")
		return
	}
	for _, step := range s.Skipped {
		if step < 0 || step > 3 {
			jsonReply(w, 400, "Invalid skipped step.")
			return
		}
	}
	if s.Skipped == nil {
		s.Skipped = []int{}
	}
	raw, _ := json.Marshal(s.Skipped)
	p := h.store.persistent
	p.mu.Lock()
	defer p.mu.Unlock()
	if !h.requireAdminLocked(w, r) {
		return
	}
	// Completion is final. Older tabs cannot turn the finished wizard back into
	// an incomplete session, even after reading the newest revision.
	result, err := h.store.persistent.db.Exec(`UPDATE onboarding SET step=?,completed=?,skipped=?,revision=revision+1 WHERE id=1 AND revision=? AND completed=0`, s.Step, s.Completed, string(raw), s.Revision)
	if err != nil {
		jsonReply(w, 500, "Could not save setup progress.")
		return
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		jsonReply(w, 409, "Setup progress changed or setup has finished. Reload Administration to continue.")
		return
	}
	s.Revision++
	writeJSON(w, s)
}

// Read only identity supplied on the trusted loopback Serve connection.
// Administration may inspect it; the sign-in test reveals only the requesting
// device's identity and never another unlinked account's data.
func (a *authentication) observedTailscaleLogin(r *http.Request) string {
	if a.mode != "tailscale" {
		return ""
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return ""
	}
	login := strings.ToLower(strings.TrimSpace(decodeHeader(r.Header.Get("Tailscale-User-Login"))))
	if len(login) > 254 || !strings.Contains(login, "@") {
		return ""
	}
	return login
}

func (h *handler) testNotification(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	var body struct {
		Destination string `json:"destination"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if !h.requireAdmin(w, r) {
		return
	}
	if body.Destination != "ntfy" && body.Destination != "webhook" {
		jsonReply(w, 400, "Choose ntfy or webhook.")
		return
	}
	// A test sends a real message, only in response to this explicit action.
	if !h.notificationTests.allow(body.Destination) {
		jsonReply(w, 429, "A test was attempted recently. Please wait before trying again.")
		return
	}
	n, ok := h.notifier.(*notificationRouter)
	if !ok {
		jsonReply(w, 503, "Notification testing is unavailable.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	if err := n.send(ctx, Notification{Kind: "test", Title: "Anchor test notification", Message: "Your Anchor notification connection is working.", Priority: 3}, body.Destination); err != nil {
		jsonReply(w, 502, "Test delivery failed. Check the destination's delivery status and settings.")
		return
	}
	jsonReply(w, 200, "Test notification accepted by the service. Check your subscribed topic or channel.")
}
