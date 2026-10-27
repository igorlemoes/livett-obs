// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package watchdog

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var tiktok = Obj{"streamServiceType": "rtmp_custom",
	"streamServiceSettings": Obj{"server": "rtmp://push-f5.tiktokcdn.com/live", "key": "k"}}

type fakeObs struct {
	active  bool
	profile string
	service Obj
	calls   []string
}

func newObs() *fakeObs { return &fakeObs{active: true, profile: "TikTok", service: tiktok} }

func (f *fakeObs) Close() {}
func (f *fakeObs) has(n string) bool {
	for _, c := range f.calls {
		if c == n {
			return true
		}
	}
	return false
}
func (f *fakeObs) Call(_ context.Context, name string, data map[string]any) (map[string]any, error) {
	f.calls = append(f.calls, name)
	switch name {
	case "GetStreamStatus":
		return Obj{"outputActive": f.active, "outputReconnecting": false}, nil
	case "GetProfileList":
		return Obj{"currentProfileName": f.profile}, nil
	case "GetStreamServiceSettings":
		return f.service, nil
	case "StopStream":
		f.active = false
	case "SetStreamServiceSettings":
		f.service = data
	}
	return Obj{}, nil
}

type fakeBrowser struct {
	ended bool
	calls []string
}

func (f *fakeBrowser) Call(_ context.Context, method string, data any) (any, error) {
	status := ""
	if m, ok := data.(Obj); ok {
		if s, ok := m["status"].(int); ok {
			status = string(rune('0' + s))
		}
	}
	f.calls = append(f.calls, method+":"+status)
	if method == "verify" {
		return Obj{"ended": f.ended}, nil
	}
	return Obj{"code": float64(0)}, nil
}

type rig struct {
	t     *testing.T
	root  string
	now   time.Time
	pidUp bool
}

func newRig(t *testing.T) *rig {
	r := &rig{t: t, root: t.TempDir(), now: time.Unix(1000, 0), pidUp: true}
	os.MkdirAll(filepath.Join(r.root, "runtime"), 0o755)
	return r
}

func (r *rig) write(name string, data Obj) {
	raw, _ := json.Marshal(data)
	os.WriteFile(filepath.Join(r.root, "runtime", name), raw, 0o644)
}

func (r *rig) journal(phase string) {
	r.write("broadcast.json", Obj{"phase": phase, "profile": "TikTok",
		"binding": Obj{"room_id": "1", "ping_room_id": "1", "stream_id": "2"}})
}

func (r *rig) beat(ageSeconds float64, pid int) {
	r.write("heartbeat.json", Obj{"t": float64(r.now.Unix()) - ageSeconds, "pid": pid})
}

func (r *rig) dog(obs *fakeObs, browser *fakeBrowser) *Watchdog {
	w := New(r.root, func(context.Context) (Obs, error) { return obs, nil },
		func(context.Context) (Browser, error) {
			if browser == nil {
				return nil, nil
			}
			return browser, nil
		})
	w.Clock = func() time.Time { return r.now }
	w.StaleAfter = 15 * time.Second
	w.Alive = func(int) bool { return r.pidUp }
	return w
}

func TestActsOnlyWhenLiveAndHeartbeatIsStale(t *testing.T) {
	r := newRig(t)
	r.journal("live")
	dog := r.dog(newObs(), &fakeBrowser{})
	r.beat(5, 1)
	if dog.ShouldAct() {
		t.Fatal("a fresh heartbeat must not trigger")
	}
	r.beat(20, 1) // hung: alive but silent
	if !dog.ShouldAct() {
		t.Fatal("a stale heartbeat must trigger")
	}
	r.journal("closed")
	if dog.ShouldAct() || !dog.ShouldExit() {
		t.Fatal("a closed broadcast needs no watching")
	}
}

func TestDeadPanelProcessTriggersImmediately(t *testing.T) {
	r := newRig(t)
	r.journal("live")
	dog := r.dog(newObs(), &fakeBrowser{})
	r.beat(0.5, 4242)
	if dog.ShouldAct() {
		t.Fatal("alive and fresh")
	}
	r.pidUp = false
	if !dog.ShouldAct() {
		t.Fatal("a dead process must not wait for the heartbeat to age")
	}
}

func TestDeadPanelStopsOurOutputEndsRoomAndWipesKey(t *testing.T) {
	r := newRig(t)
	r.journal("live")
	obs, browser := newObs(), &fakeBrowser{ended: true}
	result := r.dog(obs, browser).Act(context.Background())
	if result["obs"] != "stopped" || result["room"] != "ended" {
		t.Fatalf("%v", result)
	}
	if !contains(browser.calls, "ping:4") {
		t.Fatalf("%v", browser.calls)
	}
	if obs.service["streamServiceSettings"].(Obj)["key"] != "" {
		t.Fatal("the key must be wiped")
	}
	if read(filepath.Join(r.root, "runtime", "broadcast.json"))["phase"] != "closed" {
		t.Fatal("journal must close")
	}
}

