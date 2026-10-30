// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// Package session is the CDP bridge to the user-authenticated, managed Chrome profile. No cookie export: operations run inside the browser through the injected panel script.
package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tiktoklive/internal/chrome"
	"tiktoklive/internal/winhide"
	"tiktoklive/internal/ws"
)

// BridgeName must match the name defined by assets/panel_browser.js (guarded by a test).
const BridgeName = "__tiktokPanelV10"

// signInURL is TikTok's own sign-in page in modal form: only the sign-in box, without the home feed.
const signInURL = "https://www.tiktok.com/login?is_modal=1&hide_toggle_login_signup=1&lang=pt-BR"

const servicePage = "https://webcast.tiktokv.com/webcast/game/basic/region_check/?aid=8311"

type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func fail(msg string) error { return &Error{msg} }

var allowedOps = map[string]bool{"check": true, "create": true, "ping": true, "verify": true, "drop": true, "probe": true, "cont": true,
	"imfetch": true, "aboutme": true, "aboutmesave": true, "roomstats": true, "chat": true, "pkce": true, "profile": true}

// Page is a Chrome target as listed by /json/list.
type Page struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// Session attaches to (and, if needed, starts) the managed Chrome. The function fields are seams for tests.
type Session struct {
	Root string

	// Hooks (defaults use the real Chrome helpers).
	LaunchChrome func(ctx context.Context, profile string, headless bool) (int, string, error)
	EndpointOf   func(profile string) (int, string, bool)
	BrowserPID   func(ctx context.Context, profile string) int
	HideWindows  func(pid int, keepLogin bool) int
	ShowLoginWin func(pid int) int
	CloseChrome  func(ctx context.Context, profile string) bool

	mu            sync.Mutex
	showRequested bool // true while the user must sign in: the login window stays visible
	visibility    string
	profile       string
	page          *Page
	sweeperStop   context.CancelFunc
	login         *LoginInfo         // set when the sign-in window finished (taken once by the controller)
	loginWatch    context.CancelFunc // the watcher that follows the sign-in window
	loginRunning  bool               // true while that watcher is still going
	loginGen      int
}

func New(root string) *Session {
	return &Session{
		Root:       root,
		visibility: "hidden",
		LaunchChrome: func(ctx context.Context, profile string, headless bool) (int, string, error) {
			return chrome.Launch(ctx, profile, chrome.LaunchOptions{Headless: headless})
		},
		EndpointOf:   chrome.DebugEndpoint,
		BrowserPID:   winhide.BrowserPID,
		HideWindows:  winhide.Hide,
		ShowLoginWin: winhide.ShowLogin,
		CloseChrome:  chrome.Close,
	}
}

func (s *Session) statePath() string { return filepath.Join(s.Root, "runtime", "browser.json") }

func (s *Session) readState() map[string]any {
	state := map[string]any{}
	raw, err := os.ReadFile(s.statePath())
	if err != nil {
		// Reference to an existing isolated profile (not cookies) from the earlier experiment.
		raw, err = os.ReadFile(filepath.Join(s.Root, "work", "login-experiment.json"))
	}
	if err == nil {
		json.Unmarshal(raw, &state)
	}
	return state
}

func (s *Session) writeState(state map[string]any) error {
	os.MkdirAll(filepath.Dir(s.statePath()), 0o755)
	raw, _ := json.Marshal(state)
	return os.WriteFile(s.statePath(), raw, 0o644)
}

func str(m map[string]any, key string) string { v, _ := m[key].(string); return v }

