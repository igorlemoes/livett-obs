// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

//go:build windows

package camera

import (
	"os/exec"
	"syscall"
	"unsafe"
)

// jobHandle stays open for as long as the controller lives; when the controller ends (even if it is killed) Windows closes
// it and ends every process in the job.
var jobHandle uintptr

// bindToController puts the process in a job that is killed when its last handle closes.
func bindToController(cmd *exec.Cmd) {
	kernel := syscall.NewLazyDLL("kernel32.dll")
	if jobHandle == 0 {
		job, _, _ := kernel.NewProc("CreateJobObjectW").Call(0, 0)
		if job == 0 {
			return
		}
		var info [144]byte                             // JOBOBJECT_EXTENDED_LIMIT_INFORMATION (64-bit)
		*(*uint32)(unsafe.Pointer(&info[16])) = 0x2000 // JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		kernel.NewProc("SetInformationJobObject").Call(job, 9, uintptr(unsafe.Pointer(&info[0])), uintptr(len(info)))
		jobHandle = job
	}
	process, err := syscall.OpenProcess(0x0100|0x0001, false, uint32(cmd.Process.Pid)) // SET_QUOTA | TERMINATE
	if err != nil {
		return
	}
	defer syscall.CloseHandle(process)
	kernel.NewProc("AssignProcessToJobObject").Call(jobHandle, uintptr(process))
}
