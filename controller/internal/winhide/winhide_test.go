// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package winhide

import "testing"

func TestParseBrowserPID(t *testing.T) {
	cases := []struct {
		name, in string
		want     int
	}{
		{"empty", "", 0},
		{"null", "null", 0},
		{"garbage", "not json", 0},
		{"single browser process", `{"ProcessId":4242,"CommandLine":"chrome.exe --user-data-dir=X"}`, 4242},
		{"single helper process", `{"ProcessId":7,"CommandLine":"chrome.exe --type=renderer"}`, 0},
		{"list picks the one without --type", `[{"ProcessId":1,"CommandLine":"chrome.exe --type=gpu-process"},{"ProcessId":2,"CommandLine":"chrome.exe --user-data-dir=X"},{"ProcessId":3,"CommandLine":"chrome.exe --type=utility"}]`, 2},
		{"missing command line counts as browser", `[{"ProcessId":9}]`, 9},
	}
	for _, c := range cases {
		if got := ParseBrowserPID(c.in); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

func TestZeroPidIsANoOp(t *testing.T) {
	if Hide(0, false) != 0 || ShowLogin(0) != 0 {
		t.Fatal("pid 0 must do nothing")
	}
}
