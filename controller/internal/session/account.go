// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package session

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// AccountInfo is what TikTok's own account endpoint says about the signed-in user (name, handle, photo).
type AccountInfo struct {
	ID, Name, Handle, Avatar string
}

// LoginInfo is what the sign-in window hands over once the user finished: who signed in and a PKCE ticket that the
// LIVE service exchanges for its own session, so no cookie has to be copied between domains.
type LoginInfo struct {
	Info                        AccountInfo
	Ticket, Verifier, Challenge string
	At                          time.Time
}

// isTikTokDomain: the web site and the LIVE service. Nothing else in the profile is ever read or deleted.
func isTikTokDomain(domain string) bool {
	d := strings.TrimPrefix(strings.ToLower(domain), ".")
	for _, root := range []string{"tiktok.com", "tiktokv.com"} {
		if d == root || strings.HasSuffix(d, "."+root) {
			return true
		}
	}
	return false
}

var cookieKeys = []string{"name", "value", "domain", "path", "httpOnly", "secure", "sameSite", "priority"}

// AccountCookies returns the profile's TikTok cookies in the shape Network.setCookies accepts.
func (s *Session) AccountCookies(ctx context.Context) ([]map[string]any, error) {
	page := s.currentPage()
	if page == nil {
		return nil, fail("Conecte o navegador primeiro.")
	}
	reply, err := cdp(ctx, page.WebSocketDebuggerURL, "Network.getAllCookies", map[string]any{})
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, c := range cookieList(reply) {
		if !isTikTokDomain(str(c, "domain")) {
			continue
		}
		keep := map[string]any{}
		for _, k := range cookieKeys {
			if v, ok := c[k]; ok && v != nil && v != "" {
				keep[k] = v
			}
		}
		if exp := floatOf(c["expires"]); exp > 0 {
			keep["expires"] = exp
		}
		out = append(out, keep)
	}
	return out, nil
}

// RestoreCookies puts a saved session back into the profile. The caller signs out first.
func (s *Session) RestoreCookies(ctx context.Context, cookies []map[string]any) error {
	page := s.currentPage()
	if page == nil {
		return fail("Conecte o navegador primeiro.")
	}
	list := make([]any, 0, len(cookies))
	for _, c := range cookies {
		if !isTikTokDomain(str(c, "domain")) {
			continue // a tampered file cannot plant a cookie for another site
		}
		list = append(list, c)
	}
	if len(list) == 0 {
		return fail("A sessão salva está vazia.")
	}
	if _, err := cdp(ctx, page.WebSocketDebuggerURL, "Network.setCookies", map[string]any{"cookies": list}); err != nil {
		return fail("Não foi possível restaurar a sessão salva.")
	}
	return nil
}

// Fingerprint identifies which account the profile currently holds (a truncated hash of the session cookie; the
// cookie itself never leaves this function).
func (s *Session) Fingerprint(ctx context.Context) string {
	page := s.currentPage()
	if page == nil {
		return ""
	}
	return s.fingerprintOf(ctx, page.WebSocketDebuggerURL)
}

// ---- who is signed in

const accountInfoScript = `(async()=>{try{const r=await fetch('https://www.tiktok.com/passport/account/info/v2/?aid=1988&language=pt-BR',{credentials:'include',headers:{sdk_aid:'1988'},signal:AbortSignal.timeout(8000)});return await r.json();}catch(e){return null;}})()`

func ticketScript(challenge string) string {
	q := url.Values{"code_challenge": {challenge}, "aid": {"1988"}, "language": {"en"}}.Encode()
	return `(async()=>{try{const r=await fetch('https://www.tiktok.com/passport/web/gen_pkce_verify_ticket/?` + q + `',{credentials:'include',headers:{sdk_aid:'1988'},signal:AbortSignal.timeout(8000)});return await r.json();}catch(e){return null;}})()`
}

