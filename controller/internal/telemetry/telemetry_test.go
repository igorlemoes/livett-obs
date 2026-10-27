// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package telemetry

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tiktoklive/internal/controller"
	"tiktoklive/internal/wire"
)

func TestDescribeUnknownProtobuf(t *testing.T) {
	data := append([]byte{0x08, 0x2A, 0x12, 0x03}, "abc"...)
	got := describe(data, 3)
	raw, _ := json.Marshal(got)
	if string(raw) != `[{"f":1,"v":42},{"f":2,"len":3,"s":"abc"}]` {
		t.Fatalf("%s", raw)
	}
	if describe([]byte{0xff, 0xff}, 3) != nil {
		t.Fatal("garbage must not parse")
	}
}

func readTrace(t *testing.T, dir string) []Obj {
	raw, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []Obj
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var m Obj
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func kinds(events []Obj) map[string]int {
	m := map[string]int{}
	for _, e := range events {
		m[e["src"].(string)+"."+e["kind"].(string)]++
	}
	return m
}

func TestLifecycleWritesTheTraceAndSummaryAndNeverBlocks(t *testing.T) {
	root := t.TempDir()
	tel := New(Config{Root: root, Tail: 50 * time.Millisecond, PageWS: func() string { return "" }})
	if err := tel.Begin(context.Background(), "Meu título", controller.Obj{}); err != nil {
		t.Fatal(err)
	}
	if !tel.active() || tel.Snapshot()["active"] != true {
		t.Fatal("must be active after Begin")
	}
	tel.Event("controller", "phase", Obj{"phase": "live", "detail": "no ar"})
	tel.Finish()
	deadline := time.Now().Add(3 * time.Second)
	for tel.active() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if tel.active() {
		t.Fatal("the trace must close after the tail")
	}
	dir := tel.trace().Dir
	for i := 0; i < 100; i++ { // the summary is written right after the trace closes
		if _, err := os.Stat(filepath.Join(dir, "summary.md")); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	events := readTrace(t, dir)
	k := kinds(events)
	if k["monitor.begin"] != 1 || k["monitor.finish_scheduled"] != 1 || k["monitor.end"] != 1 || k["cdp.unavailable"] != 1 {
		t.Fatalf("%v", k)
	}
	summary, err := os.ReadFile(filepath.Join(dir, "summary.md"))
	if err != nil || !strings.Contains(string(summary), "no ar") {
		t.Fatalf("the summary must show the phase timeline: %v %s", err, summary)
	}
	tel.Event("late", "event", Obj{}) // ignored once closed
	tel.Stop()                        // idempotent
}

func TestIMBatchRecordsMessagesClassifiesSignalsAndDeduplicates(t *testing.T) {
	root := t.TempDir()
	tel := New(Config{Root: root, Tail: time.Hour, PageWS: func() string { return "" }})
	tel.Begin(context.Background(), "t", nil)
	defer tel.Stop()
	control := []byte{}
	for _, b := range "0a210a1557656263617374436f6e74726f6c4d65737361676512060a02187b10041863" + "1201632a01784801" {
		control = append(control, byte(b))
	}
	raw := mustHex("0a210a1557656263617374436f6e74726f6c4d65737361676512060a02187b100418631201632a01784801")
	_ = control
	response, err := wire.DecodeResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	tel.IMBatch(raw, response, Obj{"status": 200}, seen)
	tel.IMBatch(raw, response, Obj{"status": 200}, seen) // the same message again is flagged as a duplicate
	events := readTrace(t, tel.trace().Dir)
	var messages []Obj
	for _, e := range events {
		if e["src"] == "im" && e["kind"] == "message" {
			messages = append(messages, e["data"].(map[string]any))
		}
	}
	if len(messages) != 2 || messages[0]["classified"] != "remote_end" || messages[0]["duplicate"] != false || messages[1]["duplicate"] != true {
		t.Fatalf("%v", messages)
	}
	snapshot := tel.Snapshot()
	if snapshot["imPolls"] != 2 || snapshot["imMessages"] != 2 {
		t.Fatalf("%v", snapshot)
	}
	tel.IMBatch(nil, nil, Obj{}, seen)
	if kinds(readTrace(t, tel.trace().Dir))["im.empty"] != 1 {
		t.Fatal("an empty poll is recorded as such")
	}
}

func TestSecretsNeverReachTheTrace(t *testing.T) {
	root := t.TempDir()
	tel := New(Config{Root: root, PageWS: func() string { return "" }})
	tel.Begin(context.Background(), "t", nil)
	defer tel.Stop()
	tel.Event("browser", "call", Obj{"args": Obj{"key": "super-secret-stream-key", "url": "rtmp://h.invalid/live/abc?sign=zzzz"}})
	raw, _ := os.ReadFile(filepath.Join(tel.trace().Dir, "events.jsonl"))
	if strings.Contains(string(raw), "super-secret-stream-key") || strings.Contains(string(raw), "zzzz") || strings.Contains(string(raw), "live/abc") {
		t.Fatalf("a secret leaked: %s", raw)
	}
}

func mustHex(s string) []byte {
	out := make([]byte, len(s)/2)
	for i := range out {
		var b byte
		for _, c := range s[2*i : 2*i+2] {
			b <<= 4
			switch {
			case c >= '0' && c <= '9':
				b |= byte(c - '0')
			default:
				b |= byte(c-'a') + 10
			}
		}
		out[i] = b
	}
	return out
}
