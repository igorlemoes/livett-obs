// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package session

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"tiktoklive/internal/chrome"
	"tiktoklive/internal/ws"
)

// fakeChrome serves /json/list, one page websocket and the browser websocket, with scripted answers.
type fakeChrome struct {
	*httptest.Server
	mu        sync.Mutex
	pages     []Page
	evaluated []string
	commands  []string
	exception bool
	port      int
	source    []any // cookies served for www.tiktok.com
	target    []any // cookies served for webcast.tiktokv.com
	stored    []any // what Network.setCookies received
	paths     []string
	inputs    []map[string]any
}

func newFakeChrome(t *testing.T, withHelperPage bool) *fakeChrome {
	f := &fakeChrome{}
	mux := http.NewServeMux()
	mux.HandleFunc("/json/list", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewEncoder(w).Encode(f.pages)
	})
	serve := func(w http.ResponseWriter, r *http.Request) {
		c, err := ws.Accept(w, r)
		if err != nil {
			return
		}
		defer c.Close()
		_, msg, err := c.Read()
		if err != nil {
			return
		}
		var req struct {
			ID     int            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		json.Unmarshal(msg, &req)
		f.mu.Lock()
		f.commands = append(f.commands, req.Method)
		f.paths = append(f.paths, r.URL.Path)
		if strings.HasPrefix(req.Method, "Input.") || req.Method == "Page.navigate" {
			f.inputs = append(f.inputs, map[string]any{"method": req.Method, "params": req.Params})
		}
		result := map[string]any{}
		switch req.Method {
		case "Runtime.evaluate":
			expr, _ := req.Params["expression"].(string)
			f.evaluated = append(f.evaluated, expr)
			if f.exception {
				result = map[string]any{"exceptionDetails": map[string]any{"text": "boom"}}
			} else if strings.Contains(expr, "location.origin") {
				result = map[string]any{"result": map[string]any{"value": true}}
			} else if strings.Contains(expr, BridgeName+".check") {
				result = map[string]any{"result": map[string]any{"value": map[string]any{"code": 0}}}
			} else {
				result = map[string]any{"result": map[string]any{"value": map[string]any{"ready": true}}}
			}
		case "Page.captureScreenshot":
			result = map[string]any{"data": "/9j/4AAQ"} // base64 of a few JPEG header bytes
		case "Page.getLayoutMetrics":
			result = map[string]any{"cssVisualViewport": map[string]any{"clientWidth": 900.0, "clientHeight": 700.0}}
		case "Network.getCookies":
			urls, _ := req.Params["urls"].([]any)
			list := f.target
			if len(urls) > 0 && strings.Contains(fmt.Sprint(urls[0]), "www.tiktok.com") {
				list = f.source
			}
			result = map[string]any{"cookies": list}
		case "Network.setCookies":
			f.stored, _ = req.Params["cookies"].([]any)
		case "Target.createTarget":
			f.pages = append(f.pages, f.helper("new-target"))
			result = map[string]any{"targetId": "new-target"}
		}
		f.mu.Unlock()
		raw, _ := json.Marshal(map[string]any{"id": req.ID, "result": result})
		c.Write(ws.OpText, raw)
	}
	mux.HandleFunc("/devtools/page/", serve)
	mux.HandleFunc("/devtools/browser/", serve)
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	fmt.Sscanf(strings.TrimPrefix(f.URL, "http://127.0.0.1:"), "%d", &f.port)
	if withHelperPage {
		f.pages = []Page{f.helper("helper")}
	}
	return f
}

func (f *fakeChrome) helper(id string) Page {
	return Page{ID: id, Type: "page", URL: "https://webcast.tiktokv.com/webcast/game/basic/region_check/?aid=8311",
		WebSocketDebuggerURL: fmt.Sprintf("ws://127.0.0.1:%d/devtools/page/%s", f.port, id)}
}

func (f *fakeChrome) session(t *testing.T) *Session {
	root := t.TempDir()
	t.Setenv("LOCALAPPDATA", root)
	s := New(root)
	s.EndpointOf = func(string) (int, string, bool) { return f.port, "/devtools/browser/x", true }
	s.BrowserPID = func(context.Context, string) int { return 0 }
	s.LaunchChrome = func(context.Context, string, bool) (int, string, error) {
		return 0, "", &chrome.LaunchError{Msg: "should not launch"}
	}
	return s
}

