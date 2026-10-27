// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package controller

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tiktoklive/internal/obs"
	"tiktoklive/internal/wire"
)

// ---- fakes

type fakeObs struct {
	mu       sync.Mutex
	active   bool
	scene    string
	scenes   []string // optional: more scenes than the current one
	service  Obj
	calls    []string
	override func(name string, data Obj) (Obj, error, bool)
}

func newFakeObs() *fakeObs {
	return &fakeObs{scene: "Brio 4k", service: Obj{"streamServiceType": "rtmp_custom",
		"streamServiceSettings": Obj{"server": "rtmp://original.invalid/live", "key": "original"}}}
}

func (f *fakeObs) Close() {}

func (f *fakeObs) has(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == name {
			return true
		}
	}
	return false
}

func (f *fakeObs) setActive(v bool) { f.mu.Lock(); f.active = v; f.mu.Unlock() }

func clone(o Obj) Obj {
	raw, _ := json.Marshal(o)
	var out Obj
	json.Unmarshal(raw, &out)
	return out
}

func (f *fakeObs) Call(_ context.Context, name string, data map[string]any) (map[string]any, error) {
	f.mu.Lock()
	f.calls = append(f.calls, name)
	override := f.override
	f.mu.Unlock()
	if override != nil {
		if out, err, handled := override(name, data); handled {
			return out, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch name {
	case "GetProfileList":
		return Obj{"currentProfileName": "Meu perfil"}, nil
	case "GetCurrentProgramScene":
		return Obj{"currentProgramSceneName": f.scene}, nil
	case "GetSceneList":
		list := []any{Obj{"sceneName": f.scene}}
		if len(f.scenes) > 0 {
			list = nil
			for _, s := range f.scenes {
				list = append(list, Obj{"sceneName": s})
			}
		}
		return Obj{"scenes": list}, nil
	case "GetStreamStatus":
		return Obj{"outputActive": f.active, "outputReconnecting": false}, nil
	case "GetStreamServiceSettings":
		return clone(f.service), nil
	case "SetStreamServiceSettings":
		f.service = data
	case "SetCurrentProgramScene":
		f.scene, _ = data["sceneName"].(string)
	case "StartStream":
		f.active = true
	case "StopStream":
		f.active = false
	}
	return Obj{}, nil
}

type fakeBrowser struct {
	mu         sync.Mutex
	calls      []string
	args       map[string]any
	ended      bool
	failCreate bool
	override   func(method string, data any) (any, error, bool)
	loginShown int
}

func newFakeBrowser() *fakeBrowser { return &fakeBrowser{ended: true, args: map[string]any{}} }

func (f *fakeBrowser) Connect(context.Context) error { return nil }
func (f *fakeBrowser) ShowLogin(context.Context) error {
	f.mu.Lock()
	f.loginShown++
	f.mu.Unlock()
	return nil
}
func (f *fakeBrowser) ReleaseLogin(context.Context) {}

func (f *fakeBrowser) count(method string) (n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == method {
			n++
		}
	}
	return
}

func (f *fakeBrowser) has(method string) bool { return f.count(method) > 0 }

func (f *fakeBrowser) arg(method string) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.args[method]
}

func (f *fakeBrowser) setOverride(fn func(method string, data any) (any, error, bool)) {
	f.mu.Lock()
	f.override = fn
	f.mu.Unlock()
}

func (f *fakeBrowser) Call(_ context.Context, method string, data any) (any, error) {
	f.mu.Lock()
	f.calls = append(f.calls, method)
	f.args[method] = data
	override, ended, failCreate := f.override, f.ended, f.failCreate
	f.mu.Unlock()
	if override != nil {
		if out, err, handled := override(method, data); handled {
			return out, err
		}
	}
	switch method {
	case "check":
		return Obj{"code": json.Number("0"), "liveStatus": json.Number("4"), "enabled": true, "banned": false}, nil
	case "verify":
		return Obj{"ended": ended}, nil
	case "ping":
		return Obj{"code": json.Number("0")}, nil
	case "drop":
		return Obj{"cleared": true}, nil
	case "create":
		if failCreate {
			return nil, errors.New("uncertain transport")
		}
		raw := `{"status_code":0,"data":{"id_str":"123","stream_id_str":"789","living_room_attrs":{"room_id_str":"456"},
		 "stream_url":{"rtmp_push_url":"rtmp://push-test.tiktokcdn.com/live/synthetic-key"}}}`
		return mustJSON(raw), nil
	}
	return nil, nil
}

