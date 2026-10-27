// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package guard

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fixture struct {
	mu    sync.Mutex
	calls []string
}

func (f *fixture) add(c string) { f.mu.Lock(); f.calls = append(f.calls, c); f.mu.Unlock() }
func (f *fixture) count(c string) (n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.calls {
		if x == c {
			n++
		}
	}
	return
}

func (f *fixture) guard(t *testing.T, mutate func(*Config)) *Guard {
	cfg := Config{
		RoomID: "room-A", SessionID: "session-1", OperationTimeout: 50 * time.Millisecond,
		StopMedia:       func(context.Context) (bool, error) { f.add("stop"); return true, nil },
		RequestRoomEnd:  func(context.Context) error { f.add("end"); return nil },
		VerifyRoomEnded: func(context.Context) (bool, error) { f.add("verify"); return true, nil },
	}
	if mutate != nil {
		mutate(&cfg)
	}
	g, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func sig(room, session string, kind SignalKind) Signal {
	return Signal{RoomID: room, SessionID: session, Kind: kind, EvidenceID: "synthetic-evidence"}
}

func TestForeignRoomAndPreviousSessionCannotStopCurrentLive(t *testing.T) {
	f := &fixture{}
	g := f.guard(t, nil)
	for _, s := range []Signal{sig("room-B", "session-1", Warning), sig("room-A", "session-old", Warning)} {
		if r, err := g.Handle(context.Background(), s); r != nil || err != nil {
			t.Fatalf("foreign signal handled: %v %v", r, err)
		}
	}
	if g.RestartBlocked() || len(f.calls) != 0 {
		t.Fatal("foreign signals must not latch or call anything")
	}
}

func TestDuplicateConcurrentWarningsCloseOnceAndLatch(t *testing.T) {
	f := &fixture{}
	g := f.guard(t, nil)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := g.Handle(context.Background(), sig("room-A", "session-1", Warning))
			if err != nil || !r.MediaStopped || !r.RoomEnded {
				t.Errorf("bad result %v %v", r, err)
			}
		}()
	}
	wg.Wait()
	if f.count("stop") != 1 || f.count("end") != 1 || !g.RestartBlocked() {
		t.Fatalf("calls=%v", f.calls)
	}
}

func TestMediaFailureDoesNotPreventRemoteShutdown(t *testing.T) {
	g := (&fixture{}).guard(t, func(c *Config) {
		c.StopMedia = func(context.Context) (bool, error) { return false, errors.New("synthetic") }
	})
	r, _ := g.Handle(context.Background(), sig("room-A", "session-1", Warning))
	if r.MediaStopped || !r.RoomEnded {
		t.Fatalf("%+v", r)
	}
}

func TestRemoteRequestFailureStillStopsMediaAndVerifies(t *testing.T) {
	g := (&fixture{}).guard(t, func(c *Config) {
		c.RequestRoomEnd = func(context.Context) error { return errors.New("synthetic") }
	})
	r, _ := g.Handle(context.Background(), sig("room-A", "session-1", Warning))
	if !r.MediaStopped || !r.RoomEnded || !contains(r.Errors, "room_end_request:error") {
		t.Fatalf("%+v", r)
	}
}

func TestRequestSuccessIsNotRemoteConfirmation(t *testing.T) {
	g := (&fixture{}).guard(t, func(c *Config) {
		c.VerifyRoomEnded = func(context.Context) (bool, error) { return false, nil }
	})
	r, _ := g.Handle(context.Background(), sig("room-A", "session-1", Warning))
	if !r.MediaStopped || r.RoomEnded || !contains(r.Errors, "room_end_unconfirmed") || !g.RestartBlocked() {
		t.Fatalf("%+v", r)
	}
}

func TestMediaTimeoutDoesNotBlockRemoteClose(t *testing.T) {
	g := (&fixture{}).guard(t, func(c *Config) {
		c.StopMedia = func(ctx context.Context) (bool, error) { <-ctx.Done(); return false, ctx.Err() }
	})
	r, _ := g.Handle(context.Background(), sig("room-A", "session-1", Warning))
	if r.MediaStopped || !r.RoomEnded || !contains(r.Errors, "media_stop:timeout") {
		t.Fatalf("%+v", r)
	}
}

func TestCallerCancellationDoesNotCancelShutdown(t *testing.T) {
	f := &fixture{}
	entered, release := make(chan struct{}), make(chan struct{})
	g := f.guard(t, func(c *Config) {
		c.OperationTimeout = 2 * time.Second
		c.StopMedia = func(context.Context) (bool, error) { close(entered); <-release; f.add("stop"); return true, nil }
	})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := g.Handle(ctx, sig("room-A", "session-1", Warning)); errc <- err }()
	<-entered
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller must see cancellation, got %v", err)
	}
	close(release)
	r, err := g.Handle(context.Background(), sig("room-A", "session-1", Restriction))
	if err != nil || !r.MediaStopped || !r.RoomEnded || f.count("end") != 1 {
		t.Fatalf("%+v %v %v", r, err, f.calls)
	}
}

func TestRejectsUnclassifiedSignal(t *testing.T) {
	g := (&fixture{}).guard(t, nil)
	if _, err := g.Handle(context.Background(), Signal{RoomID: "room-A", SessionID: "session-1"}); err == nil {
		t.Fatal("a signal without kind/evidence must be rejected")
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
