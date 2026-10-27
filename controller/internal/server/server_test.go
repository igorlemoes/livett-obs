// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"tiktoklive/internal/controller"
)

type fakeAPI struct {
	started  controller.StartRequest
	chat     controller.ChatRequest
	thanks   *bool
	answers  *bool
	err      error
	slow     time.Duration
	schedule any
}

func (f *fakeAPI) Snapshot() Obj { return Obj{"phase": "ready"} }
func (f *fakeAPI) StartNative(_ context.Context, r controller.StartRequest) (controller.NativeTarget, error) {
	if !r.Confirmed {
		return controller.NativeTarget{}, &controller.PanelError{Msg: "Confirme."}
	}
	return controller.TestNativeTarget("rtmp://push.example/live", "k-"+r.Title), nil
}
func (f *fakeAPI) OpenStartDialog(context.Context) (Obj, error) { return Obj{"opened": true}, nil }
func (f *fakeAPI) SetContext(_ context.Context, text string) (Obj, error) {
	return Obj{"context": text}, nil
}
func (f *fakeAPI) AssistantInfo() Obj { return Obj{"claudeCode": "claude mcp add"} }
func (f *fakeAPI) UseAccount(_ context.Context, id string) (Obj, error) {
	return Obj{"phase": "ready", "used": id}, nil
}
func (f *fakeAPI) AddAccount(context.Context) (Obj, error) { return Obj{"phase": "login"}, nil }
func (f *fakeAPI) RemoveAccount(_ context.Context, id string) (Obj, error) {
	return Obj{"removed": id}, nil
}
func (f *fakeAPI) Logout(context.Context) (Obj, error) { return Obj{"phase": "disconnected"}, nil }
func (f *fakeAPI) Connect(context.Context) (Obj, error) {
	return Obj{"phase": "ready"}, f.err
}
func (f *fakeAPI) End(context.Context) (Obj, error) {
	time.Sleep(f.slow)
	return Obj{"phase": "closed"}, f.err
}
func (f *fakeAPI) Start(_ context.Context, r controller.StartRequest) (Obj, error) {
	f.started = r
	return Obj{"phase": "live"}, f.err
}
func (f *fakeAPI) SetSchedule(_ context.Context, m any) (Obj, error) {
	f.schedule = m
	return Obj{"ok": true}, f.err
}
func (f *fakeAPI) SetAutoEnd(enabled bool) Obj { return Obj{"autoEnd": enabled} }
func (f *fakeAPI) SaveAboutMe(_ context.Context, t string) (Obj, error) {
	return Obj{"text": t}, f.err
}
func (f *fakeAPI) SendChat(_ context.Context, r controller.ChatRequest) (Obj, error) {
	f.chat = r
	return Obj{"sent": "x"}, f.err
}
func (f *fakeAPI) SetAutopilot(t, a *bool) (Obj, error) {
	f.thanks, f.answers = t, a
	return Obj{"ok": true}, f.err
}

type rig struct {
	t    *testing.T
	srv  *Server
	api  *fakeAPI
	base string
	host string
}

func newRig(t *testing.T) *rig {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	api := &fakeAPI{}
	web := fstest.MapFS{
		"index.html": {Data: []byte(`<meta name="panel-token" content="__PANEL_TOKEN__">`)},
		"app.js":     {Data: []byte("console.log(1)")},
		"live.html":  {Data: []byte("<p>live</p>")},
	}
	s := New(api, web, port)
	s.OpTimeout = 300 * time.Millisecond
	go http.Serve(l, s.Handler())
	t.Cleanup(func() { l.Close() })
	return &rig{t: t, srv: s, api: api, base: fmt.Sprintf("http://127.0.0.1:%d", port), host: fmt.Sprintf("127.0.0.1:%d", port)}
}

func (r *rig) post(path string, body any, headers map[string]string) (int, Obj) {
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", r.base+path, bytes.NewReader(raw))
	req.Header.Set("Origin", "http://"+r.host)
	req.Header.Set("X-Panel-Token", r.srv.Token)
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := Obj{}
	data, _ := io.ReadAll(resp.Body)
	json.Unmarshal(data, &out)
	return resp.StatusCode, out
}

func (r *rig) get(path string) (int, string, http.Header) {
	resp, err := http.Get(r.base + path)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data), resp.Header
}

func TestCrossOriginOrMissingTokenCannotMutate(t *testing.T) {
	r := newRig(t)
	if !r.srv.ValidRequest(r.host, "http://"+r.host, r.srv.Token) {
		t.Fatal("the same origin with the token must pass")
	}
	for _, c := range []struct{ host, origin, token string }{
		{r.host, "https://example.com", r.srv.Token}, {"evil.invalid:8765", "http://evil.invalid:8765", r.srv.Token},
		{r.host, "http://" + r.host, ""}, {r.host, "http://" + r.host, "wrong"}, {r.host, "", r.srv.Token}} {
		if r.srv.ValidRequest(c.host, c.origin, c.token) {
			t.Errorf("%+v must be refused", c)
		}
	}
	for name, headers := range map[string]map[string]string{
		"no token": {"X-Panel-Token": ""}, "bad origin": {"Origin": "http://evil.invalid"}, "no origin": {"Origin": ""}} {
		if status, _ := r.post("/api/end", Obj{}, headers); status != 403 {
			t.Errorf("%s: status %d", name, status)
		}
	}
}