func mustJSON(raw string) Obj {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var o Obj
	if err := dec.Decode(&o); err != nil {
		panic(err)
	}
	return o
}

type env struct {
	t       *testing.T
	root    string
	obs     *fakeObs
	browser *fakeBrowser
	c       *Controller
}

func newEnv(t *testing.T, mutate func(*Options)) *env {
	e := &env{t: t, root: t.TempDir(), obs: newFakeObs(), browser: newFakeBrowser()}
	opts := Options{Root: e.root, Browser: e.browser, ObsFactory: func(context.Context) (Obs, error) { return e.obs, nil }}
	if mutate != nil {
		mutate(&opts)
	}
	e.c = New(opts)
	if _, err := e.c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.c.stopIM() })
	return e
}

func ctx() context.Context { return context.Background() }

func (e *env) start(title string) (Obj, error) {
	return e.c.Start(ctx(), StartRequest{Title: title, Scene: "Brio 4k", Confirmed: true})
}

func (e *env) mustStart() Obj {
	e.t.Helper()
	snap, err := e.start("Título")
	if err != nil {
		e.t.Fatal(err)
	}
	return snap
}

func isPanelError(err error) bool {
	var pe *PanelError
	return errors.As(err, &pe)
}

func expectPanelError(t *testing.T, err error, label string) {
	t.Helper()
	if !isPanelError(err) {
		t.Fatalf("%s: want a PanelError, got %v", label, err)
	}
}

