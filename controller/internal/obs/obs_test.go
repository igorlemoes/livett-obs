// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package obs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// --- stream adapter, against a fake Caller

type fakeObs struct {
	profile, server string
	statuses        []map[string]any
	calls           []string
}

func (f *fakeObs) Call(_ context.Context, request string, _ map[string]any) (map[string]any, error) {
	f.calls = append(f.calls, request)
	switch request {
	case "GetProfileList":
		return map[string]any{"currentProfileName": f.profile}, nil
	case "GetStreamServiceSettings":
		return map[string]any{"streamServiceType": "rtmp_custom",
			"streamServiceSettings": map[string]any{"server": f.server}}, nil
	case "GetStreamStatus":
		if len(f.statuses) == 0 {
			return map[string]any{"outputActive": false, "outputReconnecting": false}, nil
		}
		s := f.statuses[0]
		f.statuses = f.statuses[1:]
		return s, nil
	}
	return map[string]any{}, nil
}

func (f *fakeObs) has(r string) bool {
	for _, c := range f.calls {
		if c == r {
			return true
		}
	}
	return false
}

func newFake() *fakeObs { return &fakeObs{profile: "Test", server: "rtmp://127.0.0.1/live"} }

func output(f *fakeObs) *StreamOutput {
	o := NewStreamOutput(f, "Test", "rtmp://127.0.0.1/live", "")
	o.PollInterval = time.Millisecond
	return o
}

func TestProfileChangeCannotStopUnrelatedOutput(t *testing.T) {
	f := newFake()
	f.profile = "Other"
	o := output(f)
	if _, err := o.Stop(context.Background()); err == nil || f.has("StopStream") || !o.RestartBlocked {
		t.Fatalf("err=%v calls=%v", err, f.calls)
	}
}

func TestDestinationChangeCannotStopUnrelatedOutput(t *testing.T) {
	f := newFake()
	f.server = "rtmp://another-destination/live"
	if _, err := output(f).Stop(context.Background()); err == nil || f.has("StopStream") {
		t.Fatalf("err=%v calls=%v", err, f.calls)
	}
}

func TestReconnectingOutputIsStoppedAndVerified(t *testing.T) {
	f := newFake()
	f.statuses = []map[string]any{{"outputActive": false, "outputReconnecting": true}, {"outputActive": false, "outputReconnecting": false}}
	o := output(f)
	ok, err := o.Stop(context.Background())
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	n := 0
	for _, c := range f.calls {
		if c == "StopStream" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("StopStream called %d times", n)
	}
	if err := o.Start(context.Background()); err == nil || f.has("StartStream") {
		t.Fatal("restart must stay blocked")
	}
}

func TestAlreadyStoppedIsConfirmedWithoutToggle(t *testing.T) {
	f := newFake()
	if ok, err := output(f).Stop(context.Background()); err != nil || !ok || f.has("StopStream") || f.has("ToggleStream") {
		t.Fatalf("%v %v %v", ok, err, f.calls)
	}
}

func TestStopThatNeverGoesIdleIsNotConfirmed(t *testing.T) {
	f := newFake()
	busy := map[string]any{"outputActive": true, "outputReconnecting": false}
	for i := 0; i < 40; i++ {
		f.statuses = append(f.statuses, busy)
	}
	o := output(f)
	if ok, err := o.Stop(context.Background()); err != nil || ok {
		t.Fatalf("%v %v", ok, err)
	}
}

// --- client, against a fake obs-websocket v5 server

// ---- the HTTP client against a fake of the plugin's API

type fakeAPI struct {
	mu       sync.Mutex // guards requests: the server goroutine appends while the test reads
	server   *httptest.Server
	token    string
	requests []string
	handler  func(request string, data map[string]any) (int, map[string]any)
}

func (f *fakeAPI) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func newAPI(t *testing.T) (*fakeAPI, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "obs-api.json")
	return newAPIAt(t, path, "secret-token"), path
}

// newAPIAt starts a fake plugin API and (re)writes the discovery file at path to point at it.
func newAPIAt(t *testing.T, path, token string) *fakeAPI {
	t.Helper()
	f := &fakeAPI{token: token}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+f.token {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"bad token"}`))
			return
		}
		var req struct {
			Request string         `json:"request"`
			Data    map[string]any `json:"data"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.requests = append(f.requests, req.Request)
		f.mu.Unlock()
		status, body := 200, map[string]any{}
		if f.handler != nil {
			status, body = f.handler(req.Request, req.Data)
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(body)
	}))
	f.server = srv
	t.Cleanup(srv.Close)
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	os.WriteFile(path, []byte(fmt.Sprintf(`{"port":%d,"token":%q}`, port, f.token)), 0o600)
	return f
}