func TestPanelPageCarriesTheTokenAndSecurityHeaders(t *testing.T) {
	r := newRig(t)
	status, body, header := r.get("/")
	if status != 200 || !strings.Contains(body, r.srv.Token) || strings.Contains(body, "__PANEL_TOKEN__") {
		t.Fatalf("%d %s", status, body)
	}
	if !strings.Contains(header.Get("Content-Security-Policy"), "default-src 'self'") || header.Get("X-Content-Type-Options") != "nosniff" ||
		header.Get("Cache-Control") != "no-store" {
		t.Fatalf("%v", header)
	}
	if status, body, _ := r.get("/live"); status != 200 || body != "<p>live</p>" {
		t.Fatalf("the live view needs no token: %d %s", status, body)
	}
	if status, _, _ := r.get("/nope"); status != 404 {
		t.Fatalf("%d", status)
	}
	if status, body, _ := r.get("/api/state"); status != 200 || !strings.Contains(body, `"ready"`) {
		t.Fatalf("%d %s", status, body)
	}
}

func TestForeignHostHeaderIsRefusedOnGet(t *testing.T) {
	r := newRig(t)
	req, _ := http.NewRequest("GET", r.base+"/", nil)
	req.Host = "evil.invalid:8765"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("%d", resp.StatusCode)
	}
}

func TestSessionTokenIsServedOnlyToTheLoopbackHost(t *testing.T) {
	r := newRig(t)
	status, body, _ := r.get("/api/session")
	if status != 200 || !strings.Contains(body, r.srv.Token) {
		t.Fatalf("%d %s", status, body)
	}
	req, _ := http.NewRequest("GET", r.base+"/api/session", nil)
	req.Host = "evil.invalid:8765"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("foreign host got %d", resp.StatusCode)
	}
}

func TestStartParsingRequiresExactlyTrueAndCarriesTheDetails(t *testing.T) {
	r := newRig(t)
	status, out := r.post("/api/start", Obj{"title": "T", "scene": "S", "confirmed": true, "maxMinutes": 5, "productDetails": "Pizza R$ 30"}, nil)
	if status != 200 || out["phase"] != "live" {
		t.Fatalf("%d %v", status, out)
	}
	req := r.api.started
	if req.Title != "T" || req.Scene != "S" || !req.Confirmed || req.MaxMinutes != float64(5) || req.ProductDetails == nil || *req.ProductDetails != "Pizza R$ 30" {
		t.Fatalf("%+v", req)
	}
	r.post("/api/start", Obj{"title": "T", "scene": "S", "confirmed": "true"}, nil)
	if r.api.started.Confirmed {
		t.Fatal("only the JSON value true confirms a publication")
	}
}

func TestChatAndAutopilotParsing(t *testing.T) {
	r := newRig(t)
	r.post("/api/chat", Obj{"text": "oi", "replyToN": 5, "via": "thanks", "context": "evento"}, nil)
	if r.api.chat.Text != "oi" || r.api.chat.ReplyTo != 5 || r.api.chat.Via != "thanks" || fmt.Sprint(r.api.chat.Context) != "map[comment:evento]" {
		t.Fatalf("%+v", r.api.chat)
	}
	r.post("/api/chat", Obj{"text": "oi", "via": 7}, nil)
	if r.api.chat.Via == "" || r.api.chat.Via == "thanks" {
		t.Fatalf("a non-string origin must not pass as a valid one: %q", r.api.chat.Via)
	}
	if status, _ := r.post("/api/autopilot", Obj{"thanks": "yes"}, nil); status != 409 {
		t.Fatalf("%d", status)
	}
	if status, _ := r.post("/api/autopilot", Obj{"thanks": true}, nil); status != 200 || r.api.thanks == nil || !*r.api.thanks || r.api.answers != nil {
		t.Fatalf("%d", status)
	}
	if status, _ := r.post("/api/auto-end", Obj{"enabled": 1}, nil); status != 409 {
		t.Fatal("a non-boolean must be refused")
	}
}

func TestErrorsMapToConflictAndNeverLeakInternals(t *testing.T) {
	r := newRig(t)
	r.api.err = &controller.PanelError{Msg: "Conecte primeiro."}
	if status, out := r.post("/api/connect", Obj{}, nil); status != 409 || out["error"] != "Conecte primeiro." {
		t.Fatalf("%d %v", status, out)
	}
	r.api.err = errors.New("dial tcp 10.0.0.1: secret internal detail")
	status, out := r.post("/api/connect", Obj{}, nil)
	if status != 409 || strings.Contains(fmt.Sprint(out["error"]), "secret") {
		t.Fatalf("%d %v", status, out)
	}
}