func (e *env) waitFor(phase Phase, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if e.c.Phase() == phase {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func asString(o Obj) string { raw, _ := json.Marshal(o); return string(raw) }

// ---- tests

func TestConnectIsReadOnly(t *testing.T) {
	e := newEnv(t, nil)
	if e.browser.has("create") || e.obs.has("SetStreamServiceSettings") || e.obs.has("StartStream") {
		t.Fatal("connect must not publish or change anything")
	}
}

func TestExpiredTikTokSessionIsNotReadyAndCannotStart(t *testing.T) {
	e := newEnv(t, nil)
	e.browser.setOverride(func(m string, _ any) (any, error, bool) {
		if m == "check" {
			return Obj{"code": json.Number("20003"), "liveStatus": nil, "enabled": false, "banned": nil}, nil, true
		}
		return nil, nil, false
	})
	state, err := e.c.Connect(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if state["phase"] != "login" || state["canStart"] != false || !strings.Contains(state["detail"].(string), "Entre na sua conta") {
		t.Fatalf("%v", state)
	}
	if _, err := e.start("Título"); !isPanelError(err) || e.browser.has("create") {
		t.Fatalf("%v", err)
	}
	e.browser.setOverride(nil) // signing in again makes it ready
	if state, _ := e.c.Connect(ctx()); state["phase"] != "ready" {
		t.Fatalf("%v", state["phase"])
	}
}

func TestActiveOutputIsNotReconfigured(t *testing.T) {
	e := newEnv(t, nil)
	e.obs.setActive(true)
	_, err := e.start("Título")
	expectPanelError(t, err, "active output")
	if e.browser.has("create") || e.obs.has("SetStreamServiceSettings") {
		t.Fatal("nothing may be created or reconfigured")
	}
}

func TestSceneChangeAndMissingConfirmationBlockPublication(t *testing.T) {
	e := newEnv(t, nil)
	for _, c := range []struct {
		scene     string
		confirmed bool
	}{{"Outra cena", true}, {"Brio 4k", false}} {
		_, err := e.c.Start(ctx(), StartRequest{Title: "Título", Scene: c.scene, Confirmed: c.confirmed})
		expectPanelError(t, err, c.scene)
	}
	if e.browser.has("create") {
		t.Fatal("must not create")
	}
}

func TestFullManualFlowRestoresServiceAndConfirmsEnd(t *testing.T) {
	e := newEnv(t, nil)
	original := clone(e.obs.service)
	e.mustStart()
	if !e.obs.active || e.c.Phase() != Live {
		t.Fatal("not live")
	}
	if _, err := e.c.Connect(ctx()); !isPanelError(err) {
		t.Fatalf("connect during a LIVE must be refused: %v", err)
	}
	result, err := e.c.End(ctx())
	if err != nil {
		t.Fatal(err)
	}
	if e.obs.active || result["phase"] != "closed" {
		t.Fatalf("%v", result["phase"])
	}
	if asString(e.obs.service) != asString(original) {
		t.Fatalf("service not restored: %v", e.obs.service)
	}
	if strings.Contains(asString(e.c.Snapshot()), "synthetic-key") {
		t.Fatal("the stream key leaked into the snapshot")
	}
	journal, _ := os.ReadFile(filepath.Join(e.root, "runtime", "broadcast.json"))
	if strings.Contains(string(journal), "synthetic-key") {
		t.Fatal("the stream key leaked into the journal")
	}
}

func TestUnrestorableEmptyServiceClearsTemporaryKey(t *testing.T) {
	e := newEnv(t, nil)
	e.obs.service = Obj{"streamServiceType": "rtmp_common", "streamServiceSettings": Obj{}}
	e.obs.override = func(name string, data Obj) (Obj, error, bool) {
		if name == "SetStreamServiceSettings" && data["streamServiceType"] == "rtmp_common" {
			return nil, &obs.Error{Msg: "rejected", Code: 600}, true
		}
		return nil, nil, false
	}
	e.mustStart()
	result, err := e.c.End(ctx())
	if err != nil || result["phase"] != "closed" {
		t.Fatalf("%v %v", result["phase"], err)
	}
	settings := asObj(e.obs.service["streamServiceSettings"])
	if settings["key"] != "" || strings.Contains(asString(e.obs.service), "synthetic-key") {
		t.Fatalf("temporary key survived: %v", e.obs.service)
	}
}

func TestCleanServerRejectionReturnsToReadyAndAllowsRetry(t *testing.T) {
	e := newEnv(t, nil)
	rejected := false
	e.browser.setOverride(func(m string, _ any) (any, error, bool) {
		if m == "create" && !rejected {
			rejected = true
			return mustJSON(`{"status_code":4003,"data":{}}`), nil, true
		}
		return nil, nil, false
	})
	result, err := e.start("Título")
	if err != nil || result["phase"] != "ready" || e.obs.has("SetStreamServiceSettings") {
		t.Fatalf("%v %v", result["phase"], err)
	}
	if result, err = e.start("Título"); err != nil || result["phase"] != "live" {
		t.Fatalf("%v %v", result["phase"], err)
	}
}

func TestCreateReceivesTheExpectedFields(t *testing.T) {
	e := newEnv(t, nil)
	e.start("Meu título")
	raw, _ := json.Marshal(e.browser.arg("create"))
	if !strings.Contains(string(raw), `"title":"Meu título"`) || !strings.Contains(string(raw), `"gen_replay":"true"`) {
		t.Fatalf("%s", raw)
	}
}

func TestStaleTikTokKeyIsNeverRestoredAndAlwaysWiped(t *testing.T) {
	e := newEnv(t, nil)
	e.obs.service = Obj{"streamServiceType": "rtmp_custom", "streamServiceSettings": Obj{
		"server": "rtmp://push-rtmp-f5-sg01.tiktokcdn.com/live", "key": "expired-key"}}
	e.mustStart()
	result, _ := e.c.End(ctx())
	settings := asObj(e.obs.service["streamServiceSettings"])
	text := asString(e.obs.service)
	if result["phase"] != "closed" || settings["key"] != "" || strings.Contains(text, "synthetic-key") || strings.Contains(text, "expired-key") ||
		e.c.keyCleanup {
		t.Fatalf("%v %s", result["phase"], text)
	}
}

func TestCrashLeftoverKeyIsSweptOnNextConnectWhenIdle(t *testing.T) {
	e := newEnv(t, nil)
	e.mustStart()
	e.obs.setActive(false) // the controller died, OBS stopped, the key stays behind
	fresh := New(Options{Root: e.root, Browser: e.browser, ObsFactory: func(context.Context) (Obs, error) { return e.obs, nil }})
	if !fresh.keyCleanup {
		t.Fatal("the journal must remember the pending cleanup")
	}
	if _, err := fresh.Connect(ctx()); err != nil {
		t.Fatal(err)
	}
	if asObj(e.obs.service["streamServiceSettings"])["key"] != "" {
		t.Fatalf("%v", e.obs.service)
	}
}

func TestRemoteNotEndedRemainsUncertainAndBlocksNewCreate(t *testing.T) {
	e := newEnv(t, nil)
	e.mustStart()
	e.browser.mu.Lock()
	e.browser.ended = false
	e.browser.mu.Unlock()
	result, _ := e.c.End(ctx())
	if result["phase"] != "uncertain" || e.obs.active {
		t.Fatalf("%v", result["phase"])
	}
	_, err := e.start("Título")
	expectPanelError(t, err, "start after uncertain")
}

func TestCrashDuringCreationPersistsRecoveryLatch(t *testing.T) {
	e := newEnv(t, nil)
	e.browser.failCreate = true
	if _, err := e.start("Título"); err == nil {
		t.Fatal("expected the transport error")
	}
	fresh := New(Options{Root: e.root, Browser: e.browser})
	if fresh.Phase() != Recovery || e.browser.count("create") != 1 || fresh.Snapshot()["canStart"] != false {
		t.Fatalf("%v %d", fresh.Phase(), e.browser.count("create"))
	}
}

func TestKeyChangeCannotStopAnotherOutput(t *testing.T) {
	e := newEnv(t, nil)
	out := obs.NewStreamOutput(e.obs, "Meu perfil", "rtmp://original.invalid/live", "different")
	if _, err := out.Stop(ctx()); err == nil || e.obs.has("StopStream") {
		t.Fatalf("%v", err)
	}
}

// ---- IM guard

const controlHex = "0a210a1557656263617374436f6e74726f6c4d65737361676512060a02187b10041863" + "1201632a01784801"

func control() []byte { b, _ := hex.DecodeString(controlHex); return b }

func noticeResponse() []byte {
	payload, _ := hex.DecodeString("0a02187b12047761726e")
	message := append(wire.EncodeBlob(1, []byte("WebcastNoticeMessage")), wire.EncodeBlob(2, payload)...)
	message = append(message, wire.EncodeVarintField(3, 7)...)
	return wire.EncodeBlob(1, message)
}

func guardEnv(t *testing.T, payloads [][]byte, mutate func(*Options)) *env {
	queue := append([][]byte{}, payloads...)
	var mu sync.Mutex
	b := newFakeBrowser()
	b.override = func(method string, _ any) (any, error, bool) {
		switch method {
		case "imfetch":
			mu.Lock()
			var body []byte
			if len(queue) > 0 {
				body, queue = queue[0], queue[1:]
			}
			mu.Unlock()
			return Obj{"status": json.Number("200"), "type": "application/x-protobuf", "size": len(body),
				"b64": base64.StdEncoding.EncodeToString(body)}, nil, true
		case "aboutme":
			return Obj{"code": json.Number("0"), "enabled": true, "texts": []any{"Meu texto"}, "max": json.Number("240")}, nil, true
		}
		return nil, nil, false
	}
	e := &env{t: t, root: t.TempDir(), obs: newFakeObs(), browser: b}
	opts := Options{Root: e.root, Browser: b, IMGuard: true, ObsFactory: func(context.Context) (Obs, error) { return e.obs, nil }}
	if mutate != nil {
		mutate(&opts)
	}
	e.c = New(opts)
	if _, err := e.c.Connect(ctx()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.c.stopIM() })
	return e
}

func TestRemoteEndSignalEndsLiveAutomatically(t *testing.T) {
	e := guardEnv(t, [][]byte{control()}, nil)
	if e.c.Snapshot()["aboutMe"].(Obj)["text"] != "Meu texto" {
		t.Fatalf("%v", e.c.Snapshot()["aboutMe"])
	}
	e.mustStart()
	if !e.waitFor(Closed, 3*time.Second) {
		t.Fatalf("phase %v: %s", e.c.Phase(), e.c.Detail())
	}
	if e.obs.active || !strings.Contains(e.c.Detail(), "automaticamente") {
		t.Fatalf("%s", e.c.Detail())
	}
	if asObj(e.obs.service["streamServiceSettings"])["key"] != "original" {
		t.Fatalf("the real destination must be restored: %v", e.obs.service)
	}
}

func TestWarningEndsLiveByDefaultAndCanBeDisabled(t *testing.T) {
	e := guardEnv(t, [][]byte{noticeResponse()}, nil)
	e.mustStart()
	if !e.waitFor(Closed, 3*time.Second) {
		t.Fatalf("a warning must end the LIVE by default: %v", e.c.Phase())
	}
	e2 := guardEnv(t, [][]byte{noticeResponse()}, func(o *Options) { o.NoAutoEndOnWarning = true })
	e2.mustStart()
	time.Sleep(300 * time.Millisecond)
	if e2.c.Phase() != Live || !e2.obs.active {
		t.Fatalf("%v", e2.c.Phase())
	}
	e2.c.End(ctx())
}

func TestQuietChannelKeepsLiveAndManualEndStopsWatcher(t *testing.T) {
	e := guardEnv(t, nil, nil)
	e.mustStart()
	time.Sleep(300 * time.Millisecond)
	if e.c.Phase() != Live || e.c.Snapshot()["warningMonitor"] != true {
		t.Fatalf("%v", e.c.Phase())
	}
	e.c.End(ctx())
	time.Sleep(100 * time.Millisecond)
	if e.c.Snapshot()["warningMonitor"] != false {
		t.Fatal("the watcher must stop with the LIVE")
	}
}

// ---- Sobre mim

func aboutMeEnv(t *testing.T) (*env, *[]string, *bool) {
	saved := []string{}
	enabled := true
	e := &env{t: t, root: t.TempDir(), obs: newFakeObs(), browser: newFakeBrowser()}
	e.browser.override = func(method string, data any) (any, error, bool) {
		switch method {
		case "aboutme":
			text := "antigo"
			if len(saved) > 0 {
				text = saved[len(saved)-1]
			}
			return Obj{"code": json.Number("0"), "enabled": enabled, "texts": []any{text}, "max": json.Number("240")}, nil, true
		case "aboutmesave":
			if !enabled {
				return Obj{"saved": false, "reason": "unavailable"}, nil, true
			}
			saved = append(saved, asObj(data)["text"].(string))
			return Obj{"saved": true, "code": json.Number("0"), "audit": json.Number("1")}, nil, true
		}
		return nil, nil, false
	}
	e.c = New(Options{Root: e.root, Browser: e.browser, ObsFactory: func(context.Context) (Obs, error) { return e.obs, nil }})
	if _, err := e.c.Connect(ctx()); err != nil {
		t.Fatal(err)
	}
	return e, &saved, &enabled
}

func TestAboutMeConnectReadsWithoutWriting(t *testing.T) {
	e, saved, _ := aboutMeEnv(t)
	if e.c.Snapshot()["aboutMe"].(Obj)["text"] != "antigo" || len(*saved) != 0 {
		t.Fatal("connect must only read")
	}
}

func TestAboutMeExplicitSaveUpdatesAndRereads(t *testing.T) {
	e, saved, _ := aboutMeEnv(t)
	result, err := e.c.SaveAboutMe(ctx(), "Novo texto")
	if err != nil || len(*saved) != 1 || (*saved)[0] != "Novo texto" || result["aboutMe"].(Obj)["text"] != "Novo texto" ||
		!strings.Contains(result["detail"].(string), "moderação") {
		t.Fatalf("%v %v", result, err)
	}
}

func TestAboutMeInvalidOrDisabledIsRejectedWithoutWriting(t *testing.T) {
	e, saved, enabled := aboutMeEnv(t)
	for _, bad := range []string{"", "   ", strings.Repeat("x", 241)} {
		_, err := e.c.SaveAboutMe(ctx(), bad)
		expectPanelError(t, err, bad)
	}
	*enabled = false
	_, err := e.c.SaveAboutMe(ctx(), "ok")
	expectPanelError(t, err, "disabled")
	if len(*saved) != 0 {
		t.Fatal("nothing may be written")
	}
}