func TestOpenFindsThePluginAndCallReturnsData(t *testing.T) {
	f, path := newAPI(t)
	f.handler = func(request string, data map[string]any) (int, map[string]any) {
		if request == "GetCurrentProgramScene" {
			return 200, map[string]any{"currentProgramSceneName": "Brio 4k"}
		}
		return 200, map[string]any{}
	}
	c, err := OpenAt(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	out, err := c.Call(context.Background(), "GetCurrentProgramScene", nil)
	if err != nil || out["currentProgramSceneName"] != "Brio 4k" {
		t.Fatalf("%v %v", out, err)
	}
	if f.requests[0] != "GetVersion" {
		t.Fatal("Open must check that the plugin answers")
	}
}

func TestMissingOrStaleDiscoveryMeansNotRunning(t *testing.T) {
	if _, err := OpenAt(context.Background(), filepath.Join(t.TempDir(), "none.json")); err != ErrNotRunning {
		t.Fatalf("%v", err)
	}
	f, path := newAPI(t)
	f.token = "another-run" // the plugin restarted: the file's token is stale
	if _, err := OpenAt(context.Background(), path); err != ErrNotRunning {
		t.Fatalf("%v", err)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte(`{"port":0,"token":""}`), 0o600)
	if _, err := OpenAt(context.Background(), bad); err != ErrNotRunning {
		t.Fatalf("%v", err)
	}
}

func TestRejectedRequestCarriesTheCode(t *testing.T) {
	f, path := newAPI(t)
	c, err := OpenAt(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	f.handler = func(string, map[string]any) (int, map[string]any) {
		return 409, map[string]any{"error": "no", "code": 601}
	}
	_, err = c.Call(context.Background(), "SetCurrentProgramScene", map[string]any{"sceneName": "x"})
	var oe *Error
	if !errors.As(err, &oe) || oe.Code != 601 {
		t.Fatalf("%v", err)
	}
}

func TestSlowPluginTimesOut(t *testing.T) {
	f, path := newAPI(t)
	c, err := OpenAt(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	c.Timeout = 150 * time.Millisecond
	f.handler = func(string, map[string]any) (int, map[string]any) {
		time.Sleep(600 * time.Millisecond)
		return 200, map[string]any{}
	}
	if _, err := c.Call(context.Background(), "GetStats", nil); err == nil {
		t.Fatal("a call that exceeds the timeout must fail")
	}
}

func TestDiscoveryPathIsPrivateToTheUser(t *testing.T) {
	t.Setenv("LOCALAPPDATA", `C:\Users\someone\AppData\Local`)
	if got := DiscoveryPath(); got != `C:\Users\someone\AppData\Local\TikTokLiveOBS\obs-api.json` {
		t.Fatalf("%s", got)
	}
}

func TestAClientOpenedBeforeAnObsRestartFollowsTheNewInstance(t *testing.T) {
	first, path := newAPI(t)
	first.handler = func(string, map[string]any) (int, map[string]any) { return 200, map[string]any{"from": "first"} }
	c, err := OpenAt(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	// OBS restarts: the first instance is gone, a second one has a new port and token in the same file
	second := newAPIAt(t, path, "token-of-the-new-run")
	second.handler = func(string, map[string]any) (int, map[string]any) { return 200, map[string]any{"from": "second"} }
	first.server.Close()
	out, err := c.Call(context.Background(), "GetStats", nil)
	if err != nil || out["from"] != "second" {
		t.Fatalf("%v %v", out, err)
	}
	if len(second.requests) != 1 {
		t.Fatalf("the request must reach the new instance exactly once: %v", second.requests)
	}
}

func TestAStaleTokenIsRefreshedFromTheFile(t *testing.T) {
	f, path := newAPI(t)
	c, err := OpenAt(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	f.token = "rotated" // same instance now expects another token, and the file says so
	os.WriteFile(path, []byte(fmt.Sprintf(`{"port":%d,"token":"rotated"}`, f.server.Listener.Addr().(*net.TCPAddr).Port)), 0o600)
	if _, err := c.Call(context.Background(), "GetStats", nil); err != nil {
		t.Fatal(err)
	}
}

func TestNothingIsRepeatedWhenTheFileHasNotChanged(t *testing.T) {
	f, path := newAPI(t)
	c, err := OpenAt(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	f.server.Close() // OBS gone and no new file: fail, once, without a retry loop
	if _, err := c.Call(context.Background(), "StartStream", nil); err == nil {
		t.Fatal("an unreachable OBS must fail")
	}
}

func TestATimeoutIsNeverRetried(t *testing.T) {
	f, path := newAPI(t)
	c, err := OpenAt(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	c.Timeout = 100 * time.Millisecond
	f.handler = func(string, map[string]any) (int, map[string]any) {
		time.Sleep(400 * time.Millisecond)
		return 200, map[string]any{}
	}
	before := f.requestCount()
	c.Call(context.Background(), "StartStream", nil)
	time.Sleep(600 * time.Millisecond)
	if n := f.requestCount() - before; n != 1 {
		t.Fatalf("StartStream was sent %d times; a timeout has an unknown outcome and must not be repeated", n)
	}
}