func TestConnectAttachesInjectsTheScriptAndCallsAnOperation(t *testing.T) {
	f := newFakeChrome(t, true)
	s := f.session(t)
	defer s.Stop()
	ctx := context.Background()
	if err := s.Connect(ctx, "/*panel script*/"); err != nil {
		t.Fatal(err)
	}
	if !contains(f.evaluated, "/*panel script*/") {
		t.Fatalf("the panel script was not injected: %v", f.evaluated)
	}
	got, err := s.Call(ctx, "check", nil)
	if err != nil || fmt.Sprint(got.(map[string]any)["code"]) != "0" {
		t.Fatalf("%v %v", got, err)
	}
	last := f.evaluated[len(f.evaluated)-1]
	if last != "window."+BridgeName+".check(null)" {
		t.Fatalf("unexpected call expression %q", last)
	}
	raw, _ := os.ReadFile(filepath.Join(s.Root, "runtime", "browser.json"))
	var state map[string]any
	json.Unmarshal(raw, &state)
	if int(state["port"].(float64)) != f.port || state["browserPath"] != "/devtools/browser/x" {
		t.Fatalf("state not recorded: %s", raw)
	}
}

func TestConnectCreatesTheServiceTabWhenMissing(t *testing.T) {
	f := newFakeChrome(t, false)
	s := f.session(t)
	defer s.Stop()
	if err := s.Connect(context.Background(), "/*js*/"); err != nil {
		t.Fatal(err)
	}
	if !contains(f.commands, "Target.createTarget") {
		t.Fatalf("a service tab must be created: %v", f.commands)
	}
}

func TestOperationsAreWhitelistedAndRequireAConnection(t *testing.T) {
	f := newFakeChrome(t, true)
	s := f.session(t)
	if _, err := s.Call(context.Background(), "check", nil); err == nil {
		t.Fatal("a call before connecting must fail")
	}
	s.page = &Page{ID: "helper", WebSocketDebuggerURL: f.helper("helper").WebSocketDebuggerURL}
	for _, bad := range []string{"eval", "check; alert(1)", "", "__proto__"} {
		if _, err := s.Call(context.Background(), bad, nil); err == nil {
			t.Errorf("operation %q must be refused", bad)
		}
	}
}

func TestExceptionBecomesAnErrorThatWarnsAboutRepeatingCreation(t *testing.T) {
	f := newFakeChrome(t, true)
	s := f.session(t)
	defer s.Stop()
	if err := s.Connect(context.Background(), "/*js*/"); err != nil {
		t.Fatal(err)
	}
	f.exception = true
	_, err := s.Call(context.Background(), "create", map[string]any{"title": "x"})
	if err == nil || !strings.Contains(err.Error(), "Não repita a criação") {
		t.Fatalf("%v", err)
	}
}

func TestNonLocalCDPEndpointsAreRefused(t *testing.T) {
	if _, err := cdp(context.Background(), "ws://example.com:9222/devtools/browser/x", "Browser.getVersion", nil); err == nil {
		t.Fatal("a remote debugging endpoint must be refused")
	}
}

func TestLaunchFailureSurfacesItsMessage(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LOCALAPPDATA", root)
	s := New(root)
	s.EndpointOf = func(string) (int, string, bool) { return 0, "", false }
	s.LaunchChrome = func(context.Context, string, bool) (int, string, error) {
		return 0, "", &chrome.LaunchError{Msg: "Google Chrome não foi encontrado. Instale-o para usar o painel."}
	}
	err := s.Connect(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "Google Chrome não foi encontrado") {
		t.Fatalf("%v", err)
	}
}

func TestSweeperKeepsTheLoginWindowWhileSignInIsRequested(t *testing.T) {
	f := newFakeChrome(t, true)
	s := f.session(t)
	defer s.Stop()
	s.BrowserPID = func(context.Context, string) int { return 4242 }
	var mu sync.Mutex
	var keep []bool
	s.HideWindows = func(pid int, keepLogin bool) int { mu.Lock(); keep = append(keep, keepLogin); mu.Unlock(); return 1 }
	if err := s.Connect(context.Background(), "/*js*/"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.showRequested = true
	s.mu.Unlock()
	s.sweepOnce(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if len(keep) == 0 || !keep[len(keep)-1] {
		t.Fatalf("while sign-in is requested only the helper window may be hidden: %v", keep)
	}
}

func TestBridgeNameMatchesThePanelScript(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "assets", "panel_browser.js"))
	if err != nil {
		t.Fatal("panel_browser.js not found")
	}
	names := map[string]bool{}
	for _, m := range regexp.MustCompile(`__tiktokPanelV\d+`).FindAllString(string(raw), -1) {
		names[m] = true
	}
	if len(names) != 1 || !names[BridgeName] {
		t.Fatalf("script defines %v, Go calls %s", names, BridgeName)
	}
}

