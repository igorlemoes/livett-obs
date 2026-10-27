// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

//go:build windows

package vault

import (
	"errors"
	"syscall"
	"unsafe"
)

type dataBlob struct {
	size uint32
	data *byte
}

var (
	crypt32          = syscall.NewLazyDLL("crypt32.dll")
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procProtectData  = crypt32.NewProc("CryptProtectData")
	procUnprotect    = crypt32.NewProc("CryptUnprotectData")
	procLocalFree    = kernel32.NewProc("LocalFree")
	dpapiDescription = syscall.StringToUTF16Ptr("LiveTT")
)

// DPAPI seals data with the signed-in Windows user's key (CRYPTPROTECT_UI_FORBIDDEN, user scope).
type DPAPI struct{}

func blobOf(b []byte) *dataBlob {
	if len(b) == 0 {
		return &dataBlob{}
	}
	return &dataBlob{size: uint32(len(b)), data: &b[0]}
}

func takeBlob(out *dataBlob) []byte {
	if out.data == nil {
		return nil
	}
	buf := make([]byte, out.size)
	copy(buf, unsafe.Slice(out.data, out.size))
	procLocalFree.Call(uintptr(unsafe.Pointer(out.data)))
	return buf
}

func (DPAPI) Seal(plain []byte) ([]byte, error) {
	var out dataBlob
	r, _, _ := procProtectData.Call(uintptr(unsafe.Pointer(blobOf(plain))), uintptr(unsafe.Pointer(dpapiDescription)), 0, 0, 0, 0x1, uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return nil, errors.New("CryptProtectData failed")
	}
	return takeBlob(&out), nil
}

func (DPAPI) Open(sealed []byte) ([]byte, error) {
	var out dataBlob
	r, _, _ := procUnprotect.Call(uintptr(unsafe.Pointer(blobOf(sealed))), 0, 0, 0, 0, 0x1, uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return nil, errors.New("CryptUnprotectData failed")
	}
	return takeBlob(&out), nil
}
