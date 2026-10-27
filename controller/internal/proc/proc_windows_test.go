// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

//go:build windows

package proc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunningFindsThisTestAndNotAMissingProgram(t *testing.T) {
	if !Running(filepath.Base(os.Args[0])) {
		t.Fatal("the running test binary must be found")
	}
	if Running("programa-que-nao-existe-xyz.exe") {
		t.Fatal("a program that does not exist must not be found")
	}
}
