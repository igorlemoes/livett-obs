// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

//go:build !windows

package camera

import "os/exec"

func bindToController(*exec.Cmd) {}
