// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package controller

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

type signInBrowser struct {
	*fakeBrowser
	mu       sync.Mutex
	released int
	valid    bool
	synced   bool
}

func (b *signInBrowser) ReleaseLogin(context.Context) { b.mu.Lock(); b.released++; b.mu.Unlock() }
func (b *signInBrowser) SyncWebcastSession(context.Context) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.valid && !b.synced {
		b.synced = true // the fresh web login is copied to the LIVE service's domain
		return true, nil
	}
	return false, nil
}
func (b *signInBrowser) setValid() { b.mu.Lock(); b.valid = true; b.mu.Unlock() }
func (b *signInBrowser) accepted() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.valid && b.synced
}

func TestTheLoginWatcherSyncsFlipsToReadyAndReleasesTheWindowOnceSignedIn(t *testing.T) {
	fb := newFakeBrowser()
	sb := &signInBrowser{fakeBrowser: fb}
	fb.override = func(method string, _ any) (any, error, bool) {
		if method == "check" && !sb.accepted() {
			return Obj{"code": json.Number("20003")}, nil, true
		}
		return nil, nil, false
	}
	obs := newFakeObs()
	c := New(Options{Root: t.TempDir(), Browser: sb, ObsFactory: func(context.Context) (Obs, error) { return obs, nil }})
	if state, _ := c.Connect(ctx()); state["phase"] != "login" {
		t.Fatalf("%v", state["phase"])
	}
	time.Sleep(3500 * time.Millisecond)
	if c.Phase() != Login {
		t.Fatal("still signed out: the panel must keep waiting")
	}
	sb.setValid() // the user finished signing in, in the Chrome window
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && c.Phase() != Ready {
		time.Sleep(100 * time.Millisecond)
	}
	if c.Phase() != Ready {
		t.Fatalf("phase %v: %s", c.Phase(), c.Detail())
	}
	sb.mu.Lock()
	released := sb.released
	sb.mu.Unlock()
	if released == 0 {
		t.Fatal("the sign-in window must be released (hidden for good)")
	}
	if c.Snapshot()["canStart"] != true {
		t.Fatal("the account is ready to start")
	}
}
