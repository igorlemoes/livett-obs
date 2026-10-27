// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package impoll

import (
	"context"
	"encoding/hex"
	"errors"
	"net/url"
	"sync"
	"testing"
	"time"

	"tiktoklive/internal/guard"
	"tiktoklive/internal/wire"
)

var control, _ = hex.DecodeString("0a210a1557656263617374436f6e74726f6c4d65737361676512060a02187b10041863" + "1201632a01784801")

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

func (f *fixture) guard(t *testing.T, room string) *guard.Guard {
	g, err := guard.New(guard.Config{RoomID: room, SessionID: "local", OperationTimeout: time.Second,
		StopMedia:       func(context.Context) (bool, error) { f.add("stop"); return true, nil },
		RequestRoomEnd:  func(context.Context) error { f.add("end"); return errors.New("No remote adapter") },
		VerifyRoomEnded: func(context.Context) (bool, error) { return false, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func staticFetch(body []byte) Fetch {
	return func(context.Context, string) ([]byte, error) { return body, nil }
}

func TestWireToGuardStopsOnceAndReportsRemoteUnconfirmed(t *testing.T) {
	f := &fixture{}
	var urls []string
	p, err := New(f.guard(t, "123"), func(_ context.Context, u string) ([]byte, error) { urls = append(urls, u); return control, nil }, nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.PollOnce(context.Background())
	if err != nil || result.Dispatched != 1 || !result.ShutdownResults[0].MediaStopped || result.ShutdownResults[0].RoomEnded {
		t.Fatalf("%+v %v", result, err)
	}
	q, _ := url.ParseQuery(urls[0][len("https://webcast.tiktokv.com/webcast/im/fetch/?"):])
	if q.Get("identity") != "anchor" || q.Get("room_id") != "123" {
		t.Fatalf("%v", q)
	}
	if f.count("stop") != 1 || f.count("end") != 1 {
		t.Fatalf("%v", f.calls)
	}
	if _, err := p.PollOnce(context.Background()); !errors.Is(err, ErrShutdownLatched) {
		t.Fatalf("a latched shutdown must refuse more polls: %v", err)
	}
}

func TestForeignRoomIgnoredAndCursorEncodedForNextRequest(t *testing.T) {
	f := &fixture{}
	var urls []string
	p, _ := New(f.guard(t, "456"), func(_ context.Context, u string) ([]byte, error) { urls = append(urls, u); return control, nil }, nil, 0, nil)
	if r, err := p.PollOnce(context.Background()); err != nil || r.Dispatched != 0 {
		t.Fatalf("%+v %v", r, err)
	}
	p.PollOnce(context.Background())
	q, _ := url.ParseQuery(urls[1][len("https://webcast.tiktokv.com/webcast/im/fetch/?"):])
	if q.Get("cursor") != "c" || q.Get("internal_ext") != "x" || len(f.calls) != 0 {
		t.Fatalf("%v %v", q, f.calls)
	}
}

func TestMalformedPayloadDoesNotAdvanceCursorOrStop(t *testing.T) {
	f := &fixture{}
	p, _ := New(f.guard(t, "123"), staticFetch([]byte{0x0a, 0x10, 'b', 'a', 'd'}), nil, 0, nil)
	if _, err := p.PollOnce(context.Background()); !wire.IsError(err) {
		t.Fatalf("%v", err)
	}
	if len(f.calls) != 0 || p.Cursor() != "" {
		t.Fatalf("%v %q", f.calls, p.Cursor())
	}
}

func TestNetworkFailureIsNotAWarning(t *testing.T) {
	f := &fixture{}
	p, _ := New(f.guard(t, "123"), func(context.Context, string) ([]byte, error) { return nil, errors.New("Unavailable") }, nil, 0, nil)
	if _, err := p.PollOnce(context.Background()); err == nil || len(f.calls) != 0 {
		t.Fatalf("%v %v", err, f.calls)
	}
}

func TestConcurrentPollsCannotRestartAfterWarning(t *testing.T) {
	f := &fixture{}
	p, _ := New(f.guard(t, "123"), func(context.Context, string) ([]byte, error) { time.Sleep(time.Millisecond); return control, nil }, nil, 0, nil)
	var wg sync.WaitGroup
	var mu sync.Mutex
	latched := 0
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.PollOnce(context.Background()); errors.Is(err, ErrShutdownLatched) {
				mu.Lock()
				latched++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if latched != 1 || f.count("stop") != 1 {
		t.Fatalf("latched=%d calls=%v", latched, f.calls)
	}
}

func TestObserveNeverDispatchesAndAdvancesTheCursor(t *testing.T) {
	// Response: cursor(2)='c1', internal_ext(5)='e1', no messages.
	raw := append(append([]byte{0x12, 2}, "c1"...), append([]byte{0x2A, 2}, "e1"...)...)
	g := &neverDispatch{}
	p, _ := New(g, staticFetch(raw), nil, 0, nil)
	body, response, err := p.ObserveOnce(context.Background())
	if err != nil || string(body) != string(raw) || response.Cursor != "c1" {
		t.Fatalf("%v %v", response, err)
	}
	if u, _ := url.Parse(p.URL()); u.Query().Get("cursor") != "c1" {
		t.Fatalf("%s", p.URL())
	}
}

type neverDispatch struct{}

func (neverDispatch) RoomID() string       { return "456" }
func (neverDispatch) SessionID() string    { return "s" }
func (neverDispatch) RestartBlocked() bool { return false }
func (neverDispatch) Handle(context.Context, guard.Signal) (*guard.Result, error) {
	panic("observation must never dispatch")
}

func TestOnlyBrowserHintsMayBeSuppliedAndRoomMustBeNumeric(t *testing.T) {
	f := &fixture{}
	if _, err := New(f.guard(t, "123"), staticFetch(nil), [][2]string{{"room_id", "999"}}, 0, nil); err == nil {
		t.Fatal("routing parameters must not be overridable")
	}
	if _, err := New(f.guard(t, "123"), staticFetch(nil), [][2]string{{"screen_width", "1920"}}, 0, nil); err != nil {
		t.Fatal(err)
	}
	for _, room := range []string{"abc", "0", "12a", "-1"} {
		if _, err := New(&neverDispatchRoom{room}, staticFetch(nil), nil, 0, nil); err == nil {
			t.Errorf("room %q must be rejected", room)
		}
	}
	if _, err := New(f.guard(t, "123"), staticFetch(nil), nil, 20000, nil); err == nil {
		t.Fatal("dedup bound must be validated")
	}
}

type neverDispatchRoom struct{ room string }

func (n *neverDispatchRoom) RoomID() string       { return n.room }
func (n *neverDispatchRoom) SessionID() string    { return "s" }
func (n *neverDispatchRoom) RestartBlocked() bool { return false }
func (n *neverDispatchRoom) Handle(context.Context, guard.Signal) (*guard.Result, error) {
	return nil, nil
}

func TestObserverFailureNeverBlocksAShutdownSignal(t *testing.T) {
	f := &fixture{}
	p, _ := New(f.guard(t, "123"), staticFetch(control), nil, 0, func([]byte, *wire.Response) { panic("recorder bug") })
	if r, err := p.PollOnce(context.Background()); err != nil || r.Dispatched != 1 {
		t.Fatalf("%+v %v", r, err)
	}
}
