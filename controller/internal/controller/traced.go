// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package controller

import (
	"context"
	"errors"
	"net"
	"time"

	"tiktoklive/internal/obs"
	"tiktoklive/internal/session"
)

// tracedBrowser records every call/result of the browser bridge (redaction happens in the trace writer).
type tracedBrowser struct {
	inner  Browser
	source string
	emit   func(source, kind string, data Obj)
}

func (t *tracedBrowser) Connect(ctx context.Context) error   { return t.inner.Connect(ctx) }
func (t *tracedBrowser) ShowLogin(ctx context.Context) error { return t.inner.ShowLogin(ctx) }
func (t *tracedBrowser) ReleaseLogin(ctx context.Context)    { t.inner.ReleaseLogin(ctx) }

func (t *tracedBrowser) Call(ctx context.Context, method string, data any) (any, error) {
	started := time.Now()
	result, err := t.inner.Call(ctx, method, data)
	entry := Obj{"name": method, "args": data, "ms": time.Since(started).Milliseconds()}
	if err != nil {
		entry["error"] = errName(err)
	} else {
		entry["result"] = result
	}
	t.emit(t.source, "call", entry)
	return result, err
}

// tracedObs does the same for OBS control API requests.
type tracedObs struct {
	inner  Obs
	source string
	emit   func(source, kind string, data Obj)
}

func (t *tracedObs) Close() { t.inner.Close() }

func (t *tracedObs) Call(ctx context.Context, name string, data map[string]any) (map[string]any, error) {
	started := time.Now()
	result, err := t.inner.Call(ctx, name, data)
	entry := Obj{"name": name, "args": data, "ms": time.Since(started).Milliseconds()}
	if err != nil {
		entry["error"] = errName(err)
	} else {
		entry["result"] = result
	}
	t.emit(t.source, "call", entry)
	if err != nil && unreachable(err) {
		return result, errObsUnreachable // say it in words instead of a generic failure
	}
	return result, err
}

// unreachable: OBS (the plugin API) cannot be reached at all. A timeout is not included: its outcome is unknown.
func unreachable(err error) bool {
	var op *net.OpError
	return errors.Is(err, obs.ErrNotRunning) || errors.As(err, &op)
}

func errName(err error) string {
	switch err.(type) {
	case *PanelError:
		return "PanelError"
	case *session.Error:
		return "SessionError"
	}
	return "error"
}

// SessionBrowser adapts session.Session (which needs the panel script) to the Browser interface.
type SessionBrowser struct {
	S       *session.Session
	PanelJS string
}

func (b *SessionBrowser) Connect(ctx context.Context) error { return b.S.Connect(ctx, b.PanelJS) }
func (b *SessionBrowser) Call(ctx context.Context, method string, data any) (any, error) {
	return b.S.Call(ctx, method, data)
}
func (b *SessionBrowser) ShowLogin(ctx context.Context) error { return b.S.ShowLogin(ctx) }
func (b *SessionBrowser) ReleaseLogin(ctx context.Context)    { b.S.ReleaseLogin(ctx) }

// The account list's needs, forwarded to the session behind the adapter.
func (b *SessionBrowser) SignOut(ctx context.Context) error { return b.S.SignOut(ctx) }
func (b *SessionBrowser) AccountCookies(ctx context.Context) ([]map[string]any, error) {
	return b.S.AccountCookies(ctx)
}
func (b *SessionBrowser) RestoreCookies(ctx context.Context, cookies []map[string]any) error {
	return b.S.RestoreCookies(ctx, cookies)
}
func (b *SessionBrowser) Fingerprint(ctx context.Context) string { return b.S.Fingerprint(ctx) }
func (b *SessionBrowser) Identify(ctx context.Context) (AccountIdentity, error) {
	info, err := b.S.Identify(ctx)
	return AccountIdentity{ID: info.ID, Name: info.Name, Handle: info.Handle, Avatar: info.Avatar}, err
}
func (b *SessionBrowser) LoginPending() bool { return b.S.LoginPending() }
func (b *SessionBrowser) TakeLogin() *LoginTicket {
	login := b.S.TakeLogin()
	if login == nil {
		return nil
	}
	i := login.Info
	return &LoginTicket{Identity: AccountIdentity{ID: i.ID, Name: i.Name, Handle: i.Handle, Avatar: i.Avatar},
		Ticket: login.Ticket, Verifier: login.Verifier, Challenge: login.Challenge}
}

// SyncWebcastSession lets the controller's optional session sync reach the session behind the adapter.
func (b *SessionBrowser) SyncWebcastSession(ctx context.Context) (bool, error) {
	return b.S.SyncWebcastSession(ctx)
}
