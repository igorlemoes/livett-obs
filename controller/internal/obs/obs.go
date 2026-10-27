// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// Package obs talks to OBS through the plugin's local control API (plugin/livett/src/obs-api.cpp), which
// answers the obs-websocket request names the controller uses, from inside OBS: no WebSocket server, port or password
// to configure. It also holds the profile-scoped stream stop adapter. GetOutputList is never called (it crashes
// some OBS builds).
package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Error is a failed OBS request; Code is the request status code (0 when it is not a request failure).
type Error struct {
	Msg  string
	Code int
}

func (e *Error) Error() string { return e.Msg }

func fail(format string, args ...any) error { return &Error{Msg: fmt.Sprintf(format, args...)} }

// ErrNotRunning means the plugin's API is not reachable: OBS is closed, or the plugin is not loaded.
var ErrNotRunning = errors.New("OBS is not running or the LiveTT plugin is not loaded")

// Caller is what the stream adapter needs; *Client implements it and tests fake it.
type Caller interface {
	Call(ctx context.Context, requestType string, data map[string]any) (map[string]any, error)
}

// DiscoveryPath is the file the plugin writes (per Windows user) with the API port and a token for this OBS run.
func DiscoveryPath() string {
	if p := os.Getenv("TIKTOK_OBS_API_FILE"); p != "" { // tests: a second OBS must not share the real one's file
		return p
	}
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "TikTokLiveOBS", "obs-api.json")
}

type Client struct {
	URL, Token string
	Timeout    time.Duration
	http       *http.Client

	path string     // discovery file this client came from ("" = fixed address)
	mu   sync.Mutex // protects URL and Token, which change when OBS restarts
}

// Open finds the running plugin through the discovery file and checks that it answers.
func Open(ctx context.Context) (*Client, error) {
	return OpenAt(ctx, DiscoveryPath())
}

// OpenAt is Open for a given discovery file (tests).
func OpenAt(ctx context.Context, path string) (*Client, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, ErrNotRunning
	}
	var info struct {
		Port  int    `json:"port"`
		Token string `json:"token"`
	}
	if json.Unmarshal(raw, &info) != nil || info.Port <= 0 || info.Port > 65535 || info.Token == "" {
		return nil, ErrNotRunning
	}
	c := newClient(fmt.Sprintf("http://127.0.0.1:%d", info.Port), info.Token, 5*time.Second)
	c.path = path
	probe, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := c.Call(probe, "GetVersion", nil); err != nil {
		return nil, ErrNotRunning
	}
	return c, nil
}

func newClient(url, token string, timeout time.Duration) *Client {
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	return &Client{URL: url, Token: token, Timeout: timeout,
		http: &http.Client{Transport: &http.Transport{DialContext: dialer.DialContext, DisableKeepAlives: true}}}
}

// Close is kept for the callers that hold a connection; there is none to close.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// Call sends one request. A request OBS refuses comes back as *Error carrying OBS's status code.
//
// If OBS was restarted since this client was opened (a new port and token in the discovery file), the first failure
// to reach it, or a stale token, is answered by reading the file again and repeating the request once. Only failures
// where OBS cannot have acted on the request qualify: a refused connection or a refused token. A timeout never does.
func (c *Client) Call(ctx context.Context, requestType string, data map[string]any) (map[string]any, error) {
	out, err := c.call(ctx, requestType, data)
	if err != nil && c.path != "" && retrySafe(err) && c.rediscover() {
		return c.call(ctx, requestType, data)
	}
	return out, err
}

