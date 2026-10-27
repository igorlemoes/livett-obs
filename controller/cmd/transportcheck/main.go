// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// transportcheck exercises the Go transport against the real OBS and the managed Chrome, read-only:
// it asks OBS for its version/scene and runs the panel script's `check` (GET create_info). It never starts a LIVE.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"tiktoklive/internal/assets"
	"tiktoklive/internal/obs"
	"tiktoklive/internal/session"
)

func main() {
	root := os.Args[1]
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	client, err := obs.Open(ctx)
	if err != nil {
		fmt.Println("OBS:", err)
		os.Exit(1)
	}
	defer client.Close()
	version, err := client.Call(ctx, "GetVersion", nil)
	fmt.Println("OBS version:", version["obsVersion"], "ws:", version["obsWebSocketVersion"], err)
	scene, err := client.Call(ctx, "GetCurrentProgramScene", nil)
	fmt.Println("OBS scene:", scene["currentProgramSceneName"], err)
	stream, err := client.Call(ctx, "GetStreamStatus", nil)
	fmt.Println("OBS streaming:", stream["outputActive"], err)

	s := session.New(root)
	defer s.Stop()
	s.HideWindows = func(int, bool) int { return 0 } // read-only check: do not touch the user's windows
	if err := s.Connect(ctx, assets.PanelJS()); err != nil {
		fmt.Println("Chrome connect:", err)
		os.Exit(1)
	}
	result, err := s.Call(ctx, "check", nil)
	raw, _ := json.Marshal(result)
	fmt.Println("TikTok check:", string(raw), err)
}
