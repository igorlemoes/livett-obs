// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// Stand-in for the controller, only to prove the plugin's OBS service in a test copy of OBS.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
)

func main() {
	logPath := os.Args[2]
	var mu sync.Mutex
	note := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		f, _ := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		fmt.Fprintf(f, format+"\n", args...)
		f.Close()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		note("GET /api/state")
		json.NewEncoder(w).Encode(map[string]any{"phase": "ready", "detail": "",
			"obs":      map[string]any{"scene": "REA Synthetic Pattern"},
			"accounts": []any{map[string]any{"active": true, "name": "Conta de Teste", "handle": "contadeteste", "avatar": "https://p19-common-sign.tiktokcdn.com/x~c5_300x300.webp"}}})
	})
	mux.HandleFunc("/avatar", func(w http.ResponseWriter, r *http.Request) { note("GET /avatar"); b, _ := os.ReadFile(os.Getenv("STUB_AVATAR")); w.Header().Set("Content-Type", "image/webp"); w.Write(b) })
	mux.HandleFunc("/api/connect", func(w http.ResponseWriter, r *http.Request) { note("POST /api/connect"); json.NewEncoder(w).Encode(map[string]string{"phase": "ready"}) })
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		note("GET /api/session")
		json.NewEncoder(w).Encode(map[string]string{"token": "stub-token"})
	})
	mux.HandleFunc("/api/start-native", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		note("POST /api/start-native token=%q origin=%q body=%s", r.Header.Get("X-Panel-Token"), r.Header.Get("Origin"), body)
		if r.Header.Get("X-Panel-Token") != "stub-token" {
			w.WriteHeader(403)
			json.NewEncoder(w).Encode(map[string]string{"error": "token errado"})
			return
		}
		if os.Getenv("STUB_FAIL") != "" {
			w.WriteHeader(409)
			json.NewEncoder(w).Encode(map[string]string{"error": "O TikTok não confirmou que a conta está livre (teste)."})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"server": "rtmp://127.0.0.1:19350/live", "key": "stub-key-123"})
	})
	note("stub listening on %s", os.Args[1])
	http.ListenAndServe(os.Args[1], mux)
}


