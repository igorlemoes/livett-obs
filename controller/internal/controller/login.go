// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package controller

import (
	"context"
	"time"
)

// startLoginWatch re-checks the account every few seconds while the panel waits for a sign-in (the user signs in
// in the Chrome window that was brought forward). As soon as the session is valid (copying the web login to the
// LIVE service's domain when needed) the phase becomes "ready" and the sign-in window goes away for good: the user
// does not have to press "Conectar" again. One watcher at a time.
func (c *Controller) startLoginWatch() {
	c.st.Lock()
	if c.loginCancel != nil {
		c.st.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.loginCancel = cancel
	c.st.Unlock()
	go func() {
		defer func() {
			c.st.Lock()
			c.loginCancel = nil
			c.st.Unlock()
			cancel()
		}()
		for c.getPhase() == Login {
			if !sleepCtx(ctx, 3*time.Second) {
				return
			}
			if c.getPhase() != Login {
				return
			}
			if c.loginSucceeded(ctx) {
				return
			}
		}
	}()
}

// loginSucceeded reports (and applies) a valid session.
func (c *Controller) loginSucceeded(ctx context.Context) bool {
	if as, ok := c.opts.Browser.(AccountSession); ok && as.LoginPending() {
		return false // the window is still closing: wait for its ticket and identity instead of racing it
	}
	c.consumeLogin(ctx) // the sign-in window finished: exchange its ticket for a LIVE-service session
	result, err := c.raw.Call(ctx, "check", nil)
	if err != nil {
		return false
	}
	account := asObj(result)
	if !isZero(account["code"]) {
		syncer, ok := c.opts.Browser.(SessionSyncer)
		if !ok {
			return false
		}
		if synced, err := syncer.SyncWebcastSession(ctx); err != nil || !synced {
			return false
		}
		again, err := c.raw.Call(ctx, "check", nil)
		if err != nil {
			return false
		}
		if account = asObj(again); !isZero(account["code"]) {
			return false
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.getPhase() != Login {
		return true
	}
	c.st.Lock()
	c.account = account
	c.st.Unlock()
	c.readAboutMe(ctx)
	c.recordAccount(ctx, c.takePending())
	c.browser.ReleaseLogin(ctx) // the sign-in window goes away
	c.setState(Ready, "Conectado. Confira a cena, o áudio e o título antes de iniciar.")
	return true
}
