// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// Package impoll is single-step IM polling with an injected authenticated, read-only transport. No background retries, room creation, credentials or runtime dependency on any TikTok application. The caller must
// establish ownership of the room and authenticity of the transport.
package impoll

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"tiktoklive/internal/guard"
	"tiktoklive/internal/wire"
)

// Guard is what the poller needs from the shutdown coordinator (*guard.Guard implements it).
type Guard interface {
	RoomID() string
	SessionID() string
	RestartBlocked() bool
	Handle(ctx context.Context, s guard.Signal) (*guard.Result, error)
}

// Fetch performs one GET and returns the raw body.
type Fetch func(ctx context.Context, url string) ([]byte, error)

type Result struct {
	Received        int
	Dispatched      int
	ShutdownResults []*guard.Result
	NextIntervalMS  uint64
	SocketOffered   bool
}

type Poller struct {
	guard    Guard
	fetch    Fetch
	browser  [][2]string // ordered browser hints
	observer func(raw []byte, response *wire.Response)

	mu          sync.Mutex
	seen        map[string]bool
	order       []string
	limit       int
	cursor      string
	internalExt string
}

var allowedBrowser = map[string]bool{"browser_language": true, "browser_platform": true, "browser_name": true,
	"browser_version": true, "browser_online": true, "screen_width": true, "screen_height": true, "tz_name": true,
	"cookie_enabled": true}

// New validates its inputs. Only browser environment hints may be supplied: room/session/routing parameters
// cannot be overridden through the caller's query values. dedupLimit 0 means the default (1024).
func New(g Guard, fetch Fetch, browserParams [][2]string, dedupLimit int, observer func([]byte, *wire.Response)) (*Poller, error) {
	room := g.RoomID()
	if room == "" || strings.Trim(room, "0") == "" || strings.Trim(room, "0123456789") != "" {
		return nil, errors.New("a concrete numeric room is required")
	}
	if dedupLimit == 0 {
		dedupLimit = 1024
	}
	if dedupLimit < 1 || dedupLimit > 10000 {
		return nil, errors.New("invalid deduplication bound")
	}
	for _, kv := range browserParams {
		if !allowedBrowser[kv[0]] {
			return nil, errors.New("unexpected browser parameter")
		}
	}
	return &Poller{guard: g, fetch: fetch, browser: browserParams, observer: observer, seen: map[string]bool{}, limit: dedupLimit}, nil
}

func quotePlus(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '.', c == '-', c == '~':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// URL builds the im/fetch request. The version overrides the SDK default (180800);
// did_rule=3 is the SDK fallback when no device ID is supplied (acceptance for anchor sessions stays an
// integration question).
func (p *Poller) URL() string {
	pairs := append([][2]string{}, p.browser...)
	pairs = append(pairs, [][2]string{
		{"aid", "8311"}, {"app_name", "tiktok_live_studio"}, {"channel", "studio"}, {"live_id", "12"},
		{"version_code", "300400"}, {"force_https", "false"}, {"device_platform", "web"}, {"app_language", "en"},
		{"sup_ws_ds_opt", "1"}, {"resp_content_type", "protobuf"}, {"did_rule", "3"}, {"device_id", ""},
		{"identity", "anchor"}, {"room_id", p.guard.RoomID()}, {"fetch_rule", "1"}, {"last_rtt", "0"},
		{"cursor", p.cursor}, {"internal_ext", p.internalExt}, {"history_comment_cursor", ""},
	}...)
	parts := make([]string, 0, len(pairs))
	for _, kv := range pairs {
		parts = append(parts, quotePlus(kv[0])+"="+quotePlus(kv[1]))
	}
	return "https://webcast.tiktokv.com/webcast/im/fetch/?" + strings.Join(parts, "&")
}

func (p *Poller) fetchBody(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	return p.fetch(ctx, p.URL())
}

// Cursor exposes the current cursor (used by tests and telemetry).
func (p *Poller) Cursor() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cursor
}

// ObserveOnce fetches one batch for passive capture only: nothing is dispatched to the guard.
func (p *Poller) ObserveOnce(ctx context.Context) ([]byte, *wire.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	raw, err := p.fetchBody(ctx)
	if err != nil {
		return nil, nil, err
	}
	if len(raw) > wire.MaxFrame {
		return nil, nil, &wire.Error{Msg: "Invalid HTTP message body"}
	}
	if len(raw) == 0 {
		return raw, nil, nil
	}
	response, err := wire.DecodeResponse(raw)
	if err != nil {
		return nil, nil, err
	}
	if response.Cursor != "" {
		p.cursor = response.Cursor
	}
	p.internalExt = response.InternalExt
	return raw, response, nil
}

var ErrShutdownLatched = errors.New("broadcast shutdown is already latched")

// PollOnce fetches one batch and hands every classified, de-duplicated signal for this room to the guard.
func (p *Poller) PollOnce(ctx context.Context) (*Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.guard.RestartBlocked() {
		return nil, ErrShutdownLatched
	}
	raw, err := p.fetchBody(ctx)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || len(raw) > wire.MaxFrame {
		return nil, &wire.Error{Msg: "Invalid HTTP message body"}
	}
	response, err := wire.DecodeResponse(raw)
	if err != nil {
		return nil, err
	}
	if p.observer != nil {
		func() {
			defer func() { recover() }() // passive recording must never block a shutdown signal
			p.observer(raw, response)
		}()
	}
	// Decode the whole candidate batch before delivering any signal.
	var candidates []*guard.Signal
	for _, m := range response.Messages {
		signal, err := wire.Classify(m, p.guard.SessionID())
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, signal)
	}
	result := &Result{Received: len(response.Messages)}
	for _, signal := range candidates {
		if signal == nil || signal.RoomID != p.guard.RoomID() || p.seen[signal.EvidenceID] {
			continue
		}
		shutdown, err := p.guard.Handle(ctx, *signal)
		if err != nil {
			return nil, err
		}
		p.seen[signal.EvidenceID] = true
		p.order = append(p.order, signal.EvidenceID)
		if len(p.order) > p.limit {
			delete(p.seen, p.order[0])
			p.order = p.order[1:]
		}
		result.Dispatched++
		result.ShutdownResults = append(result.ShutdownResults, shutdown)
	}
	if response.Cursor != "" {
		p.cursor = response.Cursor
	}
	p.internalExt = response.InternalExt
	result.NextIntervalMS = response.FetchInterval
	if result.NextIntervalMS < 1000 {
		result.NextIntervalMS = 1000
	}
	result.SocketOffered = response.FetchType == 1
	return result, nil
}
