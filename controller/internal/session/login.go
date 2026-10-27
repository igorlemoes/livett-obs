// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package session

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
)

// The embedded sign-in lets the user finish the TikTok login from the panel: screenshots of the sign-in tab go out,
// the user's own taps, text and keys go in. Nothing typed is logged or kept here, and only the TikTok sign-in tab
// of the managed profile can be driven (never the service tab), with navigation limited to two fixed URLs.

var loginURLs = map[string]string{"login": "https://www.tiktok.com/login", "qr": "https://www.tiktok.com/login/qrcode"}

var loginKeys = map[string]struct {
	code int
	char string
}{"Enter": {13, "\r"}, "Backspace": {8, ""}, "Tab": {9, ""}}

// MaxLoginText bounds one typed chunk.
const MaxLoginText = 64

func (s *Session) loginTab(ctx context.Context) (*Page, error) {
	state := s.readState()
	pages, err := s.pages(ctx, intOf(state, "port"))
	if err != nil {
		return nil, fail("A aba de login do TikTok não está aberta.")
	}
	for i := range pages {
		host := hostOf(pages[i].URL)
		if pages[i].Type == "page" && strings.HasSuffix(host, "tiktok.com") && host != "webcast.tiktokv.com" {
			return &pages[i], nil
		}
	}
	return nil, fail("A aba de login do TikTok não está aberta.")
}

func (s *Session) loginCDP(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	tab, err := s.loginTab(ctx)
	if err != nil {
		return nil, err
	}
	return cdp(ctx, tab.WebSocketDebuggerURL, method, params)
}

// LoginShot returns a JPEG of the sign-in page.
func (s *Session) LoginShot(ctx context.Context) ([]byte, error) {
	reply, err := s.loginCDP(ctx, "Page.captureScreenshot", map[string]any{"format": "jpeg", "quality": 65})
	if err != nil {
		return nil, err
	}
	data, _ := reply["data"].(string)
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil || len(raw) == 0 {
		return nil, fail("O navegador não devolveu a imagem.")
	}
	return raw, nil
}

func (s *Session) loginViewport(ctx context.Context) (float64, float64, error) {
	reply, err := s.loginCDP(ctx, "Page.getLayoutMetrics", map[string]any{})
	if err != nil {
		return 0, 0, err
	}
	m := asObjMap(reply["cssVisualViewport"])
	w, h := floatOf(m["clientWidth"]), floatOf(m["clientHeight"])
	if w <= 0 || h <= 0 {
		return 0, 0, fail("Não foi possível medir a página de login.")
	}
	return w, h, nil
}

// LoginTap clicks at a position given as fractions (0..1) of the page.
func (s *Session) LoginTap(ctx context.Context, fx, fy float64) error {
	if fx < 0 || fx > 1 || fy < 0 || fy > 1 {
		return fail("Posição inválida.")
	}
	w, h, err := s.loginViewport(ctx)
	if err != nil {
		return err
	}
	x, y := fx*(w-1), fy*(h-1)
	for _, kind := range []string{"mouseMoved", "mousePressed", "mouseReleased"} {
		if _, err := s.loginCDP(ctx, "Input.dispatchMouseEvent", map[string]any{"type": kind, "x": x, "y": y, "button": "left", "clickCount": 1}); err != nil {
			return err
		}
	}
	return nil
}

// LoginText types text into the focused field.
func (s *Session) LoginText(ctx context.Context, text string) error {
	if n := len([]rune(text)); n == 0 || n > MaxLoginText {
		return fail("Texto inválido.")
	}
	_, err := s.loginCDP(ctx, "Input.insertText", map[string]any{"text": text})
	return err
}

// LoginKey presses Enter, Backspace or Tab.
func (s *Session) LoginKey(ctx context.Context, key string) error {
	spec, ok := loginKeys[key]
	if !ok {
		return fail("Tecla não permitida.")
	}
	for _, kind := range []string{"rawKeyDown", "keyUp"} {
		event := map[string]any{"type": kind, "key": key, "code": key, "windowsVirtualKeyCode": spec.code, "nativeVirtualKeyCode": spec.code}
		if kind == "rawKeyDown" && spec.char != "" {
			event["text"] = spec.char
		}
		if _, err := s.loginCDP(ctx, "Input.dispatchKeyEvent", event); err != nil {
			return err
		}
	}
	return nil
}

// LoginGo opens one of the two fixed sign-in pages.
func (s *Session) LoginGo(ctx context.Context, where string) error {
	target, ok := loginURLs[where]
	if !ok {
		return fail("Destino não permitido.")
	}
	if u, err := url.Parse(target); err != nil || u.Scheme != "https" {
		return fmt.Errorf("invalid built-in url")
	}
	_, err := s.loginCDP(ctx, "Page.navigate", map[string]any{"url": target})
	return err
}

func asObjMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func floatOf(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case interface{ Float64() (float64, error) }:
		f, _ := n.Float64()
		return f
	}
	return 0
}
