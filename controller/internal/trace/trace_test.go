// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package trace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeysTokensAndStreamNamesNeverSurvive(t *testing.T) {
	data := map[string]any{
		"key": "abc", "msToken": strings.Repeat("x", 10), "nested": map[string]any{"password": "p", "ok": 7},
		"url":  "rtmp://host.invalid/live/secret-stream-name?expire=1&sign=" + strings.Repeat("a", 50),
		"pull": "see https://h.invalid/a/b?token=zzz&x=1 and mail me@example.com",
		"blob": strings.Repeat("A", 80), "raw": []byte("123"),
	}
	raw, _ := json.Marshal(Redact(data))
	text := string(raw)
	for _, forbidden := range []string{"secret-stream-name", "zzz", "me@example.com", strings.Repeat("A", 40), `"abc"`, "aaaaaaaaaa"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("%q survived: %s", forbidden, text)
		}
	}
	if !strings.Contains(text, "host.invalid") || !strings.Contains(text, `"ok":7`) {
		t.Fatalf("lost harmless data: %s", text)
	}
}

func TestChangesReportOnlyDifferences(t *testing.T) {
	old := Flatten(map[string]any{"a": map[string]any{"b": 1, "c": []any{1, 2}}}, "")
	new := Flatten(map[string]any{"a": map[string]any{"b": 2, "c": []any{1, 2}}}, "")
	got := Changes(old, new)
	if len(got) != 1 || got[0].Path != "a.b" || got[0].Old != 1 || got[0].New != 2 {
		t.Fatalf("%+v", got)
	}
}

func TestListsAreCappedAndDepthIsBounded(t *testing.T) {
	items := make([]any, 250)
	out := Redact(items).([]any)
	if len(out) != 201 || out[200] != "[+50 items]" {
		t.Fatalf("len=%d last=%v", len(out), out[len(out)-1])
	}
	var deep any = 1
	for i := 0; i < 20; i++ {
		deep = map[string]any{"a": deep}
	}
	if !strings.Contains(string(mustJSON(t, Redact(deep))), "[deep]") {
		t.Fatal("deep nesting must be cut")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestTraceCountsAndBlobs(t *testing.T) {
	root := t.TempDir()
	tr, err := New(root, "")
	if err != nil {
		t.Fatal(err)
	}
	tr.Event("x", "y", map[string]any{"a": 1, "password": "hunter2"})
	if name := tr.Blob("frame/x", []byte{1, 2}); name != "blobs/00001-frame_x.bin" {
		t.Fatalf("blob name %q", name)
	}
	tr.Close()
	tr.Event("x", "y", map[string]any{"late": true}) // ignored once closed
	if tr.Blob("late", []byte{1}) != "" {
		t.Fatal("a closed trace must not accept blobs")
	}
	if tr.Total() != 1 {
		t.Fatalf("total %d", tr.Total())
	}
	raw, _ := os.ReadFile(filepath.Join(tr.Dir, "events.jsonl"))
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 || strings.Contains(string(raw), "hunter2") || !strings.Contains(lines[0], `"src":"x"`) {
		t.Fatalf("%q", raw)
	}
	var record map[string]any
	if json.Unmarshal([]byte(lines[0]), &record) != nil || record["ts"] == nil || !strings.HasSuffix(record["ts"].(string), "+00:00") {
		t.Fatalf("bad record %q", lines[0])
	}
}

func TestRedactURLMasksStreamKeyAndLongOrSecretValues(t *testing.T) {
	got := RedactURL("rtmps://push.invalid:1935/live/abc123?k=v&key=SECRETVALUE&n=" + strings.Repeat("z", 41))
	want := "rtmps://push.invalid:1935/live/[stream-key]?k=v&key=[11]&n=[41]"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}
