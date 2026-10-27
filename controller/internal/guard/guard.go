// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// Package guard is the independent shutdown coordinator.
// No TikTok/OBS network adapter lives here: the caller injects the three operations.
package guard

import (
	"context"
	"errors"
	"sync"
	"time"
)

type SignalKind string

const (
	Warning     SignalKind = "warning"
	Restriction SignalKind = "restriction"
	RemoteEnd   SignalKind = "remote_end"
)

// Signal must be validated/decoded upstream before it is constructed.
type Signal struct {
	RoomID, SessionID string
	Kind              SignalKind
	EvidenceID        string
}

type Result struct {
	MediaStopped bool
	RoomEnded    bool
	Errors       []string
}

type Config struct {
	RoomID, SessionID string
	StopMedia         func(context.Context) (bool, error)
	RequestRoomEnd    func(context.Context) error
	VerifyRoomEnded   func(context.Context) (bool, error)
	OperationTimeout  time.Duration
}

// Guard is one instance per broadcast. A stop stays latched, including after failures.
type Guard struct {
	cfg     Config
	mu      sync.Mutex
	started bool
	done    chan struct{}
	result  Result
	Trigger *Signal
}

func New(cfg Config) (*Guard, error) {
	if cfg.RoomID == "" || cfg.SessionID == "" || cfg.OperationTimeout <= 0 ||
		cfg.StopMedia == nil || cfg.RequestRoomEnd == nil || cfg.VerifyRoomEnded == nil {
		return nil, errors.New("a room, session, operations and positive timeout are required")
	}
	return &Guard{cfg: cfg, done: make(chan struct{})}, nil
}

func (g *Guard) RoomID() string    { return g.cfg.RoomID }
func (g *Guard) SessionID() string { return g.cfg.SessionID }

func (g *Guard) RestartBlocked() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.started
}

// Handle returns (nil, nil) for a signal from another room/session. Concurrent signals share one shutdown. The
// shutdown runs on its own goroutine, so a caller that gives up (ctx) never cancels it.
func (g *Guard) Handle(ctx context.Context, s Signal) (*Result, error) {
	if s.RoomID != g.cfg.RoomID || s.SessionID != g.cfg.SessionID {
		return nil, nil
	}
	if s.Kind == "" || s.EvidenceID == "" {
		return nil, errors.New("a classified signal and evidence reference are required")
	}
	g.mu.Lock()
	if !g.started {
		g.started = true
		trigger := s
		g.Trigger = &trigger
		go g.closeAll()
	}
	g.mu.Unlock()
	select {
	case <-g.done:
		result := g.result
		return &result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func classify(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "error"
}

func (g *Guard) closeAll() {
	defer close(g.done)
	var wg sync.WaitGroup
	var stopped, ended bool
	var mediaErrors, roomErrors []string
	wg.Add(2)
	// Failure or delay on one side must not prevent the other side's attempt.
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), g.cfg.OperationTimeout)
		defer cancel()
		ok, err := g.cfg.StopMedia(ctx)
		switch {
		case err != nil:
			mediaErrors = []string{"media_stop:" + classify(err)}
		case !ok:
			mediaErrors = []string{"media_stop_unconfirmed"}
		default:
			stopped = true
		}
	}()
	go func() {
		defer wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), g.cfg.OperationTimeout)
		if err := g.cfg.RequestRoomEnd(ctx); err != nil {
			roomErrors = append(roomErrors, "room_end_request:"+classify(err))
		}
		cancel()
		// A request failure may mean its response was lost; read state anyway.
		ctx, cancel = context.WithTimeout(context.Background(), g.cfg.OperationTimeout)
		defer cancel()
		ok, err := g.cfg.VerifyRoomEnded(ctx)
		switch {
		case err != nil:
			roomErrors = append(roomErrors, "room_end_verify:"+classify(err))
		case !ok:
			roomErrors = append(roomErrors, "room_end_unconfirmed")
		default:
			ended = true
		}
	}()
	wg.Wait()
	g.result = Result{MediaStopped: stopped, RoomEnded: ended, Errors: append(mediaErrors, roomErrors...)}
}