func TestSlowOperationAnswers504WithoutBeingCancelled(t *testing.T) {
	r := newRig(t)
	r.api.slow = 600 * time.Millisecond
	status, out := r.post("/api/end", Obj{}, nil)
	if status != 504 || !strings.Contains(fmt.Sprint(out["error"]), "não repita a criação") {
		t.Fatalf("%d %v", status, out)
	}
}

func TestBadBodiesAndUnknownRoutes(t *testing.T) {
	r := newRig(t)
	req, _ := http.NewRequest("POST", r.base+"/api/end", strings.NewReader("[1,2]"))
	req.Header.Set("Origin", "http://"+r.host)
	req.Header.Set("X-Panel-Token", r.srv.Token)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("a JSON array is not an object: %d", resp.StatusCode)
	}
	big := Obj{"x": strings.Repeat("a", 9000)}
	if status, _ := r.post("/api/end", big, nil); status != 400 {
		t.Fatalf("an oversize body must be refused: %d", status)
	}
	if status, _ := r.post("/api/unknown", Obj{}, nil); status != 404 {
		t.Fatalf("%d", status)
	}
}

func TestAvatarRouteRefusesForeignHosts(t *testing.T) {
	r := newRig(t)
	if status, _, _ := r.get("/avatar?u=https://evil.example/a.png"); status != 404 {
		t.Fatalf("%d", status)
	}
	if status, _, _ := r.get("/avatar"); status != 404 {
		t.Fatalf("%d", status)
	}
}

func TestAccountEndpointsReachTheController(t *testing.T) {
	r := newRig(t)
	for path, key := range map[string]string{"/api/account/use": "used", "/api/account/remove": "removed"} {
		if status, out := r.post(path, Obj{"id": "42"}, nil); status != 200 || out[key] != "42" {
			t.Fatalf("%s: %d %v", path, status, out)
		}
	}
	if status, out := r.post("/api/account/add", Obj{}, nil); status != 200 || out["phase"] != "login" {
		t.Fatalf("%d %v", status, out)
	}
	if status, out := r.post("/api/account/logout", Obj{}, nil); status != 200 || out["phase"] != "disconnected" {
		t.Fatalf("%d %v", status, out)
	}
	if status, _ := r.post("/api/account/use", Obj{"id": "42"}, map[string]string{"X-Panel-Token": ""}); status != 403 {
		t.Fatal("account changes need the panel token")
	}
}

func TestContextAndAssistantInfoEndpointsNeedTheToken(t *testing.T) {
	r := newRig(t)
	if status, out := r.post("/api/context", Obj{"text": "Camiseta R$ 59"}, nil); status != 200 || out["context"] != "Camiseta R$ 59" {
		t.Fatalf("%d %v", status, out)
	}
	if status, out := r.post("/api/assistant-info", Obj{}, nil); status != 200 || out["claudeCode"] == nil {
		t.Fatalf("%d %v", status, out)
	}
	for _, path := range []string{"/api/context", "/api/assistant-info"} {
		if status, _ := r.post(path, Obj{}, map[string]string{"X-Panel-Token": ""}); status != 403 {
			t.Fatalf("%s must need the panel token", path)
		}
	}
}

func TestStartNativeEndpointReturnsTheDestinationOnlyToAConfirmedTokenCall(t *testing.T) {
	r := newRig(t)
	status, out := r.post("/api/start-native", Obj{"title": "T", "scene": "S", "confirmed": true}, nil)
	if status != 200 || out["server"] != "rtmp://push.example/live" || out["key"] != "k-T" {
		t.Fatalf("%d %v", status, out)
	}
	if status, _ := r.post("/api/start-native", Obj{"title": "T", "scene": "S", "confirmed": "true"}, nil); status != 409 {
		t.Fatalf("a non-boolean confirmation must be refused, got %d", status)
	}
	if status, _ := r.post("/api/start-native", Obj{"title": "T", "confirmed": true}, map[string]string{"X-Panel-Token": ""}); status != 403 {
		t.Fatal("the destination must never be given without the panel token")
	}
}

func TestStartDialogEndpointOnlyOpensTheWindowAndNeedsTheToken(t *testing.T) {
	r := newRig(t)
	if status, out := r.post("/api/start-dialog", Obj{}, nil); status != 200 || out["opened"] != true {
		t.Fatalf("%d %v", status, out)
	}
	if status, _ := r.post("/api/start-dialog", Obj{}, map[string]string{"X-Panel-Token": ""}); status != 403 {
		t.Fatal("the start window must not be opened without the panel token")
	}
}