func contains(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

var _ = time.Second

func cookie(name, domain, value string) map[string]any {
	return map[string]any{"name": name, "domain": domain, "path": "/", "value": value, "httpOnly": true, "expires": 1893456000.0}
}

func TestSyncCopiesTheWebSessionToTheLiveServiceDomain(t *testing.T) {
	f := newFakeChrome(t, true)
	s := f.session(t)
	defer s.Stop()
	if err := s.Connect(context.Background(), "/*js*/"); err != nil {
		t.Fatal(err)
	}
	// Not signed in on the web: nothing to copy.
	if synced, err := s.SyncWebcastSession(context.Background()); err != nil || synced {
		t.Fatalf("%v %v", synced, err)
	}
	f.source = []any{cookie("sessionid_ss", ".tiktok.com", "SECRET-A"), cookie("ttwid", ".tiktok.com", "x")}
	synced, err := s.SyncWebcastSession(context.Background())
	if err != nil || !synced {
		t.Fatalf("%v %v", synced, err)
	}
	var names []string
	for _, c := range f.stored {
		m := c.(map[string]any)
		names = append(names, m["name"].(string))
		if m["domain"] != ".tiktokv.com" || m["value"] != "SECRET-A" || m["secure"] != true || m["sameSite"] != "None" || m["httpOnly"] != true {
			t.Fatalf("%v", m)
		}
	}
	if strings.Join(names, ",") != "sessionid_ss,sessionid" || len(f.stored) != 2 {
		t.Fatalf("only the session cookie and its alias may be copied: %v", names)
	}
	// Already in sync: nothing to do.
	f.stored = nil
	f.target = []any{cookie("sessionid_ss", ".tiktokv.com", "SECRET-A")}
	if synced, _ := s.SyncWebcastSession(context.Background()); synced || f.stored != nil {
		t.Fatal("an up-to-date target must be left alone")
	}
	// A stale target is replaced by the fresher session of the same profile.
	f.source = []any{cookie("sessionid_ss", ".tiktok.com", "SECRET-B")}
	if synced, _ := s.SyncWebcastSession(context.Background()); !synced || f.stored[0].(map[string]any)["value"] != "SECRET-B" {
		t.Fatal("a stale target must be refreshed")
	}
}

func TestEmbeddedLoginDrivesOnlyTheSignInTabWithinItsLimits(t *testing.T) {
	f := newFakeChrome(t, true)
	s := f.session(t)
	defer s.Stop()
	ctx := context.Background()
	if err := s.Connect(ctx, "/*js*/"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.paths, f.inputs = nil, nil // forget what Connect did
	f.pages = append(f.pages, Page{ID: "login", Type: "page", URL: "https://www.tiktok.com/login",
		WebSocketDebuggerURL: fmt.Sprintf("ws://127.0.0.1:%d/devtools/page/login", f.port)})
	f.mu.Unlock()

	if shot, err := s.LoginShot(ctx); err != nil || len(shot) == 0 {
		t.Fatalf("%v %v", shot, err)
	}
	if err := s.LoginTap(ctx, 0.5, 0.25); err != nil {
		t.Fatal(err)
	}
	if err := s.LoginText(ctx, "123456"); err != nil {
		t.Fatal(err)
	}
	if err := s.LoginKey(ctx, "Enter"); err != nil {
		t.Fatal(err)
	}
	if err := s.LoginGo(ctx, "qr"); err != nil {
		t.Fatal(err)
	}
	// every command went to the sign-in tab, never to the service tab
	for _, p := range f.paths {
		if p == "/devtools/page/helper" {
			t.Fatalf("the service tab must not receive sign-in commands: %v", f.paths)
		}
	}
	var taps []map[string]any
	for _, in := range f.inputs {
		if in["method"] == "Input.dispatchMouseEvent" {
			taps = append(taps, in["params"].(map[string]any))
		}
	}
	if len(taps) != 3 || taps[1]["type"] != "mousePressed" || taps[1]["x"] != 0.5*899 || taps[1]["y"] != 0.25*699 {
		t.Fatalf("%v", taps)
	}
	last := f.inputs[len(f.inputs)-1]
	if last["method"] != "Page.navigate" || last["params"].(map[string]any)["url"] != "https://www.tiktok.com/login/qrcode" {
		t.Fatalf("%v", last)
	}
	// limits
	for name, err := range map[string]error{
		"tap outside": s.LoginTap(ctx, 1.5, 0.5), "empty text": s.LoginText(ctx, ""), "long text": s.LoginText(ctx, strings.Repeat("x", 65)),
		"bad key": s.LoginKey(ctx, "F12"), "bad url": s.LoginGo(ctx, "https://evil.example"),
	} {
		if err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}