func TestNeverTouchesAForeignProfileOrDestination(t *testing.T) {
	r := newRig(t)
	foreignProfile := newObs()
	foreignProfile.profile = "Outro"
	foreignService := newObs()
	foreignService.service = Obj{"streamServiceType": "rtmp_common", "streamServiceSettings": Obj{"service": "YouTube"}}
	for _, obs := range []*fakeObs{foreignProfile, foreignService} {
		r.journal("live")
		result := r.dog(obs, &fakeBrowser{ended: true}).Act(context.Background())
		if result["obs"] != "not_ours" || obs.has("StopStream") || obs.has("SetStreamServiceSettings") {
			t.Fatalf("%v %v", result, obs.calls)
		}
	}
}

func TestUnconfirmedRoomOrMissingBrowserLeavesUncertain(t *testing.T) {
	r := newRig(t)
	r.journal("live")
	result := r.dog(newObs(), &fakeBrowser{ended: false}).Act(context.Background())
	if result["room"] != "not_confirmed" || read(filepath.Join(r.root, "runtime", "broadcast.json"))["phase"] != "uncertain" {
		t.Fatalf("%v", result)
	}
	r.journal("live")
	if result := r.dog(newObs(), nil).Act(context.Background()); result["room"] != "browser_unavailable" {
		t.Fatalf("%v", result)
	}
}

func TestOverrunOfTheScheduledEndMakesTheWatchdogStepInEvenIfThePanelIsAlive(t *testing.T) {
	r := newRig(t)
	data := Obj{"phase": "live", "profile": "TikTok", "end_at": float64(r.now.Unix()) - 5,
		"binding": Obj{"room_id": "1", "ping_room_id": "1", "stream_id": "2"}}
	r.write("broadcast.json", data)
	r.beat(1, 1)
	dog := r.dog(newObs(), &fakeBrowser{})
	if dog.ShouldAct() {
		t.Fatal("inside the grace period")
	}
	data["end_at"] = float64(r.now.Unix()) - 30
	r.write("broadcast.json", data)
	if !dog.ShouldAct() {
		t.Fatal("past the grace period the watchdog must step in")
	}
}

func TestDestinationCheck(t *testing.T) {
	if !IsTikTokDestination(tiktok) {
		t.Fatal("a TikTok ingest is ours")
	}
	other := Obj{"streamServiceType": "rtmp_custom", "streamServiceSettings": Obj{"server": "rtmp://a.example/live"}}
	if IsTikTokDestination(other) {
		t.Fatal("a foreign server is not")
	}
}

func TestPidAliveSeesOurOwnProcessAndNotABogusOne(t *testing.T) {
	if !PidAlive(os.Getpid()) {
		t.Fatal("this process is alive")
	}
	if PidAlive(0) || PidAlive(-5) {
		t.Fatal("non-positive pids are never alive")
	}
}

func TestRunExitsWhenThereIsNothingToWatchAndActsOnADeadPanel(t *testing.T) {
	r := newRig(t)
	dog := r.dog(newObs(), &fakeBrowser{ended: true})
	dog.PollEvery = 10 * time.Millisecond
	if err := dog.Run(context.Background()); err != nil { // no journal: nothing to watch
		t.Fatal(err)
	}
	r.journal("live")
	r.beat(0.5, 4242)
	r.pidUp = false
	if err := dog.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if read(filepath.Join(r.root, "runtime", "watchdog.json")) == nil {
		t.Fatal("the outcome must be recorded for the panel")
	}
}

func TestADeadPanelStopsAStreamStartedByTheObsButtonAndLeavesTheServiceAlone(t *testing.T) {
	r := newRig(t)
	r.journal("live")
	obs, browser := newObs(), &fakeBrowser{ended: true}
	obs.service = Obj{"streamServiceType": "tiktok_live", "streamServiceSettings": Obj{}}
	result := r.dog(obs, browser).Act(context.Background())
	if result["obs"] != "stopped" || result["room"] != "ended" {
		t.Fatalf("%v", result)
	}
	if obs.has("SetStreamServiceSettings") {
		t.Fatal("the plugin's own service holds no key: it must not be replaced by a blank destination")
	}
	if !IsNativeDestination(obs.service) || IsNativeDestination(tiktok) {
		t.Fatal("only the plugin's service type is native")
	}
}
