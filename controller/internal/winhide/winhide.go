// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// Package winhide hides/shows the top-level windows of the managed Chrome (Windows only; no effect on the
// browser itself). The browser stays a normal, non-headless Chrome with its usual
// identity: its windows are simply taken off screen and out of the taskbar (SW_HIDE) and brought back only
// for sign-in.
package winhide

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	swHide    = 0
	swShow    = 5
	swRestore = 9

	// HelperTitlePrefix: the helper tab (JSON page) is titled with its own address.
	HelperTitlePrefix = "webcast.tiktokv.com"
)

type process struct {
	ProcessID   int    `json:"ProcessId"`
	CommandLine string `json:"CommandLine"`
}

// ParseBrowserPID reads PowerShell JSON (one object or a list) of the profile's chrome.exe processes and returns
// the browser pid: the one process whose command line has no --type= (renderers/GPU/utility have one).
func ParseBrowserPID(output string) int {
	output = strings.TrimSpace(output)
	if output == "" {
		return 0
	}
	var list []process
	if err := json.Unmarshal([]byte(output), &list); err != nil {
		var one process
		if json.Unmarshal([]byte(output), &one) != nil {
			return 0
		}
		list = []process{one}
	}
	for _, p := range list {
		if !strings.Contains(p.CommandLine, "--type=") && p.ProcessID > 0 {
			return p.ProcessID
		}
	}
	return 0
}

// BrowserPID finds the browser process of the Chrome started with --user-data-dir=profile (0 when not found).
func BrowserPID(ctx context.Context, profile string) int {
	script := "Get-CimInstance Win32_Process -Filter \"Name='chrome.exe'\" | " +
		"Where-Object { $_.CommandLine -like '*user-data-dir=" + strings.ReplaceAll(profile, "'", "''") + "*' } | " +
		"Select-Object ProcessId,CommandLine | ConvertTo-Json -Compress"
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	return ParseBrowserPID(string(out))
}

var (
	user32                       = syscall.NewLazyDLL("user32.dll")
	procEnumWindows              = user32.NewProc("EnumWindows")
	procGetWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
	procGetClassName             = user32.NewProc("GetClassNameW")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procShowWindow               = user32.NewProc("ShowWindow")
	procGetWindowTextLength      = user32.NewProc("GetWindowTextLengthW")
	procGetWindowText            = user32.NewProc("GetWindowTextW")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
)

func windowsOf(pid int) []uintptr {
	var found []uintptr
	callback := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		var owner uint32
		procGetWindowThreadProcessID.Call(hwnd, uintptr(unsafe.Pointer(&owner)))
		if int(owner) == pid {
			var name [64]uint16
			procGetClassName.Call(hwnd, uintptr(unsafe.Pointer(&name[0])), 64)
			if strings.HasPrefix(syscall.UTF16ToString(name[:]), "Chrome_WidgetWin") {
				found = append(found, hwnd)
			}
		}
		return 1
	})
	procEnumWindows.Call(callback, 0)
	return found
}

func visible(hwnd uintptr) bool {
	r, _, _ := procIsWindowVisible.Call(hwnd)
	return r != 0
}

func title(hwnd uintptr) string {
	n, _, _ := procGetWindowTextLength.Call(hwnd)
	if n == 0 {
		return ""
	}
	buf := make([]uint16, n+1)
	procGetWindowText.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), n+1)
	return syscall.UTF16ToString(buf)
}

// Hide hides the visible top-level Chrome windows of pid. With keepLogin the sign-in window stays visible and
// only the helper window is hidden. Returns how many windows were hidden.
func Hide(pid int, keepLogin bool) int {
	if pid == 0 {
		return 0
	}
	count := 0
	for _, hwnd := range windowsOf(pid) {
		if !visible(hwnd) {
			continue
		}
		if keepLogin && !strings.HasPrefix(title(hwnd), HelperTitlePrefix) {
			continue
		}
		procShowWindow.Call(hwnd, swHide)
		count++
	}
	return count
}

// ShowLogin brings the sign-in window (never the helper) back and focuses it. Returns how many were shown.
func ShowLogin(pid int) int {
	if pid == 0 {
		return 0
	}
	var shown []uintptr
	for _, hwnd := range windowsOf(pid) {
		if t := title(hwnd); t != "" && !strings.HasPrefix(t, HelperTitlePrefix) {
			procShowWindow.Call(hwnd, swRestore)
			procShowWindow.Call(hwnd, swShow)
			shown = append(shown, hwnd)
		}
	}
	if len(shown) > 0 {
		procSetForegroundWindow.Call(shown[0])
	}
	return len(shown)
}