func evalIn(ctx context.Context, ws, expression string) map[string]any {
	reply, err := cdp(ctx, ws, "Runtime.evaluate", map[string]any{"expression": expression, "awaitPromise": true, "returnByValue": true})
	if err != nil || reply["exceptionDetails"] != nil {
		return nil
	}
	inner, _ := reply["result"].(map[string]any)
	value, _ := inner["value"].(map[string]any)
	return value
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case json.Number:
			return v.String()
		}
	}
	return ""
}

// parseAccountInfo reads the endpoint's answer defensively: its field names are not documented.
func parseAccountInfo(reply map[string]any) AccountInfo {
	data, _ := reply["data"].(map[string]any)
	if data == nil {
		return AccountInfo{}
	}
	info := AccountInfo{
		ID:     firstString(data, "user_id_str", "user_id", "uid"),
		Name:   firstString(data, "screen_name", "nickname", "name"),
		Handle: firstString(data, "username", "unique_id", "handle"),
		Avatar: firstString(data, "avatar_url", "avatar_larger", "avatar_thumb"),
	}
	if !strings.HasPrefix(info.Avatar, "https://") {
		info.Avatar = ""
	}
	return info
}

var handlePattern = regexp.MustCompile(`^[A-Za-z0-9_.]{1,40}$`)

// profileAvatarScript reads the account's own public profile page for its photo, for accounts whose passport record
// has none. The handle is validated before it goes into the script.
func profileAvatarScript(handle string) string {
	return `(async()=>{try{const r=await fetch('https://www.tiktok.com/@` + handle + `',{credentials:'include',signal:AbortSignal.timeout(8000)});const t=await r.text();const m=t.match(/"avatar(?:Larger|Medium|Thumb)":"(https:[^"]+)"/);return {avatar:m?JSON.parse('"'+m[1]+'"'):''};}catch(e){return {avatar:''};}})()`
}

// fillAvatar completes info.Avatar from the profile page when the account record had no photo.
func fillAvatar(ctx context.Context, ws string, info *AccountInfo) {
	if info.Avatar != "" || !handlePattern.MatchString(info.Handle) {
		return
	}
	if reply := evalIn(ctx, ws, profileAvatarScript(info.Handle)); reply != nil {
		if a, _ := reply["avatar"].(string); strings.HasPrefix(a, "https://") {
			info.Avatar = a
		}
	}
}

// Identify asks TikTok who the profile is signed in as, from a throw-away tab on a light tiktok.com page.
func (s *Session) Identify(ctx context.Context) (AccountInfo, error) {
	state := s.readState()
	created, err := s.browserCDP(ctx, state, "Target.createTarget", map[string]any{"url": "https://www.tiktok.com/robots.txt", "background": true})
	if err != nil {
		return AccountInfo{}, err
	}
	id, _ := created["targetId"].(string)
	defer s.browserCDP(context.Background(), state, "Target.closeTarget", map[string]any{"targetId": id})
	for i := 0; i < 40; i++ {
		select {
		case <-time.After(250 * time.Millisecond):
		case <-ctx.Done():
			return AccountInfo{}, ctx.Err()
		}
		pages, err := s.pages(ctx, intOf(state, "port"))
		if err != nil {
			return AccountInfo{}, err
		}
		for _, p := range pages {
			if p.ID != id || hostOf(p.URL) != "www.tiktok.com" {
				continue
			}
			// The target's URL is the one being loaded: wait until the page really is on the site (not about:blank).
			if ready := evalIn(ctx, p.WebSocketDebuggerURL, `({o:location.origin,r:document.readyState})`); ready != nil &&
				ready["o"] == "https://www.tiktok.com" && ready["r"] != "loading" {
				info := parseAccountInfo(evalIn(ctx, p.WebSocketDebuggerURL, accountInfoScript))
				fillAvatar(ctx, p.WebSocketDebuggerURL, &info)
				return info, nil
			}
		}
	}
	return AccountInfo{}, fail("Não foi possível identificar a conta.")
}

// ---- the sign-in window

