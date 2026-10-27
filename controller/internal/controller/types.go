// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// Package controller is the stateful manual broadcast controller: no publication
// occurs during connect/check, a failed or uncertain creation latches into a recovery state that survives a crash,
// the OBS destination key is always restored or wiped, and an automatic end is wired to TikTok's own signals.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"tiktoklive/internal/obs"
	"tiktoklive/internal/wire"
)

// Obj is a decoded JSON object. Numbers may be json.Number, float64 or int: use the helpers below to read them.
type Obj = map[string]any

// PanelError is a user-facing refusal or failure (its message is shown in the panel).
type PanelError struct{ Msg string }

func (e *PanelError) Error() string { return e.Msg }

func perr(msg string) error { return &PanelError{msg} }

func perrf(format string, args ...any) error { return &PanelError{fmt.Sprintf(format, args...)} }

// Browser is the authenticated Chrome bridge (session.Session behind an adapter; fakes in tests).
type Browser interface {
	Connect(ctx context.Context) error
	Call(ctx context.Context, method string, data any) (any, error)
	ShowLogin(ctx context.Context) error
	ReleaseLogin(ctx context.Context)
}

// AccountIdentity is who TikTok says is signed in (never a secret).
type AccountIdentity struct{ ID, Name, Handle, Avatar string }

// LoginTicket is what the finished sign-in window hands over: the identity and the PKCE ticket that the LIVE
// service exchanges for its own session.
type LoginTicket struct {
	Identity                    AccountIdentity
	Ticket, Verifier, Challenge string
}

// AccountSession is optional on a Browser: everything the account list needs from the managed Chrome.
type AccountSession interface {
	SignOut(ctx context.Context) error
	AccountCookies(ctx context.Context) ([]map[string]any, error)
	RestoreCookies(ctx context.Context, cookies []map[string]any) error
	Fingerprint(ctx context.Context) string
	Identify(ctx context.Context) (AccountIdentity, error)
	TakeLogin() *LoginTicket
	LoginPending() bool
}

// SessionSyncer is optional on a Browser: copy the web login to the LIVE service's domain. It reports whether
// anything changed (then the account check is repeated).
type SessionSyncer interface {
	SyncWebcastSession(ctx context.Context) (bool, error)
}

// Obs is the connection to the plugin control API in OBS.
type Obs interface {
	obs.Caller
	Close()
}

// Recorder is the optional passive signal capture (telemetry). Nothing in it may affect a broadcast.
type Recorder interface {
	Event(source, kind string, data Obj)
	Snapshot() Obj
	Begin(ctx context.Context, title string, obsInfo Obj) error
	AttachRoom(ctx context.Context, binding Binding, observeIM bool)
	IMBatch(raw []byte, response *wire.Response, side Obj, seen map[string]bool)
	IMError(err error, side Obj)
	Finish()
}

// Binding is the room identity recorded for recovery (the journal stores it).
type Binding struct {
	RoomID     string `json:"room_id"`
	PingRoomID string `json:"ping_room_id"`
	StreamID   string `json:"stream_id"`
}

func (b Binding) obj() Obj {
	return Obj{"room_id": b.RoomID, "ping_room_id": b.PingRoomID, "stream_id": b.StreamID}
}

// ---- tolerant readers for JSON-decoded values

func asObj(v any) Obj {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return Obj{}
}

func str(m Obj, key string) string {
	s, _ := m[key].(string)
	return s
}

func isTrue(v any) bool { b, ok := v.(bool); return ok && b }

// number reads any JSON-ish number; ok is false for non-numbers (bools included).
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	}
	return 0, false
}

func numOr(v any, def float64) float64 {
	if f, ok := number(v); ok {
		return f
	}
	return def
}

// isZero reports whether v is the number 0 (the TikTok status_code/code success value).
func isZero(v any) bool { f, ok := number(v); return ok && f == 0 }

func isInt(v any, want int64) bool {
	f, ok := number(v)
	return ok && f == float64(want) && f == math.Trunc(f)
}

// normalize turns json.Number values into int64/float64 so snapshots print as plain JSON numbers.
func normalize(v any) any {
	switch n := v.(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i
		}
		if f, err := n.Float64(); err == nil {
			return f
		}
		return n.String()
	case map[string]any:
		out := make(map[string]any, len(n))
		for k, x := range n {
			out[k] = normalize(x)
		}
		return out
	case []any:
		out := make([]any, len(n))
		for i, x := range n {
			out[i] = normalize(x)
		}
		return out
	}
	return v
}

// IsTikTokLeftover is true for a custom RTMP destination pointing at TikTok's ingest (a used, now stale key).
func IsTikTokLeftover(service Obj) bool {
	if service == nil || str(service, "streamServiceType") != "rtmp_custom" {
		return false
	}
	server := strings.ToLower(str(asObj(service["streamServiceSettings"]), "server"))
	if !strings.HasPrefix(server, "rtmp") || strings.Count(server, "/") < 2 {
		return false
	}
	return strings.Contains(strings.Split(server, "/")[2], "tiktok")
}

// BlankService is the destination that holds no key at all.
func blankService() Obj {
	return Obj{"streamServiceType": "rtmp_custom",
		"streamServiceSettings": Obj{"server": "", "key": "", "use_auth": false}}
}
