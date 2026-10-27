// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

//go:build windows

// Package proc answers one question about the machine: is a program running?
package proc

import (
	"strings"
	"syscall"
	"unsafe"
)

// Running reports whether a process with this executable name (for example "obs64.exe") exists. When the process
// list cannot be read it answers true: callers use this to decide to quit, and must never do so by mistake.
func Running(name string) bool {
	snapshot, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return true
	}
	defer syscall.CloseHandle(snapshot)
	var entry syscall.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := syscall.Process32First(snapshot, &entry); err != nil {
		return true
	}
	for {
		if strings.EqualFold(syscall.UTF16ToString(entry.ExeFile[:]), name) {
			return true
		}
		if err := syscall.Process32Next(snapshot, &entry); err != nil {
			return false
		}
	}
}