func retrySafe(err error) bool {
	if errors.Is(err, ErrNotRunning) {
		return true
	}
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// rediscover re-reads the discovery file and reports whether the address or token changed.
func (c *Client) rediscover() bool {
	raw, err := os.ReadFile(c.path)
	if err != nil {
		return false
	}
	var info struct {
		Port  int    `json:"port"`
		Token string `json:"token"`
	}
	if json.Unmarshal(raw, &info) != nil || info.Port <= 0 || info.Port > 65535 || info.Token == "" {
		return false
	}
	url := fmt.Sprintf("http://127.0.0.1:%d", info.Port)
	c.mu.Lock()
	defer c.mu.Unlock()
	if url == c.URL && info.Token == c.Token {
		return false
	}
	c.URL, c.Token = url, info.Token
	return true
}

func (c *Client) call(ctx context.Context, requestType string, data map[string]any) (map[string]any, error) {
	if data == nil {
		data = map[string]any{}
	}
	body, err := json.Marshal(map[string]any{"request": requestType, "data": data})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	c.mu.Lock()
	url, token := c.URL, c.Token
	c.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/call", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var reply map[string]any
	if json.Unmarshal(raw, &reply) != nil {
		return nil, fail("OBS sent invalid JSON")
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return reply, nil
	case http.StatusConflict:
		code, _ := reply["code"].(float64)
		return nil, &Error{Msg: fmt.Sprintf("%s rejected (%d)", requestType, int(code)), Code: int(code)}
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, ErrNotRunning // a stale token from an earlier OBS run
	}
	return nil, fail("%s failed (HTTP %d)", requestType, resp.StatusCode)
}

// StreamOutput stops (or starts) the stream only while OBS still points at the profile and destination the
// broadcast was created with, so a shutdown can never act on an unrelated output.
// NativeServiceType is the service type the plugin registers in OBS ("LiveTT"). With it the destination lives in the
// plugin, not in the OBS settings, so there is no server or key to compare: only the profile and the type are checked.
const NativeServiceType = "tiktok_live"

type StreamOutput struct {
	Client               Caller
	Profile, Server, Key string // Key is checked only when non-empty
	Native               bool   // the OBS button started this stream through the plugin's service
	RestartBlocked       bool
	PollInterval         time.Duration
	pollAttempts         int
}

func NewStreamOutput(client Caller, profile, server, key string) *StreamOutput {
	return &StreamOutput{Client: client, Profile: profile, Server: server, Key: key, PollInterval: 100 * time.Millisecond, pollAttempts: 20}
}

func str(m map[string]any, key string) string { s, _ := m[key].(string); return s }
func flag(m map[string]any, key string) bool  { b, _ := m[key].(bool); return b }

func (s *StreamOutput) ValidateTarget(ctx context.Context) error {
	profile, err := s.Client.Call(ctx, "GetProfileList", nil)
	if err != nil {
		return err
	}
	service, err := s.Client.Call(ctx, "GetStreamServiceSettings", nil)
	if err != nil {
		return err
	}
	if str(profile, "currentProfileName") != s.Profile {
		return fail("OBS profile changed; refusing to operate on another output")
	}
	if s.Native {
		// The plugin hands the profile its own service back as soon as the stream stops, so the type cannot be required:
		// the profile is what identifies "our" output.
		return nil
	}
	settings, _ := service["streamServiceSettings"].(map[string]any)
	if str(service, "streamServiceType") != "rtmp_custom" || str(settings, "server") != s.Server {
		return fail("OBS destination changed; refusing to operate on another output")
	}
	if s.Key != "" && str(settings, "key") != s.Key {
		return fail("OBS stream key changed; refusing to operate on another output")
	}
	return nil
}

func (s *StreamOutput) Start(ctx context.Context) error {
	if s.RestartBlocked {
		return fail("Restart is blocked for this broadcast")
	}
	if err := s.ValidateTarget(ctx); err != nil {
		return err
	}
	_, err := s.Client.Call(ctx, "StartStream", nil)
	return err
}

// Stop latches the restart block first, then stops and verifies. Returns whether OBS confirmed it is idle.
func (s *StreamOutput) Stop(ctx context.Context) (bool, error) {
	s.RestartBlocked = true
	if err := s.ValidateTarget(ctx); err != nil {
		return false, err
	}
	status, err := s.Client.Call(ctx, "GetStreamStatus", nil)
	if err != nil {
		return false, err
	}
	if flag(status, "outputActive") || flag(status, "outputReconnecting") {
		if _, err := s.Client.Call(ctx, "StopStream", nil); err != nil {
			return false, err
		}
	}
	attempts := s.pollAttempts
	if attempts == 0 {
		attempts = 20
	}
	for i := 0; i < attempts; i++ {
		status, err = s.Client.Call(ctx, "GetStreamStatus", nil)
		if err != nil {
			return false, err
		}
		if !flag(status, "outputActive") && !flag(status, "outputReconnecting") {
			return true, nil
		}
		select {
		case <-time.After(s.PollInterval):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return false, nil
}