// NewVerifier returns a PKCE code verifier and its challenge. The challenge is the MD5 (hex) of the verifier.
func NewVerifier() (verifier, challenge string) {
	raw := make([]byte, 32)
	rand.Read(raw)
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := md5.Sum([]byte(verifier))
	return verifier, hex.EncodeToString(sum[:])
}

// TakeLogin returns (once) what the last completed sign-in window produced.
func (s *Session) TakeLogin() *LoginInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	info := s.login
	s.login = nil
	if info != nil && time.Since(info.At) > 3*time.Minute {
		return nil // a ticket from an earlier sign-in must never be applied to whatever session is active now
	}
	return info
}

// LoginPending reports that the sign-in window is still being followed: the account has not been handed over yet.
func (s *Session) LoginPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loginRunning
}

// forgetLogin drops a pending sign-in result and stops following its window (used when the session is cleared).
func (s *Session) forgetLogin() {
	s.mu.Lock()
	s.login = nil
	cancel := s.loginWatch
	s.loginWatch = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Session) setLogin(info *LoginInfo) {
	s.mu.Lock()
	s.login = info
	s.mu.Unlock()
}

// watchLogin follows the sign-in window: as soon as it leaves the sign-in page signed in (it
// lands on TikTok's home feed, which would start playing videos) the window is closed, after taking the PKCE ticket
// and the account's identity from it. Ends when the window is gone or after 15 minutes.
func (s *Session) watchLogin(targetID string) {
	s.mu.Lock()
	if s.loginWatch != nil {
		s.loginWatch()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	s.loginWatch = cancel
	s.loginGen++
	gen := s.loginGen
	s.loginRunning = true
	s.mu.Unlock()
	go func() {
		defer func() {
			cancel()
			s.mu.Lock()
			if s.loginGen == gen {
				s.loginRunning = false
			}
			s.mu.Unlock()
		}()
		state := s.readState()
		for {
			select {
			case <-time.After(600 * time.Millisecond):
			case <-ctx.Done():
				return
			}
			pages, err := s.pages(ctx, intOf(state, "port"))
			if err != nil {
				return
			}
			var tab *Page
			for i := range pages {
				if pages[i].ID == targetID {
					tab = &pages[i]
					break
				}
			}
			if tab == nil {
				return // the user closed the window
			}
			if hostOf(tab.URL) != "www.tiktok.com" || strings.HasPrefix(pathOf(tab.URL), "/login") {
				continue
			}
			if s.fingerprintOf(ctx, tab.WebSocketDebuggerURL) == "" {
				continue // not signed in yet (e.g. a consent page in the middle of the flow)
			}
			verifier, challenge := NewVerifier()
			login := &LoginInfo{Verifier: verifier, Challenge: challenge, At: time.Now()}
			login.Info = parseAccountInfo(evalIn(ctx, tab.WebSocketDebuggerURL, accountInfoScript))
			fillAvatar(ctx, tab.WebSocketDebuggerURL, &login.Info)
			if reply := evalIn(ctx, tab.WebSocketDebuggerURL, ticketScript(challenge)); reply != nil {
				if data, _ := reply["data"].(map[string]any); data != nil {
					login.Ticket = firstString(data, "verify_ticket")
				}
			}
			s.setLogin(login)
			s.browserCDP(ctx, state, "Target.closeTarget", map[string]any{"targetId": targetID})
			return
		}
	}()
}

func pathOf(rawurl string) string {
	u, err := url.Parse(rawurl)
	if err != nil {
		return ""
	}
	return u.Path
}

func (s *Session) fingerprintOf(ctx context.Context, ws string) string {
	reply, err := cdp(ctx, ws, "Network.getCookies", map[string]any{"urls": []any{"https://www.tiktok.com/"}})
	if err != nil {
		return ""
	}
	for _, c := range cookieList(reply) {
		if str(c, "name") == "sessionid_ss" && str(c, "value") != "" {
			sum := sha256.Sum256([]byte(str(c, "value")))
			return hex.EncodeToString(sum[:8])
		}
	}
	return ""
}
