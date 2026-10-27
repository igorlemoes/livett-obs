// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package camera

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

// A phone only opens its camera on a page whose HTTPS certificate its browser trusts. A certificate made on this computer is
// never trusted (an iPhone refuses the camera even after the warning is accepted), so the preferred way is a Cloudflare
// "quick tunnel": the free cloudflared program gives this computer's phone door a public https:// address with a real
// certificate, with no account and no port opened in the router or the firewall. Only the page and the small connection
// messages cross Cloudflare; the video still goes straight from the phone to OBS.

var tunnelWait = 40 * time.Second

var tunnelAddress = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

// cloudflaredPath finds the program: an explicit setting, then the PATH, then the places the Windows installers use.
func (h *Hub) cloudflaredPath() string {
	if h.Tunnel != "" {
		if _, err := os.Stat(h.Tunnel); err == nil {
			return h.Tunnel
		}
		return ""
	}
	if p, err := exec.LookPath("cloudflared"); err == nil {
		return p
	}
	var candidates []string
	for _, base := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")} {
		if base != "" {
			candidates = append(candidates, filepath.Join(base, "cloudflared", "cloudflared.exe"))
		}
	}
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		candidates = append(candidates, filepath.Join(local, "Microsoft", "WinGet", "Links", "cloudflared.exe"))
		if found, _ := filepath.Glob(filepath.Join(local, "Microsoft", "WinGet", "Packages", "Cloudflare.cloudflared_*", "cloudflared.exe")); len(found) > 0 {
			candidates = append(candidates, found...)
		}
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// startTunnel runs cloudflared against the local port and returns the process and its public address. The process is
// tied to this one: if the controller dies, Windows ends the tunnel with it.
func startTunnel(exe string, port int) (*exec.Cmd, string, error) {
	cmd := exec.Command(exe, "tunnel", "--no-autoupdate", "--url", fmt.Sprintf("http://127.0.0.1:%d", port))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, "", err
	}
	if err := cmd.Start(); err != nil {
		return nil, "", err
	}
	bindToController(cmd)
	found := make(chan string, 1)
	closed := make(chan struct{}) // the program ended or closed its output
	go func() {
		defer close(closed)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() { // keeps reading after the address, so the program never blocks on a full pipe
			if m := tunnelAddress.FindString(scanner.Text()); m != "" {
				select {
				case found <- m:
				default:
				}
			}
		}
	}()
	select {
	case u := <-found:
		return cmd, u, nil
	case <-closed:
	case <-time.After(tunnelWait):
	}
	cmd.Process.Kill()
	cmd.Wait()
	return nil, "", errors.New("o túnel da Cloudflare não ficou pronto: confira a internet e tente de novo")
}

// cloudflaredCached is cloudflaredPath for the state the panel polls every two seconds: it looks again at most every 15 s.
func (h *Hub) cloudflaredCached() string {
	h.mu.Lock()
	if time.Since(h.exeLooked) < 15*time.Second {
		exe := h.exe
		h.mu.Unlock()
		return exe
	}
	h.mu.Unlock()
	exe := h.cloudflaredPath()
	h.mu.Lock()
	h.exe, h.exeLooked = exe, time.Now()
	h.mu.Unlock()
	return exe
}
