// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// Package chrome manages the Chrome that holds the TikTok session: a persistent profile, started on demand
// The user signs in personally in that window (QR/e-mail/verification are never
// automated); the controller only attaches over the local debugging port. No cookie export.
package chrome

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"tiktoklive/internal/ws"
)

const LoginURL = "https://www.tiktok.com/"

var candidates = []string{
	`C:\Program Files\Google\Chrome\Application\chrome.exe`,
	`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
}

// LaunchError is any failure to find, start or reach the managed browser.
type LaunchError struct{ Msg string }

func (e *LaunchError) Error() string { return e.Msg }

func launchErr(msg string) error { return &LaunchError{msg} }

// Process start and endpoint probe are variables so tests can replace them.
var (
	startProcess = func(args []string) error {
		cmd := exec.Command(args[0], args[1:]...)
		if err := cmd.Start(); err != nil {
			return err
		}
		go cmd.Wait()
		return nil
	}
	endpointOf = DebugEndpoint
)

func localAppData() string {
	if base := os.Getenv("LOCALAPPDATA"); base != "" {
		return base
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "AppData", "Local")
}

func PersistentProfile() string {
	return filepath.Join(localAppData(), "TikTokLiveOBS", "chrome-profile")
}

func resolve(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	return filepath.Clean(abs)
}

func samePath(a, b string) bool { return strings.EqualFold(resolve(a), resolve(b)) }

// FindChrome returns the first installed chrome.exe (extra candidates first).
func FindChrome(extra ...string) (string, error) {
	all := append(append([]string{}, extra...), candidates...)
	all = append(all, filepath.Join(os.Getenv("LOCALAPPDATA"), `Google\Chrome\Application\chrome.exe`))
	for _, c := range all {
		if c == "" {
			continue
		}
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c, nil
		}
	}
	return "", launchErr("Google Chrome não foi encontrado. Instale-o para usar o painel.")
}

// AllowedProfile accepts only the persistent profile.
func AllowedProfile(path string) bool { return samePath(resolve(path), PersistentProfile()) }

// DebugEndpoint reports the port and browser websocket path when the profile's Chrome answers on its debugging port.
func DebugEndpoint(profile string) (port int, browserPath string, ok bool) {
	raw, err := os.ReadFile(filepath.Join(profile, "DevToolsActivePort"))
	if err != nil {
		return 0, "", false
	}
	lines := strings.Split(strings.ReplaceAll(strings.TrimSpace(string(raw)), "\r", ""), "\n")
	if len(lines) < 2 {
		return 0, "", false
	}
	port, err = strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil {
		return 0, "", false
	}
	client := http.Client{Timeout: time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
	if err != nil {
		return 0, "", false
	}
	defer resp.Body.Close()
	var version struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&version) != nil ||
		!strings.HasSuffix(version.WebSocketDebuggerURL, lines[1]) {
		return 0, "", false
	}
	return port, lines[1], true
}

type LaunchOptions struct {
	Chrome   string        // empty = auto-detect
	Wait     time.Duration // 0 = 16 s
	Headless bool          // opt-in only; never for a first sign-in
}

// Launch starts Chrome on profile (unless it already answers) and returns its debugging port and browser path.
func Launch(ctx context.Context, profile string, opts LaunchOptions) (int, string, error) {
	if !AllowedProfile(profile) {
		return 0, "", launchErr("O perfil do navegador não corresponde ao local esperado.")
	}
	if port, path, ok := endpointOf(profile); ok {
		return port, path, nil
	}
	if err := os.MkdirAll(profile, 0o755); err != nil {
		return 0, "", err
	}
	os.Remove(filepath.Join(profile, "DevToolsActivePort")) // stale from a previous run
	exe := opts.Chrome
	if exe == "" {
		var err error
		if exe, err = FindChrome(); err != nil {
			return 0, "", err
		}
	}
	args := []string{exe, "--user-data-dir=" + profile, "--remote-debugging-port=0",
		"--no-first-run", "--no-default-browser-check", "--disable-sync", "--mute-audio"}
	_, statErr := os.Stat(filepath.Join(profile, "Default"))
	signedInBefore := statErr == nil
	if opts.Headless && signedInBefore {
		// Opt-in only. NOTE: this browser identifies itself as automated (HeadlessChrome,
		// navigator.webdriver=true); that is deliberately not disguised.
		args = append(args, "--headless=new", "--window-size=1280,800", "about:blank")
	} else {
		if signedInBefore {
			// Already signed in once: stay out of the way, and do not open TikTok's home page (it plays videos).
			args = append(args, "--start-minimized", "about:blank")
		} else {
			args = append(args, LoginURL)
		}
	}
	if err := startProcess(args); err != nil {
		return 0, "", launchErr("Não foi possível iniciar o Chrome.")
	}
	wait := opts.Wait
	if wait == 0 {
		wait = 16 * time.Second
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if port, path, ok := endpointOf(profile); ok {
			return port, path, nil
		}
		select {
		case <-time.After(250 * time.Millisecond):
		case <-ctx.Done():
			return 0, "", ctx.Err()
		}
	}
	return 0, "", launchErr("O navegador não disponibilizou o controle local.")
}

// Close gracefully closes the Chrome running on profile (this flushes cookies). True when it is gone.
func Close(ctx context.Context, profile string) bool {
	port, path, ok := endpointOf(profile)
	if !ok {
		return true
	}
	dialCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	if conn, err := ws.Dial(dialCtx, fmt.Sprintf("ws://127.0.0.1:%d%s", port, path)); err == nil {
		conn.Write(ws.OpText, []byte(`{"id":1,"method":"Browser.close"}`))
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		conn.Read()
		conn.Close()
	}
	cancel()
	for i := 0; i < 40; i++ {
		select {
		case <-time.After(250 * time.Millisecond):
		case <-ctx.Done():
			return false
		}
		if _, _, still := endpointOf(profile); !still {
			return true
		}
	}
	return false
}
