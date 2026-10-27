// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"tiktoklive/internal/vault"
)

// The account list: each account the user signed in with keeps its session (sealed, see
// package vault) so switching is a click, not a new sign-in. The managed Chrome holds one account at a time.

func (c *Controller) accountSession() (AccountSession, *vault.Store, bool) {
	as, ok := c.opts.Browser.(AccountSession)
	if !ok || c.opts.Vault == nil {
		return nil, nil, false
	}
	return as, c.opts.Vault, true
}

// refreshAccounts rebuilds the list the snapshot shows (names, handles, photos; never a session).
func (c *Controller) refreshAccounts() {
	if c.opts.Vault == nil {
		return
	}
	list := c.opts.Vault.List()
	c.st.Lock()
	defer c.st.Unlock()
	out := make([]Obj, 0, len(list))
	for _, a := range list {
		out = append(out, Obj{"id": a.ID, "name": a.Name, "handle": a.Handle, "avatar": a.Avatar,
			"expired": a.Expired, "active": a.ID == c.activeAccount})
	}
	c.accounts = out
}

// accountLog appends one line about an account operation to runtime/accounts/events.log (never a cookie, ticket or
// full id: only short prefixes), so a wrong result can be explained afterwards. Kept small: it is trimmed when large.
func (c *Controller) accountLog(format string, args ...any) {
	path := filepath.Join(c.root, "runtime", "accounts", "events.log")
	os.MkdirAll(filepath.Dir(path), 0o755)
	if info, err := os.Stat(path); err == nil && info.Size() > 64<<10 {
		os.Remove(path)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}

func short(s string) string {
	if len(s) > 6 {
		return s[:6]
	}
	return s
}

// Idle reports that nothing is being decided or broadcast: no LIVE, no room to confirm, no operation under way.
// The controller may quit on its own (when OBS is gone) only in this state.
func (c *Controller) Idle() bool {
	c.st.Lock()
	defer c.st.Unlock()
	switch c.phase {
	case Closed:
		return true // the LIVE ended and its end was confirmed; the room is only kept for the record
	case Disconnected, Ready, Login:
		return c.binding == nil
	}
	return false
}

// accountGateLocked: accounts change only when no LIVE exists or is being decided (that would cut its control).
// c.mu must be held.
func (c *Controller) accountGateLocked() error {
	switch c.getPhase() {
	case Disconnected, Ready, Login, Closed:
	default:
		return perr("Só é possível mudar de conta quando não há LIVE em andamento nem pendência a resolver.")
	}
	c.st.Lock()
	if c.phase == Closed {
		// The end was verified at the TikTok and the room dropped: nothing is left to confirm for the old account.
		c.binding, c.output = nil, nil
	}
	hasBinding := c.binding != nil
	c.st.Unlock()
	if hasBinding {
		return perr("Há uma LIVE anterior a confirmar. Conecte e confirme que ela terminou antes de mudar de conta.")
	}
	return nil
}

// consumeLogin takes what a finished sign-in window left (identity + PKCE ticket) and exchanges the ticket for a
// session of the LIVE service. The ticket never reaches a trace: the untraced browser is used. A failure only
// means the older route (copying the web login to the LIVE domain) is still needed.
func (c *Controller) consumeLogin(ctx context.Context) {
	as, ok := c.opts.Browser.(AccountSession)
	if !ok {
		return
	}
	ticket := as.TakeLogin()
	if ticket == nil {
		return
	}
	if ticket.Ticket != "" {
		reply, err := c.raw.Call(ctx, "pkce", Obj{"ticket": ticket.Ticket, "verifier": ticket.Verifier, "challenge": ticket.Challenge})
		r := asObj(reply)
		event := Obj{"ok": err == nil && isTrue(r["ok"]), "message": r["message"], "errorCode": r["errorCode"]}
		c.accountLog("pkce: ok=%v erro=%v identidade do login: @%s id=%s…", event["ok"], err != nil, ticket.Identity.Handle, short(ticket.Identity.ID))
		if err != nil {
			event["error"] = "call failed"
		}
		c.emit("controller", "pkce_login", event)
		if user := asObj(r["user"]); ticket.Identity.ID == "" && str(user, "id") != "" {
			ticket.Identity = AccountIdentity{ID: str(user, "id"), Name: str(user, "name"), Handle: str(user, "handle"), Avatar: str(user, "avatar")}
		}
	}
	c.st.Lock()
	c.pendingTicket = ticket
	c.st.Unlock()
}

func (c *Controller) takePending() *LoginTicket {
	c.st.Lock()
	defer c.st.Unlock()
	t := c.pendingTicket
	c.pendingTicket = nil
	return t
}

// recordAccount saves the session the browser holds right now under the right account (adding it when new) and marks
// it as the active one. c.mu must be held. Failures are silent: the list is a convenience, never a requirement.
func (c *Controller) recordAccount(ctx context.Context, ticket *LoginTicket) {
	as, v, ok := c.accountSession()
	if !ok {
		return
	}
	fp := as.Fingerprint(ctx)
	if fp == "" {
		return
	}
	cookies, err := as.AccountCookies(ctx)
	if err != nil || len(cookies) == 0 {
		return
	}
	prof := c.profile(ctx) // name, @handle and photo as the LIVE service shows them
	acc, known := v.ByFingerprint(fp)
	if !known && prof.Handle != "" {
		// The same account signing in again (a new session): recognised by its @handle, no need to ask who it is.
		for _, a := range v.List() {
			if strings.EqualFold(a.Handle, prof.Handle) {
				acc, known = a, true
				break
			}
		}
	}
	c.accountLog("record: sessão %s, já salva=%v, ticket=%v, perfil=@%s", short(fp), known, ticket != nil, prof.Handle)
	fill := func(i AccountIdentity) { // only what is still empty
		if acc.Name == "" {
			acc.Name = i.Name
		}
		if acc.Handle == "" {
			acc.Handle = i.Handle
		}
		if acc.Avatar == "" {
			acc.Avatar = i.Avatar
		}
	}
	if !known {
		// Only the numeric id is needed here: from the sign-in window's result or, failing that, from the passport.
		var who AccountIdentity
		if ticket != nil {
			who = ticket.Identity
		}
		if !vault.ValidID(who.ID) {
			if got, err := as.Identify(ctx); err == nil {
				who = got
			}
		}
		acc = vault.Account{ID: who.ID}
		if !vault.ValidID(acc.ID) {
			acc.ID = "fp-" + fp
		}
		if existing, found := v.Find(acc.ID); found {
			acc = existing // the same account signed in again: keep its entry, replace its session
		}
		fill(who)
	}
	if known && strings.HasPrefix(acc.ID, "fp-") {
		// Saved under a placeholder id (the real one could not be read then): give it the real id now.
		if got, err := as.Identify(ctx); err == nil && vault.ValidID(got.ID) && got.ID != acc.ID {
			v.Remove(acc.ID)
			acc.ID = got.ID
		}
	}
	if prof.Name != "" || prof.Handle != "" {
		// The LIVE service's profile wins (the passport only knows the account holder's name and personal photo), and
		// it is read on every connection, so the photo link never goes stale.
		if prof.Name != "" {
			acc.Name = prof.Name
		}
		if prof.Handle != "" {
			acc.Handle = prof.Handle
		}
		if prof.Avatar != "" {
			acc.Avatar = prof.Avatar
		}
	} else if acc.Name == "" && acc.Handle == "" {
		if got, err := as.Identify(ctx); err == nil { // nothing better available: at least the passport's data
			fill(got)
		}
	}
	acc.Fingerprint = fp
	c.accountLog("record: gravando conta id=%s… @%s (nome=%v foto=%v) com a sessão %s", short(acc.ID), acc.Handle, acc.Name != "", acc.Avatar != "", short(fp))
	if err := v.Save(acc, cookies); err != nil {
		c.emit("controller", "account_save_failed", Obj{})
		return
	}
	c.st.Lock()
	c.activeAccount = acc.ID
	c.st.Unlock()
	c.refreshAccounts()
}

// profile reads the signed-in account's display name, @handle and photo from the LIVE service (empty on failure).
func (c *Controller) profile(ctx context.Context) AccountIdentity {
	reply, err := c.raw.Call(ctx, "profile", nil)
	r := asObj(reply)
	if err != nil || !isZero(r["code"]) {
		return AccountIdentity{}
	}
	avatar := str(r, "avatar")
	if !strings.HasPrefix(avatar, "https://") {
		avatar = ""
	}
	return AccountIdentity{Name: str(r, "name"), Handle: str(r, "handle"), Avatar: avatar}
}

// UseAccount switches the managed browser to a saved account: the current session is saved first, then the chosen
// one is restored. If TikTok no longer accepts it the sign-in window opens and the entry is marked as expired.
func (c *Controller) UseAccount(ctx context.Context, id string) (Obj, error) {
	as, v, ok := c.accountSession()
	if !ok {
		return nil, perr("A lista de contas não está disponível.")
	}
	target, found := v.Find(id)
	if !found {
		return nil, perr("Conta não encontrada.")
	}
	cookies, err := v.Cookies(id)
	if err != nil {
		return nil, perr("Não foi possível abrir a sessão salva desta conta: " + err.Error())
	}
	c.mu.Lock()
	if err := c.accountGateLocked(); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	if err = c.browser.Connect(ctx); err == nil {
		c.recordAccount(ctx, nil) // keep the account being left
		if err = as.SignOut(ctx); err == nil {
			err = as.RestoreCookies(ctx, cookies)
		}
		c.accountLog("use: conta id=%s… restaurada err=%v, sessão agora=%q", short(target.ID), err != nil, short(as.Fingerprint(ctx)))
	}
	if err == nil {
		c.st.Lock()
		c.account, c.activeAccount = Obj{}, target.ID
		c.st.Unlock()
		c.emit("controller", "account_switched", Obj{})
	}
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	state, err := c.Connect(ctx)
	if err == nil && c.getPhase() == Login {
		v.MarkExpired(id)
		c.refreshAccounts()
	}
	return state, err
}

// AddAccount keeps the current account and opens the sign-in window for another one.
func (c *Controller) AddAccount(ctx context.Context) (Obj, error) {
	as, _, ok := c.accountSession()
	if !ok {
		return nil, perr("A lista de contas não está disponível.")
	}
	c.mu.Lock()
	if err := c.accountGateLocked(); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	err := c.browser.Connect(ctx)
	if err == nil {
		c.recordAccount(ctx, nil)
		err = as.SignOut(ctx)
		left, _ := as.AccountCookies(ctx)
		c.accountLog("add: SignOut err=%v, cookies do TikTok que restaram=%d, sessão restante=%q", err, len(left), short(as.Fingerprint(ctx)))
	}
	if err == nil {
		c.st.Lock()
		c.account, c.activeAccount = Obj{}, ""
		c.st.Unlock()
		c.refreshAccounts()
		c.emit("controller", "account_add", Obj{})
	}
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return c.Connect(ctx) // no session left: this shows the sign-in window
}

// Logout signs the current account out and forgets its saved session ("Sair").
func (c *Controller) Logout(ctx context.Context) (Obj, error) {
	as, v, ok := c.accountSession()
	if !ok {
		return nil, perr("A lista de contas não está disponível.")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.accountGateLocked(); err != nil {
		return nil, err
	}
	if err := c.browser.Connect(ctx); err != nil {
		return nil, err
	}
	if acc, known := v.ByFingerprint(as.Fingerprint(ctx)); known {
		v.Remove(acc.ID)
	}
	if err := as.SignOut(ctx); err != nil {
		return nil, err
	}
	c.browser.ReleaseLogin(ctx)
	c.st.Lock()
	c.account, c.activeAccount = Obj{}, ""
	c.st.Unlock()
	c.refreshAccounts()
	c.emit("controller", "account_logout", Obj{})
	c.setState(Disconnected, "Conta desconectada. Clique em Conectar para entrar de novo ou escolha outra conta.")
	return c.Snapshot(), nil
}

// RemoveAccount forgets a saved account. Removing the active one signs it out.
func (c *Controller) RemoveAccount(ctx context.Context, id string) (Obj, error) {
	_, v, ok := c.accountSession()
	if !ok {
		return nil, perr("A lista de contas não está disponível.")
	}
	if _, found := v.Find(id); !found {
		return nil, perr("Conta não encontrada.")
	}
	c.st.Lock()
	active := c.activeAccount == id
	c.st.Unlock()
	if active {
		return c.Logout(ctx)
	}
	if err := v.Remove(id); err != nil {
		return nil, perr("Não foi possível remover a conta.")
	}
	c.refreshAccounts()
	return c.Snapshot(), nil
}
