// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package chrome

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func withLocalAppData(t *testing.T) string {
	local := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)
	return local
}

func TestOnlyExpectedProfileLocationsAreAllowed(t *testing.T) {
	local := withLocalAppData(t)
	if !AllowedProfile(PersistentProfile()) {
		t.Fatal("the persistent profile must be allowed")
	}
	if AllowedProfile(filepath.Join(local, "other")) {
		t.Fatal("an arbitrary folder must be refused")
	}
	if _, _, err := Launch(context.Background(), filepath.Join(local, "other"), LaunchOptions{}); err == nil {
		t.Fatal("Launch must refuse an unexpected profile")
	}
}

func TestNoEndpointWithoutMarker(t *testing.T) {
	if _, _, ok := DebugEndpoint(t.TempDir()); ok {
		t.Fatal("no marker, no endpoint")
	}
}

func runLaunch(t *testing.T, signedIn, headless bool) []string {
	withLocalAppData(t)
	profile := PersistentProfile()
	if signedIn {
		os.MkdirAll(filepath.Join(profile, "Default"), 0o755)
	} else {
		os.MkdirAll(profile, 0o755)
	}
	var got []string
	oldStart, oldEndpoint := startProcess, endpointOf
	defer func() { startProcess, endpointOf = oldStart, oldEndpoint }()
	startProcess = func(args []string) error { got = args; return nil }
	calls := 0
	endpointOf = func(string) (int, string, bool) {
		calls++
		return 1, "/x", calls > 1
	}
	if _, _, err := Launch(context.Background(), profile, LaunchOptions{Chrome: "chrome.exe", Wait: 2 * time.Second, Headless: headless}); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestHeadlessIsOptInAndNeverForAFirstLogin(t *testing.T) {
	if slices.Contains(runLaunch(t, true, false), "--headless=new") {
		t.Fatal("headless must be opt-in")
	}
	if !slices.Contains(runLaunch(t, true, true), "--headless=new") {
		t.Fatal("headless was requested for a signed-in profile")
	}
	first := runLaunch(t, false, true)
	if slices.Contains(first, "--headless=new") || slices.Contains(first, "--start-minimized") {
		t.Fatalf("first login must be visible: %v", first)
	}
	if !slices.Contains(runLaunch(t, true, false), "--start-minimized") {
		t.Fatal("a signed-in profile starts minimized")
	}
}
