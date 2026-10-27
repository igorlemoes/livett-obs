// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package controller

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"tiktoklive/internal/obs"
)

func nativeEnv(t *testing.T) *env {
	e := newEnv(t, nil)
	e.c.StartPoll = 5 * time.Millisecond
	// the profile's service is the plugin's own: that is what makes the OBS button start a LIVE
	e.obs.mu.Lock()
	e.obs.service = Obj{"streamServiceType": obs.NativeServiceType, "streamServiceSettings": Obj{}}
	e.obs.mu.Unlock()
	return e
}

func (e *env) startNative(title string) (NativeTarget, error) {
	return e.c.StartNative(ctx(), StartRequest{Title: title, Scene: "Brio 4k", Confirmed: true})
}

func TestNativeStartHandsTheDestinationOverAndGoesLiveWhenObsStarts(t *testing.T) {
	e := nativeEnv(t)
	target, err := e.startNative("Título")
	if err != nil {
		t.Fatal(err)
	}
	if target.Server() != "rtmp://push-test.tiktokcdn.com/live" || target.Key() != "synthetic-key" {
		t.Fatalf("%s", target)
	}
	if strings.Contains(fmt.Sprintf("%v %+v %#v", target, target, target), "synthetic-key") {
		t.Fatal("the key must never be printed")
	}
	if e.obs.has("SetStreamServiceSettings") || e.obs.has("StartStream") {
		t.Fatal("in native mode OBS itself starts the output: the controller must not touch the destination or start it")
	}
	e.obs.setActive(true) // the plugin's service gave OBS the destination and OBS connected
	if !e.waitFor(Live, 3*time.Second) {
		t.Fatalf("%v: %s", e.c.Phase(), e.c.Detail())
	}
	if raw := asString(e.c.Snapshot()); strings.Contains(raw, "synthetic-key") {
		t.Fatal("the key must not appear in the panel state")
	}
	if _, err := e.c.End(ctx()); err != nil {
		t.Fatal(err)
	}
	if e.c.Phase() != Closed || !e.obs.has("StopStream") || e.obs.has("SetStreamServiceSettings") {
		t.Fatalf("phase=%v: ending must stop OBS and leave the destination alone", e.c.Phase())
	}
}

func TestNativeStartRefusedBeforeTheHandoffReturnsTheError(t *testing.T) {
	e := nativeEnv(t)
	if _, err := e.c.StartNative(ctx(), StartRequest{Title: "Título", Scene: "Brio 4k", Confirmed: false}); !isPanelError(err) {
		t.Fatalf("an unconfirmed start must be refused: %v", err)
	}
	if _, err := e.c.StartNative(ctx(), StartRequest{Title: "Título", Scene: "outra cena", Confirmed: true}); !isPanelError(err) {
		t.Fatalf("a changed scene must be refused: %v", err)
	}
	if e.browser.has("create") {
		t.Fatal("nothing may be created when the start is refused")
	}
}

func TestNativeStartRollsBackWhenObsNeverStarts(t *testing.T) {
	e := nativeEnv(t)
	if _, err := e.startNative("Título"); err != nil {
		t.Fatal(err)
	}
	// OBS never becomes active (for example the connection to TikTok failed): the room must be closed again
	if !e.waitFor(Closed, 5*time.Second) {
		t.Fatalf("%v: %s", e.c.Phase(), e.c.Detail())
	}
	last, _ := e.browser.arg("ping").(Obj)
	if e.browser.count("ping") != 2 || fmt.Sprint(last["status"]) != "4" {
		t.Fatalf("the room must be prepared (ping 1) and then ended (ping 4); pings=%d last=%v", e.browser.count("ping"), last["status"])
	}
	if e.obs.has("SetStreamServiceSettings") {
		t.Fatal("the destination must not have been touched")
	}
}

func TestNativeStartRefusesWhileSomethingIsBeingDecided(t *testing.T) {
	e := nativeEnv(t)
	e.c.setState(Creating, "")
	if _, err := e.startNative("Título"); err == nil {
		t.Fatal("no start while a creation is under way")
	}
}