func intOf(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

// ---- CDP

func getJSON(ctx context.Context, rawurl string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", rawurl, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}

func isLocal(rawurl string) bool {
	u, err := url.Parse(rawurl)
	if err != nil {
		return false
	}
	h := u.Hostname()
	return h == "localhost" || h == "127.0.0.1"
}

// cdp runs one command on a fresh connection and returns its result.
func cdp(ctx context.Context, wsURL, method string, params map[string]any) (map[string]any, error) {
	if !isLocal(wsURL) {
		return nil, fail("O controle do navegador deve ser local.")
	}
	dialCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	conn, err := ws.Dial(dialCtx, wsURL)
	cancel()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	raw, _ := json.Marshal(map[string]any{"id": 1, "method": method, "params": params})
	if err := conn.Write(ws.OpText, raw); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(20 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	for {
		conn.SetReadDeadline(deadline)
		_, msg, err := conn.Read()
		if err != nil {
			return nil, err
		}
		var reply struct {
			ID     int            `json:"id"`
			Result map[string]any `json:"result"`
			Error  any            `json:"error"`
		}
		// json.Number keeps big identifiers and counters exact (no float64 rounding).
		dec := json.NewDecoder(bytes.NewReader(msg))
		dec.UseNumber()
		if dec.Decode(&reply) != nil || reply.ID != 1 {
			continue
		}
		if reply.Error != nil {
			return nil, fail("O navegador recusou a operação.")
		}
		if reply.Result == nil {
			return map[string]any{}, nil
		}
		return reply.Result, nil
	}
}

func (s *Session) currentPage() *Page {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.page
}

// Evaluate runs a JavaScript expression in the service tab and returns its (JSON) value.
func (s *Session) Evaluate(ctx context.Context, expression string) (any, error) {
	page := s.currentPage()
	if page == nil {
		return nil, fail("Conecte o navegador primeiro.")
	}
	result, err := cdp(ctx, page.WebSocketDebuggerURL, "Runtime.evaluate", map[string]any{
		"expression": expression, "awaitPromise": true, "returnByValue": true})
	if err != nil {
		return nil, err
	}
	if result["exceptionDetails"] != nil {
		return nil, fail("A operação no TikTok falhou. Não repita a criação sem verificar a sala.")
	}
	inner, _ := result["result"].(map[string]any)
	return inner["value"], nil
}

// Call runs one of the panel script's operations with JSON-encoded data.
func (s *Session) Call(ctx context.Context, method string, data any) (any, error) {
	if !allowedOps[method] {
		return nil, errors.New("Unsupported browser operation")
	}
	arg, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return s.Evaluate(ctx, fmt.Sprintf("window.%s.%s(%s)", BridgeName, method, arg))
}

func (s *Session) pages(ctx context.Context, port int) ([]Page, error) {
	var pages []Page
	if err := getJSON(ctx, fmt.Sprintf("http://127.0.0.1:%d/json/list", port), &pages); err != nil {
		return nil, err
	}
	return pages, nil
}

func hostOf(rawurl string) string {
	u, err := url.Parse(rawurl)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func (s *Session) browserCDP(ctx context.Context, state map[string]any, method string, params map[string]any) (map[string]any, error) {
	return cdp(ctx, fmt.Sprintf("ws://127.0.0.1:%d%s", intOf(state, "port"), str(state, "browserPath")), method, params)
}

// Connect attaches to the managed Chrome (starting it if needed — the user signs in there), opens the service
// tab and injects the panel script (panelJS is the content of assets/panel_browser.js).
func (s *Session) Connect(ctx context.Context, panelJS string) error {
	state := s.readState()
	profile := chrome.PersistentProfile()
	port, browserPath, running := s.EndpointOf(profile)
	startedNow := !running
	visibility := "hidden"
	if v := str(state, "visibility"); v != "" {
		visibility = v
	}
	headless := str(state, "mode") == "headless" // opt-in via runtime/browser.json
	s.mu.Lock()
	s.visibility, s.profile = visibility, profile
	s.mu.Unlock()
	if !running {
		var err error
		if port, browserPath, err = s.LaunchChrome(ctx, profile, headless); err != nil {
			var le *chrome.LaunchError
			if errors.As(err, &le) {
				return fail(le.Msg)
			}
			return err
		}
	}
	state["profile"], state["port"], state["browserPath"] = profile, port, browserPath
	if err := s.writeState(state); err != nil {
		return err
	}
	if err := s.attach(ctx, state, panelJS, startedNow, headless, visibility); err != nil {
		var se *Error
		if errors.As(err, &se) {
			return err
		}
		return fail("Mantenha aberto o Chrome de teste autenticado. Veja o guia se ele foi fechado.")
	}
	return nil
}

func (s *Session) attach(ctx context.Context, state map[string]any, panelJS string, startedNow, headless bool, visibility string) error {
	port := intOf(state, "port")
	pages, err := s.pages(ctx, port)
	if err != nil {
		return err
	}
	var page *Page
	for i := range pages {
		if pages[i].Type == "page" && hostOf(pages[i].URL) == "webcast.tiktokv.com" {
			page = &pages[i]
			break
		}
	}
	if page == nil {
		created, err := s.browserCDP(ctx, state, "Target.createTarget", map[string]any{"url": servicePage, "newWindow": true})
		if err != nil {
			return err
		}
		targetID, _ := created["targetId"].(string)
		for i := 0; i < 75 && page == nil; i++ { // a cold Chrome can take ~10 s to load the helper page
			select {
			case <-time.After(200 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
			pages, err = s.pages(ctx, port)
			if err != nil {
				return err
			}
			for j := range pages {
				if pages[j].ID == targetID {
					s.mu.Lock()
					s.page = &pages[j]
					s.mu.Unlock()
					ready, err := s.Evaluate(ctx, "location.origin==='https://webcast.tiktokv.com'")
					if err == nil && ready == true {
						page = &pages[j]
					}
					break
				}
			}
		}
		if page == nil {
			return fail("A aba do serviço não terminou de carregar.")
		}
	}
	s.mu.Lock()
	s.page = page
	s.mu.Unlock()
	if _, err := s.Evaluate(ctx, panelJS); err != nil {
		return err
	}
	if !s.keepLogin() {
		s.closeStrayTabs(ctx, false) // the start page (TikTok's home) would play videos in the hidden window
	}
	switch {
	case visibility == "minimized":
		s.tuckHelper(ctx, state)
		if startedNow && !headless {
			s.minimizeAll(ctx, state)
		}
	case !headless:
		s.sweepOnce(ctx)
		s.startSweeper()
	}
	return nil
}

// minimizeAll keeps every window out of the way after Chrome was started by us (new windows map late).
func (s *Session) minimizeAll(ctx context.Context, state map[string]any) {
	for attempt := 0; attempt < 4; attempt++ {
		pages, err := s.pages(ctx, intOf(state, "port"))
		if err == nil {
			visible := 0
			for _, page := range pages {
				if page.Type != "page" {
					continue
				}
				w, err := s.browserCDP(ctx, state, "Browser.getWindowForTarget", map[string]any{"targetId": page.ID})
				if err != nil {
					continue
				}
				window := w["windowId"]
				b, err := s.browserCDP(ctx, state, "Browser.getWindowBounds", map[string]any{"windowId": window})
				if err != nil {
					continue
				}
				if bounds, _ := b["bounds"].(map[string]any); str(bounds, "windowState") != "minimized" {
					visible++
					s.browserCDP(ctx, state, "Browser.setWindowBounds", map[string]any{"windowId": window, "bounds": map[string]any{"windowState": "minimized"}})
				}
			}
			if visible == 0 {
				return
			}
		}
		select {
		case <-time.After(600 * time.Millisecond):
		case <-ctx.Done():
			return
		}
	}
}

// tuckHelper minimizes the helper tab's window when it is alone in it (never the login window). Cosmetic only.
func (s *Session) tuckHelper(ctx context.Context, state map[string]any) {
	page := s.currentPage()
	pages, err := s.pages(ctx, intOf(state, "port"))
	if err != nil || page == nil {
		return
	}
	windows := map[string]any{}
	count := map[any]int{}
	for _, p := range pages {
		if p.Type != "page" {
			continue
		}
		reply, err := s.browserCDP(ctx, state, "Browser.getWindowForTarget", map[string]any{"targetId": p.ID})
		if err != nil {
			return
		}
		windows[p.ID] = reply["windowId"]
		count[reply["windowId"]]++
	}
	mine, ok := windows[page.ID]
	if !ok || count[mine] != 1 {
		return
	}
	for i := 0; i < 3; i++ {
		s.browserCDP(ctx, state, "Browser.setWindowBounds", map[string]any{"windowId": mine, "bounds": map[string]any{"windowState": "minimized"}})
		time.Sleep(500 * time.Millisecond)
		b, err := s.browserCDP(ctx, state, "Browser.getWindowBounds", map[string]any{"windowId": mine})
		if err == nil {
			if bounds, _ := b["bounds"].(map[string]any); str(bounds, "windowState") == "minimized" {
				return
			}
		}
	}
}

func (s *Session) pid(ctx context.Context) int {
	s.mu.Lock()
	profile := s.profile
	s.mu.Unlock()
	if profile == "" {
		return 0
	}
	return s.BrowserPID(ctx, profile)
}

func (s *Session) keepLogin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.showRequested
}

// sweepOnce takes the browser's windows off screen. While sign-in is needed only the helper window is hidden.
func (s *Session) sweepOnce(ctx context.Context) {
	if pid := s.pid(ctx); pid != 0 {
		s.HideWindows(pid, s.keepLogin())
	}
}

// startSweeper keeps windows hidden when they reappear (new window, restored by the user), cheaply.
func (s *Session) startSweeper() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sweeperStop != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.sweeperStop = cancel
	go func() {
		pid := 0
		for {
			if pid == 0 {
				pid = s.pid(ctx)
			}
			if pid != 0 {
				s.HideWindows(pid, s.keepLogin())
			}
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Stop ends the background window sweeper.
func (s *Session) Stop() {
	s.mu.Lock()
	cancel, watch := s.sweeperStop, s.loginWatch
	s.sweeperStop, s.loginWatch = nil, nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if watch != nil {
		watch()
	}
}

// ReleaseLogin: sign-in is done. The sign-in tabs are closed (TikTok's home page keeps playing videos, with sound,
// in a hidden window) and only the helper tab stays.
func (s *Session) ReleaseLogin(ctx context.Context) {
	s.mu.Lock()
	was := s.showRequested
	s.showRequested = false
	s.mu.Unlock()
	if was {
		s.closeStrayTabs(ctx, true)
		s.sweepOnce(ctx)
	}
}

// closeStrayTabs closes every tab except the helper's: the TikTok home page, a blank start tab and, when
// includeLogin is set, the sign-in tabs too. The session lives in the profile's cookies, so nothing is lost.
func (s *Session) closeStrayTabs(ctx context.Context, includeLogin bool) {
	helper := s.currentPage()
	if helper == nil {
		return
	}
	state := s.readState()
	pages, err := s.pages(ctx, intOf(state, "port"))
	if err != nil {
		return
	}
	for _, p := range pages {
		if p.Type != "page" || p.ID == helper.ID {
			continue
		}
		if !includeLogin && strings.Contains(p.URL, "tiktok.com/login") {
			continue
		}
		s.browserCDP(ctx, state, "Target.closeTarget", map[string]any{"targetId": p.ID})
	}
}

func (s *Session) isHeadless(ctx context.Context, state map[string]any) bool {
	v, err := s.browserCDP(ctx, state, "Browser.getVersion", map[string]any{})
	if err != nil {
		return false
	}
	return strings.Contains(str(v, "product"), "Headless")
}

// ShowLogin brings the TikTok sign-in window to the front so the user can log in (never automated).
func (s *Session) ShowLogin(ctx context.Context) error {
	raw, err := os.ReadFile(s.statePath())
	if err != nil {
		return fail("Conecte o navegador primeiro.")
	}
	state := map[string]any{}
	json.Unmarshal(raw, &state)
	pages, err := s.pages(ctx, intOf(state, "port"))
	if err != nil {
		return err
	}
	helper := ""
	if p := s.currentPage(); p != nil {
		helper = p.ID
	}
	if s.isHeadless(ctx, state) {
		// A hidden browser cannot be used to sign in: reopen the same profile visibly.
		profile := str(state, "profile")
		s.CloseChrome(ctx, profile)
		port, path, err := s.LaunchChrome(ctx, profile, false)
		if err != nil {
			return err
		}
		state["port"], state["browserPath"] = port, path
		s.writeState(state)
		s.mu.Lock()
		s.page = nil
		s.mu.Unlock()
		return nil
	}
	// Reuse only a tab that is already on the sign-in page (keeps what the user typed). Any other TikTok tab is stale
	// (e.g. the signed-in home page after "switch account") and is closed, so a fresh sign-in window opens.
	targetID := ""
	for _, p := range pages {
		if p.Type != "page" || p.ID == helper || !strings.HasSuffix(hostOf(p.URL), "tiktok.com") || hostOf(p.URL) == "" {
			continue
		}
		if targetID == "" && strings.Contains(p.URL, "tiktok.com/login") {
			targetID = p.ID
		} else {
			s.browserCDP(ctx, state, "Target.closeTarget", map[string]any{"targetId": p.ID})
		}
	}
	if targetID == "" {
		// The official sign-in page in its "modal" form (no feed, no videos), in a small window of its own.
		created, err := s.browserCDP(ctx, state, "Target.createTarget", map[string]any{"url": signInURL, "newWindow": true, "width": 520, "height": 660})
		if err != nil {
			return err
		}
		targetID, _ = created["targetId"].(string)
	}
	if _, err := s.browserCDP(ctx, state, "Target.activateTarget", map[string]any{"targetId": targetID}); err != nil {
		return err
	}
	s.mu.Lock()
	s.showRequested = true // the sweeper now leaves the sign-in window alone
	s.mu.Unlock()
	s.watchLogin(targetID)
	if pid := s.pid(ctx); pid != 0 {
		s.ShowLoginWin(pid)
	}
	if w, err := s.browserCDP(ctx, state, "Browser.getWindowForTarget", map[string]any{"targetId": targetID}); err == nil {
		s.browserCDP(ctx, state, "Browser.setWindowBounds", map[string]any{"windowId": w["windowId"], "bounds": map[string]any{"windowState": "normal"}})
	}
	return nil
}

// PageWebSocket is the debugging websocket of the service tab ("" until connected); telemetry attaches to it.
func (s *Session) PageWebSocket() string {
	if p := s.currentPage(); p != nil {
		return p.WebSocketDebuggerURL
	}
	return ""
}
