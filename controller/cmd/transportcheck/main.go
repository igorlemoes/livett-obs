// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// transportcheck exercises the Go transport against the real OBS and the managed Chrome, read-only:
// it asks OBS for its version/scene and runs the panel script's `check` (GET create_info). With the argument `continue` it also reads
// GET room/continue (the account's last room) and prints only its shape, never ids. It never starts a LIVE.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"tiktoklive/internal/assets"
	"tiktoklive/internal/obs"
	"tiktoklive/internal/session"
)

func main() {
	root := os.Args[1]
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	continueOnly := len(os.Args) > 2 && os.Args[2] == "continue"
	if client, err := obs.Open(ctx); err != nil {
		fmt.Println("OBS:", err)
		if !continueOnly {
			os.Exit(1)
		}
	} else {
		defer client.Close()
		version, err := client.Call(ctx, "GetVersion", nil)
		fmt.Println("OBS version:", version["obsVersion"], "ws:", version["obsWebSocketVersion"], err)
		scene, err := client.Call(ctx, "GetCurrentProgramScene", nil)
		fmt.Println("OBS scene:", scene["currentProgramSceneName"], err)
		stream, err := client.Call(ctx, "GetStreamStatus", nil)
		fmt.Println("OBS streaming:", stream["outputActive"], err)
	}

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
	if continueOnly {
		last, err := s.Call(ctx, "cont", nil)
		if m, ok := last.(map[string]any); ok {
			printContinue(root, m)
			delete(m, "ids") // never print ids
			delete(m, "roomKeys")
		}
		raw, _ = json.Marshal(last)
		fmt.Println("TikTok room/continue:", string(raw), err)
	}
}

// printContinue compares the room the service reports with the journal of the panel, printing only equal/different.
func printContinue(root string, m map[string]any) {
	ids, _ := m["ids"].(map[string]any)
	raw, err := os.ReadFile(filepath.Join(root, "runtime", "broadcast.json"))
	if err != nil {
		fmt.Println("journal: not readable")
		return
	}
	var j struct {
		Binding map[string]any `json:"binding"`
	}
	json.Unmarshal(raw, &j)
	for _, k := range [][2]string{{"room", "room_id"}, {"ping", "ping_room_id"}, {"stream", "stream_id"}} {
		fmt.Println("same", k[0], "id as the panel journal:", ids[k[0]] != nil && ids[k[0]] == j.Binding[k[1]])
	}
}
