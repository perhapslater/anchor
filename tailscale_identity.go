package main

import (
	"errors"
	"net/http"
	"strings"
	"time"
)

// Identity linking is independent of password sign-in. Preserve the authenticated
// administrator's current local session for a self-link; other sessions for the
// changed account still end. This path cannot edit passwords, roles or enablement.
func (p *persistentState) saveTailscaleIdentity(id, login, actor, sessionHash string, revision int64) error {
	return p.saveTailscaleIdentityChecked(id, login, actor, sessionHash, revision, nil)
}

func (p *persistentState) saveTailscaleIdentityChecked(id, login, actor, sessionHash string, revision int64, check authorizationCheck) error {
	login = strings.ToLower(strings.TrimSpace(login))
	if len(login) > 254 || login != "" && (!strings.Contains(login, "@") || strings.ContainsAny(login, " \t\r\n")) {
		return errors.New("enter the email address used to sign into Tailscale")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if check != nil {
		if err := check(); err != nil {
			return err
		}
	}
	u, ok := p.user(id)
	if !ok {
		return errors.New("account not found")
	}
	if login == "" && u.password == "" {
		return errors.New("keep a local password or a Tailscale identity for this person")
	}
	tx, err := p.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE settings SET revision=revision+1 WHERE id=1 AND revision=?`, revision)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errConflict
	}
	if u.Tailscale != login {
		var value any
		if login != "" {
			value = login
		}
		if _, err = tx.Exec(`UPDATE users SET tailscale=?,auth_version=auth_version+1 WHERE id=?`, value, id); err != nil {
			return errors.New("could not save identity; check whether it is already linked to another person")
		}
		if _, err = tx.Exec(`UPDATE user_photos SET tailscale_url='',revision=revision+1 WHERE user_id=? AND tailscale_url!=''`, id); err != nil {
			return err
		}
		keep := ""
		if id == actor {
			keep = sessionHash
		}
		if _, err = tx.Exec(`DELETE FROM sessions WHERE user_id=? AND hash<>?`, id, keep); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`INSERT INTO audit(at,actor,action) VALUES(?,?,?)`, time.Now().Unix(), actor, "Tailscale identity saved: "+id); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return p.reload()
}

func (h *handler) adminTailscaleIdentity(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	var body struct {
		ID       string `json:"id"`
		Email    string `json:"email"`
		Revision int64  `json:"revision"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	who := authFrom(r)
	p := h.store.persistent
	h.saved(w, p.saveTailscaleIdentityChecked(body.ID, body.Email, who.user.ID, who.hash, body.Revision, p.authorizationGuard(who, true)))
}
