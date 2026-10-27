// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package main

import "testing"

func TestTheRelayAddressMustBeHTTPSAndCanBeOverridden(t *testing.T) {
	t.Setenv("TIKTOK_CAMERA_RELAY", "")
	if got := relayAddress(); got == "" || got[:8] != "https://" {
		t.Fatalf("the project's relay must be used by default: %q", got)
	}
	t.Setenv("TIKTOK_CAMERA_RELAY", "https://outro.example.com/")
	if got := relayAddress(); got != "https://outro.example.com" {
		t.Fatalf("%q", got)
	}
	t.Setenv("TIKTOK_CAMERA_RELAY", "http://127.0.0.1:8799")
	if got := relayAddress(); got != "http://127.0.0.1:8799" {
		t.Fatalf("a local relay is allowed for tests: %q", got)
	}
	for _, bad := range []string{"http://exemplo.com", "ftp://x", "javascript:alert(1)", "outro-texto"} {
		t.Setenv("TIKTOK_CAMERA_RELAY", bad)
		if got := relayAddress(); got != "" {
			t.Errorf("%q must be refused, got %q", bad, got)
		}
	}
}
