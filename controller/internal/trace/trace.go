// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// Package trace writes a private, redacted JSONL trace of one broadcast test (port of src/trace.py).
//
// It lives under runtime/traces (git-ignored). Credentials, stream keys, cookies and long opaque tokens are never
// written; raw IM frames are kept as separate local blobs.
package trace

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var (
	secretName = regexp.MustCompile(`(?i)^(key|authentication|ttwid|cookie|set-cookie|authorization)$|token|secret|passw|` +
		`sessionid|^sid_|sign|bogus|device_id|install_id|odin|stream_key|csrf|ticket|verifyfp`)
	urlRE   = regexp.MustCompile(`(?:rtmps?|https?|wss?)://[^\s"'\\<>]+`)
	emailRE = regexp.MustCompile(`[\p{L}\p{N}_.+-]+@[\p{L}\p{N}_-]+\.[\p{L}\p{N}_.-]+`)
	tokenRE = regexp.MustCompile(`[A-Za-z0-9_+/=-]{40,}`)
)

const (
	maxText      = 4000
	maxBlobTotal = 256 * 1024 * 1024
)

// quotePlus is a query-string escape that keeps '[' and ']' and turns spaces into '+'.
func quotePlus(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '.', c == '-', c == '~', c == '[', c == ']':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func unquotePlus(s string) string {
	if v, err := url.QueryUnescape(s); err == nil {
		return v
	}
	return s
}

// RedactURL keeps scheme, host:port and path, masks the RTMP stream key and any secret or long query value.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[url]"
	}
	host := strings.ToLower(u.Hostname())
	if p := u.Port(); p != "" && p != "0" {
		host += ":" + p
	}
	path := u.Path
	if strings.HasPrefix(u.Scheme, "rtmp") {
		if cut := strings.LastIndex(path, "/"); cut >= 0 && path[cut+1:] != "" {
			path = path[:cut] + "/[stream-key]"
		}
	}
	var pairs []string
	for _, part := range strings.Split(u.RawQuery, "&") {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		k, v = unquotePlus(k), unquotePlus(v)
		if secretName.MatchString(k) || utf8.RuneCountInString(v) > 40 {
			v = fmt.Sprintf("[%d]", utf8.RuneCountInString(v))
		}
		pairs = append(pairs, quotePlus(k)+"="+quotePlus(v))
	}
	out := u.Scheme + "://" + host + path
	if len(pairs) > 0 {
		out += "?" + strings.Join(pairs, "&")
	}
	return out
}

// RedactText masks URLs' secrets, e-mails and long opaque tokens, and truncates very long text.
func RedactText(text string) string {
	text = urlRE.ReplaceAllStringFunc(text, RedactURL)
	text = emailRE.ReplaceAllString(text, "[email]")
	text = tokenRE.ReplaceAllStringFunc(text, func(m string) string { return fmt.Sprintf("[tok:%d]", len(m)) })
	if n := utf8.RuneCountInString(text); n > maxText {
		text = string([]rune(text)[:maxText]) + fmt.Sprintf("…[+%d]", n-maxText)
	}
	return text
}

// Redact returns a copy of any JSON-like value that is safe to write to a trace.
func Redact(value any) any { return redact(value, 0) }

func isScalarSecret(v any) bool {
	switch v.(type) {
	case string, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number:
		return true
	}
	return false
}

func redact(value any, depth int) any {
	if depth > 14 {
		return "[deep]"
	}
	switch v := value.(type) {
	case nil:
		return nil
	case string:
		return RedactText(v)
	case []byte:
		return fmt.Sprintf("[bytes:%d]", len(v))
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if secretName.MatchString(key) && isScalarSecret(item) {
				out[key] = "[redacted]"
			} else {
				out[key] = redact(item, depth+1)
			}
		}
		return out
	case []any:
		n := len(v)
		if n > 200 {
			n = 200
		}
		items := make([]any, 0, n+1)
		for _, item := range v[:n] {
			items = append(items, redact(item, depth+1))
		}
		if len(v) > 200 {
			items = append(items, fmt.Sprintf("[+%d items]", len(v)-200))
		}
		return items
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number:
		return v
	}
	return RedactText(fmt.Sprint(value))
}

// Flatten gives a dotted-path view of a JSON document, used to report only what changed.
func Flatten(value any, prefix string) map[string]any {
	out := map[string]any{}
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			for k, x := range Flatten(item, path) {
				out[k] = x
			}
		}
	case []any:
		for i, item := range v {
			if i >= 50 {
				break
			}
			for k, x := range Flatten(item, fmt.Sprintf("%s[%d]", prefix, i)) {
				out[k] = x
			}
		}
	default:
		out[prefix] = value
	}
	return out
}

type Change struct {
	Path     string `json:"path"`
	Old, New any
}

func (c Change) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{"path": c.Path, "old": c.Old, "new": c.New})
}

// Changes lists the paths whose value differs, sorted by path.
func Changes(old, new map[string]any) []Change {
	paths := map[string]bool{}
	for p := range old {
		paths[p] = true
	}
	for p := range new {
		paths[p] = true
	}
	var sorted []string
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	var out []Change
	for _, p := range sorted {
		if fmt.Sprint(old[p]) != fmt.Sprint(new[p]) || (old[p] == nil) != (new[p] == nil) {
			out = append(out, Change{Path: p, Old: old[p], New: new[p]})
		}
	}
	return out
}

// Trace is one broadcast's trace folder. Safe for concurrent use.
type Trace struct {
	Dir     string
	Started string
	mu      sync.Mutex
	file    *os.File
	t0      time.Time
	counts  map[string]int
	blobs   int
	bytes   int
	closed  bool
}

func New(root, label string) (*Trace, error) {
	if label == "" {
		label = "live"
	}
	dir := filepath.Join(root, "runtime", "traces", time.Now().Format("20060102-150405")+"-"+label)
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &Trace{Dir: dir, Started: time.Now().UTC().Format(time.RFC3339Nano), file: f, t0: time.Now(), counts: map[string]int{}}, nil
}

// Event appends one redacted record. It never returns an error: tracing must not disturb the broadcast.
func (t *Trace) Event(source, kind string, data map[string]any) {
	record := map[string]any{
		"t":   float64(time.Since(t.t0).Milliseconds()) / 1000,
		"ts":  time.Now().UTC().Format("2006-01-02T15:04:05.000") + "+00:00",
		"src": source, "kind": kind, "data": Redact(data),
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(record) != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.counts[source+"."+kind]++
	t.file.Write(buf.Bytes())
}

var blobName = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// Blob stores raw bytes next to the trace and returns its relative name ("" when closed or over budget).
func (t *Trace) Blob(name string, data []byte) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.bytes+len(data) > maxBlobTotal {
		return ""
	}
	t.bytes += len(data)
	t.blobs++
	relative := fmt.Sprintf("blobs/%05d-%s.bin", t.blobs, blobName.ReplaceAllString(name, "_"))
	if os.WriteFile(filepath.Join(t.Dir, relative), data, 0o644) != nil {
		return ""
	}
	return relative
}

func (t *Trace) Total() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, c := range t.counts {
		n += c
	}
	return n
}

func (t *Trace) Closed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

func (t *Trace) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.closed = true
		t.file.Close()
	}
}
