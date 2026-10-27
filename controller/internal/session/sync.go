// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package session

import (
	"context"
	"errors"
	"strings"
)

// SyncWebcastSession reproduces, inside the managed profile, the minimal domain sync the LIVE service needs: the web
// login lives on .tiktok.com, but the LIVE service (webcast.tiktokv.com) reads its own copy of the session cookie
// (sessionid_ss, aliased as sessionid) on .tiktokv.com; without it every request answers 20003.
//
// It copies only that one cookie, only between two domains of the same profile and the same user, only when the
// source exists and the target is missing or different, and it never returns, logs or traces the value.
func (s *Session) SyncWebcastSession(ctx context.Context) (bool, error) {
	page := s.currentPage()
	if page == nil {
		return false, fail("Conecte o navegador primeiro.")
	}
	sourceReply, err := cdp(ctx, page.WebSocketDebuggerURL, "Network.getCookies", map[string]any{"urls": []any{"https://www.tiktok.com/"}})
	if err != nil {
		return false, err
	}
	var source map[string]any
	for _, c := range cookieList(sourceReply) {
		if str(c, "name") == "sessionid_ss" && str(c, "domain") == ".tiktok.com" && str(c, "path") == "/" && str(c, "value") != "" {
			source = c
			break
		}
	}
	if source == nil {
		return false, nil // not signed in on the web yet: nothing to copy
	}
	targetReply, err := cdp(ctx, page.WebSocketDebuggerURL, "Network.getCookies", map[string]any{"urls": []any{"https://webcast.tiktokv.com/"}})
	if err != nil {
		return false, err
	}
	for _, c := range cookieList(targetReply) {
		if str(c, "name") == "sessionid_ss" && str(c, "value") == str(source, "value") {
			return false, nil // already in sync
		}
	}
	copies := []any{}
	for _, name := range []string{"sessionid_ss", "sessionid"} {
		item := map[string]any{"name": name, "value": source["value"], "domain": ".tiktokv.com", "path": "/",
			"httpOnly": source["httpOnly"] == true, "secure": true, "sameSite": "None"}
		if exp, ok := source["expires"].(float64); ok && exp > 0 {
			item["expires"] = exp
		} else if n, ok := source["expires"].(interface{ Float64() (float64, error) }); ok {
			if f, err := n.Float64(); err == nil && f > 0 {
				item["expires"] = f
			}
		}
		copies = append(copies, item)
	}
	if _, err := cdp(ctx, page.WebSocketDebuggerURL, "Network.setCookies", map[string]any{"cookies": copies}); err != nil {
		return false, errors.New("could not store the synchronized session")
	}
	return true, nil
}

// SignOut ends the TikTok web session of the managed profile by deleting the cookies of TikTok's own domains (web and
// LIVE service), so the next connection asks for a new sign-in. Nothing else in the profile is touched.
func (s *Session) SignOut(ctx context.Context) error {
	s.forgetLogin() // nothing from an earlier sign-in survives the sign-out
	page := s.currentPage()
	if page == nil {
		return fail("Conecte o navegador primeiro.")
	}
	reply, err := cdp(ctx, page.WebSocketDebuggerURL, "Network.getAllCookies", map[string]any{})
	if err != nil {
		return err
	}
	for _, c := range cookieList(reply) {
		domain := strings.TrimPrefix(str(c, "domain"), ".")
		if domain != "tiktok.com" && !strings.HasSuffix(domain, ".tiktok.com") && domain != "tiktokv.com" && !strings.HasSuffix(domain, ".tiktokv.com") {
			continue
		}
		if _, err := cdp(ctx, page.WebSocketDebuggerURL, "Network.deleteCookies",
			map[string]any{"name": str(c, "name"), "domain": str(c, "domain"), "path": str(c, "path")}); err != nil {
			return errors.New("could not end the TikTok session")
		}
	}
	return nil
}

func cookieList(reply map[string]any) []map[string]any {
	var out []map[string]any
	if list, ok := reply["cookies"].([]any); ok {
		for _, c := range list {
			if m, ok := c.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out
}
