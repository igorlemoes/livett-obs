// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// Package telemetry is passive, best-effort signal capture for a broadcast.
//
// Nothing here can start, stop or alter a broadcast, and no failure here may reach the controller: every collector
// runs in its own guarded goroutine. IM messages are only recorded, never dispatched to the guard. The trace format
// (events.jsonl + blobs/) is summarised by BuildSummary.
package telemetry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"tiktoklive/internal/controller"
	"tiktoklive/internal/impoll"
	"tiktoklive/internal/trace"
	"tiktoklive/internal/wire"
	"tiktoklive/internal/ws"
)

type Obj = map[string]any

// ObsCaller is the part of an OBS connection the collector needs.
type ObsCaller interface {
	Call(ctx context.Context, requestType string, data map[string]any) (map[string]any, error)
}

var keptRequestHeaders = set("content-type", "sdk_aid", "x-ss-stub", "accept", "origin", "referer")
var keptResponseHeaders = set("content-type", "content-length", "server", "date", "via", "cache-control", "x-tt-logid",
	"x-tt-trace-id", "x-tt-trace-host", "x-tt-trace-tag", "access-control-allow-origin", "access-control-allow-headers")

func set(items ...string) map[string]bool {
	m := map[string]bool{}
	for _, i := range items {
		m[i] = true
	}
	return m
}

// Config wires the collectors to the running app.
type Config struct {
	Root string
	Tail time.Duration // keep listening this long after the end
	// PageWS returns the debugging websocket of the service tab ("" when unknown).
	PageWS func() string
	// ObsOpen opens a short connection to OBS through the plugin's control API (nil disables the OBS collector).
	ObsOpen func(ctx context.Context) (ObsCaller, error)
	// Browser runs panel operations (probe / imfetch); it is the untraced bridge.
	Browser func(ctx context.Context, method string, data any) (any, error)
}

type Telemetry struct {
	cfg Config

	mu      sync.Mutex
	tr      *trace.Trace
	cancel  context.CancelFunc
	runCtx  context.Context // shared by every collector of the current trace
	wg      sync.WaitGroup
	stopper bool
	stats   struct{ imPolls, imErrors, imMessages int }
}

func New(cfg Config) *Telemetry {
	if cfg.Tail == 0 {
		cfg.Tail = 10 * time.Second
	}
	return &Telemetry{cfg: cfg}
}

var _ controller.Recorder = (*Telemetry)(nil)

func (t *Telemetry) trace() *trace.Trace {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.tr
}

func (t *Telemetry) active() bool {
	tr := t.trace()
	return tr != nil && !tr.Closed()
}

func (t *Telemetry) Event(source, kind string, data Obj) {
	if tr := t.trace(); tr != nil && !tr.Closed() {
		tr.Event(source, kind, data)
	}
}

func (t *Telemetry) Snapshot() Obj {
	tr := t.trace()
	if tr == nil {
		return Obj{"active": false}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return Obj{"active": !tr.Closed(), "events": tr.Total(), "dir": lastElem(tr.Dir),
		"imPolls": t.stats.imPolls, "imMessages": t.stats.imMessages}
}

func lastElem(path string) string {
	path = strings.ReplaceAll(path, "\\", "/")
	return path[strings.LastIndex(path, "/")+1:]
}

// spawn runs a collector guarded: a failure becomes a trace event, never an error for the controller.
func (t *Telemetry) spawn(ctx context.Context, name string, fn func(context.Context) error) {
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				t.Event("monitor", "collector_error", Obj{"collector": name, "error": "panic", "detail": fmt.Sprint(r)})
			}
		}()
		if err := fn(ctx); err != nil && ctx.Err() == nil {
			t.Event("monitor", "collector_error", Obj{"collector": name, "error": fmt.Sprintf("%T", err), "detail": truncate(err.Error(), 200)})
		}
	}()
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// Begin opens the trace and starts the broadcast-wide collectors. Monitoring must never block a broadcast start.
func (t *Telemetry) Begin(ctx context.Context, title string, obsInfo controller.Obj) error {
	if t.active() {
		return nil
	}
	tr, err := trace.New(t.cfg.Root, "")
	if err != nil {
		return nil
	}
	runCtx, cancel := context.WithCancel(context.Background())
	t.mu.Lock()
	t.tr, t.cancel, t.runCtx, t.stopper = tr, cancel, runCtx, false
	t.stats = struct{ imPolls, imErrors, imMessages int }{}
	t.mu.Unlock()
	t.Event("monitor", "begin", Obj{"started": tr.Started, "title": title})
	t.spawn(runCtx, "cdp", t.cdpLoop)
	if t.cfg.ObsOpen != nil {
		t.spawn(runCtx, "obs", t.obsLoop)
	}
	t.spawn(runCtx, "process", t.processLoop)
	t.spawn(runCtx, "targets", t.targetsLoop)
	return nil
}

func (t *Telemetry) AttachRoom(ctx context.Context, binding controller.Binding, observeIM bool) {
	if !t.active() {
		return
	}
	t.Event("monitor", "room_attached", Obj{"room_id": binding.RoomID, "stream_id": binding.StreamID})
	runCtx := t.collectorCtx()
	if runCtx == nil {
		return
	}
	t.spawn(runCtx, "room", func(c context.Context) error { return t.roomLoop(c, binding) })
	if observeIM {
		t.spawn(runCtx, "im", func(c context.Context) error { return t.imLoop(c, binding) })
	}
}

func (t *Telemetry) collectorCtx() context.Context {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.runCtx
}

// Finish keeps listening for Tail after the end, then closes the trace.
func (t *Telemetry) Finish() {
	t.mu.Lock()
	if t.tr == nil || t.tr.Closed() || t.stopper {
		t.mu.Unlock()
		return
	}
	t.stopper = true
	tail := t.cfg.Tail
	t.mu.Unlock()
	t.Event("monitor", "finish_scheduled", Obj{"tail": tail.Seconds()})
	go func() {
		time.Sleep(tail)
		t.Stop()
	}()
}

// Stop cancels the collectors, writes the final event and the summary, and closes the trace.
func (t *Telemetry) Stop() {
	t.mu.Lock()
	tr, cancel := t.tr, t.cancel
	t.cancel = nil
	t.mu.Unlock()
	if tr == nil || tr.Closed() {
		return
	}
	if cancel != nil {
		cancel()
	}
	t.wg.Wait()
	t.mu.Lock()
	stats := Obj{"im_polls": t.stats.imPolls, "im_errors": t.stats.imErrors, "im_messages": t.stats.imMessages}
	t.mu.Unlock()
	t.Event("monitor", "end", Obj{"stats": stats})
	tr.Close()
	WriteSummary(tr.Dir)
}

// ---- Chrome / CDP

type cdpConn struct {
	c       *ws.Conn
	mu      sync.Mutex
	next    int
	pending map[int]chan Obj
	onEvent func(method string, params Obj)
}

func (d *cdpConn) send(method string, params Obj, timeout time.Duration) (Obj, error) {
	d.mu.Lock()
	d.next++
	id := d.next
	ch := make(chan Obj, 1)
	d.pending[id] = ch
	d.mu.Unlock()
	raw, _ := json.Marshal(Obj{"id": id, "method": method, "params": params})
	if err := d.c.Write(ws.OpText, raw); err != nil {
		return nil, err
	}
	select {
	case reply := <-ch:
		return reply, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("timeout")
	}
}

func (d *cdpConn) readLoop() error {
	for {
		_, raw, err := d.c.Read()
		if err != nil {
			return err
		}
		var msg Obj
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.UseNumber()
		if dec.Decode(&msg) != nil {
			continue
		}
		if idv, ok := msg["id"].(json.Number); ok {
			id, _ := idv.Int64()
			d.mu.Lock()
			ch := d.pending[int(id)]
			delete(d.pending, int(id))
			d.mu.Unlock()
			if ch != nil {
				ch <- msg
			}
			continue
		}
		method, _ := msg["method"].(string)
		params, _ := msg["params"].(map[string]any)
		d.onEvent(method, params)
	}
}

func (t *Telemetry) cdpLoop(ctx context.Context) error {
	known := map[string]string{}
	var kmu sync.Mutex
	for ctx.Err() == nil {
		page := ""
		if t.cfg.PageWS != nil {
			page = t.cfg.PageWS()
		}
		if page == "" {
			t.Event("cdp", "unavailable", Obj{})
			return nil
		}
		dialCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		conn, err := ws.Dial(dialCtx, page)
		cancel()
		if err != nil {
			t.Event("cdp", "error", Obj{"error": "dial"})
		} else {
			d := &cdpConn{c: conn, pending: map[int]chan Obj{}}
			d.onEvent = func(method string, params Obj) {
				kmu.Lock()
				defer kmu.Unlock()
				t.cdpEvent(ctx, d, method, params, known)
			}
			go func() { <-ctx.Done(); conn.Close() }()
			go d.send("Network.enable", Obj{"maxPostDataSize": 65536}, 10*time.Second)
			for _, domain := range []string{"Runtime", "Log", "Page"} {
				go d.send(domain+".enable", Obj{}, 10*time.Second)
			}
			t.Event("cdp", "attached", Obj{})
			d.readLoop()
			conn.Close()
			t.Event("cdp", "detached", Obj{})
		}
		select {
		case <-time.After(3 * time.Second):
		case <-ctx.Done():
		}
	}
	return nil
}

func headerView(headers any, keep map[string]bool) Obj {
	out := Obj{}
	var other []any
	for name, value := range asObj(headers) {
		low := strings.ToLower(name)
		switch {
		case low == "cookie":
			var names []any
			for _, part := range strings.Split(fmt.Sprint(value), ";") {
				names = append(names, strings.TrimSpace(strings.SplitN(part, "=", 2)[0]))
			}
			out[low] = Obj{"names": names}
		case low == "set-cookie":
			var names []any
			for _, line := range strings.Split(fmt.Sprint(value), "\n") {
				if strings.TrimSpace(line) != "" {
					names = append(names, strings.TrimSpace(strings.SplitN(line, "=", 2)[0]))
				}
			}
			out[low] = Obj{"names": names}
		case keep[low]:
			out[low] = value
		default:
			other = append(other, low)
		}
	}
	if other != nil {
		sort.Slice(other, func(i, j int) bool { return other[i].(string) < other[j].(string) })
		out["_other"] = other
	}
	return out
}

func asObj(v any) Obj {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return Obj{}
}

func str(m Obj, k string) string { s, _ := m[k].(string); return s }

func (t *Telemetry) cdpEvent(ctx context.Context, d *cdpConn, method string, params Obj, known map[string]string) {
	defer func() {
		if r := recover(); r != nil {
			t.Event("cdp", "handler_error", Obj{"method": method, "error": "panic"})
		}
	}()
	id := str(params, "requestId")
	switch method {
	case "Network.requestWillBeSent":
		request := asObj(params["request"])
		known[id] = str(request, "url")
		if len(known) > 2000 {
			for k := range known {
				delete(known, k)
				break
			}
		}
		var post any
		if body := str(request, "postData"); body != "" {
			form := Obj{}
			if values, err := url.ParseQuery(body); err == nil {
				for k, v := range values {
					form[k] = v[len(v)-1]
				}
			}
			post = form
		}
		t.Event("cdp", "request", Obj{"id": id, "method": request["method"], "url": trace.RedactURL(str(request, "url")),
			"type": params["type"], "initiator": asObj(params["initiator"])["type"],
			"headers": headerView(request["headers"], keptRequestHeaders), "post": post})
	case "Network.responseReceived":
		response := asObj(params["response"])
		t.Event("cdp", "response", Obj{"id": id, "url": trace.RedactURL(str(response, "url")), "status": response["status"],
			"mime": response["mimeType"], "protocol": response["protocol"], "remote_ip": response["remoteIPAddress"],
			"headers": headerView(response["headers"], keptResponseHeaders)})
	case "Network.loadingFinished":
		raw := known[id]
		path := ""
		if u, err := url.Parse(raw); err == nil {
			path = u.Path
		}
		t.Event("cdp", "finished", Obj{"id": id, "bytes": params["encodedDataLength"]})
		if strings.HasPrefix(path, "/webcast/") && path != "/webcast/im/fetch/" {
			go t.cdpBody(d, id, path)
		}
	case "Network.loadingFailed":
		t.Event("cdp", "failed", Obj{"id": id, "error": params["errorText"], "canceled": params["canceled"],
			"blocked": params["blockedReason"], "cors": params["corsErrorStatus"], "url": trace.RedactURL(known[id])})
	case "Network.webSocketCreated":
		t.Event("cdp", "ws_created", Obj{"id": id, "url": trace.RedactURL(str(params, "url"))})
	case "Network.webSocketHandshakeResponseReceived":
		response := asObj(params["response"])
		t.Event("cdp", "ws_handshake", Obj{"id": id, "status": response["status"],
			"headers": headerView(response["headers"], keptResponseHeaders)})
	case "Network.webSocketFrameReceived", "Network.webSocketFrameSent":
		frame := asObj(params["response"])
		data := str(frame, "payloadData")
		entry := Obj{"id": id, "dir": "out", "opcode": frame["opcode"], "len": utf8.RuneCountInString(data)}
		if strings.HasSuffix(method, "Received") {
			entry["dir"] = "in"
		}
		if n, _ := frame["opcode"].(json.Number); n.String() == "2" {
			if raw, err := base64.StdEncoding.DecodeString(data); err == nil {
				entry["blob"] = t.blob("ws-frame", raw)
			}
		} else {
			entry["text"] = truncate(data, 2000)
		}
		t.Event("cdp", "ws_frame", entry)
	case "Network.webSocketClosed", "Network.webSocketFrameError":
		kind := "ws_" + strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(method, "Network.webSocket"), ""))
		t.Event("cdp", kind, Obj{"id": id, "error": params["errorMessage"]})
	case "Network.eventSourceMessageReceived":
		t.Event("cdp", "sse", Obj{"id": id, "name": params["eventName"], "data": truncate(str(params, "data"), 2000)})
	case "Runtime.consoleAPICalled":
		var parts []string
		for _, a := range params["args"].([]any) {
			am := asObj(a)
			if v, ok := am["value"]; ok {
				parts = append(parts, fmt.Sprint(v))
			} else {
				parts = append(parts, fmt.Sprint(am["description"]))
			}
		}
		t.Event("cdp", "console", Obj{"level": params["type"], "text": truncate(strings.Join(parts, " "), 1000)})
	case "Runtime.exceptionThrown":
		details := asObj(params["exceptionDetails"])
		t.Event("cdp", "exception", Obj{"text": details["text"],
			"description": truncate(str(asObj(details["exception"]), "description"), 500)})
	case "Log.entryAdded":
		entry := asObj(params["entry"])
		t.Event("cdp", "log", Obj{"level": entry["level"], "source": entry["source"], "text": truncate(str(entry, "text"), 500), "url": entry["url"]})
	case "Page.frameNavigated":
		t.Event("cdp", "navigated", Obj{"url": asObj(params["frame"])["url"]})
	}
}

func (t *Telemetry) blob(name string, data []byte) any {
	if tr := t.trace(); tr != nil {
		if rel := tr.Blob(name, data); rel != "" {
			return rel
		}
	}
	return nil
}

func (t *Telemetry) cdpBody(d *cdpConn, id, path string) {
	reply, err := d.send("Network.getResponseBody", Obj{"requestId": id}, 10*time.Second)
	if err != nil {
		t.Event("cdp", "body_error", Obj{"id": id, "path": path, "error": "error"})
		return
	}
	result := asObj(reply["result"])
	body := str(result, "body")
	if result["base64Encoded"] == true {
		if raw, err := base64.StdEncoding.DecodeString(body); err == nil {
			body = strings.ToValidUTF8(string(raw), "�")
		}
	}
	if len(body) > 1_000_000 {
		t.Event("cdp", "body", Obj{"id": id, "path": path, "truncated": len(body)})
		return
	}
	var parsed any
	dec := json.NewDecoder(strings.NewReader(body))
	dec.UseNumber()
	if dec.Decode(&parsed) != nil {
		parsed = truncate(body, 2000)
	}
	t.Event("cdp", "body", Obj{"id": id, "path": path, "body": parsed})
}

func (t *Telemetry) targetsLoop(ctx context.Context) error {
	page := ""
	if t.cfg.PageWS != nil {
		page = t.cfg.PageWS()
	}
	u, err := url.Parse(page)
	if err != nil || u.Port() == "" {
		return nil
	}
	for ctx.Err() == nil {
		var targets []Obj
		if err := getJSON(ctx, "http://127.0.0.1:"+u.Port()+"/json/list", &targets); err != nil {
			t.Event("chrome", "targets_error", Obj{"error": "error"})
		} else {
			list := []any{}
			for _, tg := range targets {
				list = append(list, Obj{"type": tg["type"], "url": trace.RedactURL(str(tg, "url"))})
			}
			t.Event("chrome", "targets", Obj{"targets": list})
		}
		select {
		case <-time.After(15 * time.Second):
		case <-ctx.Done():
		}
	}
	return nil
}

// ---- OBS

func (t *Telemetry) obsLoop(ctx context.Context) error {
	client, err := t.cfg.ObsOpen(ctx)
	if err != nil {
		return err
	}
	if closer, ok := client.(interface{ Close() }); ok {
		defer closer.Close()
	}
	request := func(name string, data Obj) (Obj, error) {
		if data == nil {
			data = Obj{}
		}
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		result, err := client.Call(callCtx, name, data)
		if err != nil {
			return nil, fmt.Errorf("%s:%v", name, err)
		}
		return result, nil
	}
	// NOT GetOutputList: it crashes OBS 32.2.2 (obs_output_get_width).
	for _, name := range []string{"GetVersion", "GetVideoSettings", "GetInputList", "GetStreamServiceSettings", "GetCurrentProgramScene"} {
		result, err := request(name, nil)
		if err != nil {
			t.Event("obs", "snapshot_error", Obj{"name": name, "error": truncate(err.Error(), 100)})
			continue
		}
		t.Event("obs", "snapshot", Obj{"name": name, "result": result})
		if name == "GetInputList" {
			inputs, _ := result["inputs"].([]any)
			for _, it := range inputs {
				input := str(asObj(it), "inputName")
				for _, extra := range []string{"GetInputSettings", "GetInputAudioTracks", "GetInputMute"} {
					if r, err := request(extra, Obj{"inputName": input}); err == nil {
						t.Event("obs", "snapshot", Obj{"name": extra, "input": input, "result": r})
					}
				}
			}
		}
	}
	var prevBytes float64
	var prevAt time.Time
	havePrev := false
	lastScene, lastActive := "", false
	for ctx.Err() == nil {
		stats, err := request("GetStats", nil)
		if err != nil {
			return err
		}
		status, err := request("GetStreamStatus", nil)
		if err != nil {
			return err
		}
		now := time.Now()
		var kbps any
		if b, ok := status["outputBytes"].(float64); ok && havePrev {
			kbps = math.Round((b-prevBytes)*8/1000/math.Max(now.Sub(prevAt).Seconds(), 1e-3)*10) / 10
		}
		if b, ok := status["outputBytes"].(float64); ok {
			prevBytes = b
		}
		prevAt, havePrev = now, true
		t.Event("obs", "stats", Obj{"stats": stats, "stream": status, "kbps": kbps})
		// there is no event feed: scene and stream state changes are noticed by comparing samples
		if scene, err := request("GetCurrentProgramScene", nil); err == nil {
			if name := str(scene, "currentProgramSceneName"); name != lastScene {
				t.Event("obs", "event", Obj{"type": "CurrentProgramSceneChanged", "data": Obj{"sceneName": name}})
				lastScene = name
			}
		}
		if active, _ := status["outputActive"].(bool); active != lastActive {
			t.Event("obs", "event", Obj{"type": "StreamStateChanged", "data": Obj{"outputActive": active}})
			lastActive = active
		}
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
		}
	}
	return nil
}

// processLoop samples the OBS process (CPU, memory, TCP connections) every 10 s.
func (t *Telemetry) processLoop(ctx context.Context) error {
	script := `$p=Get-Process obs64 -ErrorAction Stop;` +
		`[pscustomobject]@{cpu=$p.CPU;ws=$p.WorkingSet64;threads=$p.Threads.Count;` +
		`conns=@(Get-NetTCPConnection -OwningProcess $p.Id -ErrorAction SilentlyContinue|` +
		`Where-Object State -ne Listen|Select-Object State,RemoteAddress,RemotePort)}|ConvertTo-Json -Compress -Depth 4`
	failures := 0
	for failures < 3 && ctx.Err() == nil {
		runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		cmd := exec.CommandContext(runCtx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
		out, err := cmd.Output()
		cancel()
		var sample Obj
		if err == nil && json.Unmarshal(out, &sample) == nil {
			t.Event("obs_process", "sample", sample)
			failures = 0
		} else if ctx.Err() == nil {
			failures++
			t.Event("obs_process", "error", Obj{"error": "error"})
		}
		select {
		case <-time.After(10 * time.Second):
		case <-ctx.Done():
		}
	}
	return nil
}

// ---- room state

func (t *Telemetry) roomLoop(ctx context.Context, b controller.Binding) error {
	var last map[string]any
	var lastFull time.Time
	for ctx.Err() == nil {
		result, err := t.cfg.Browser(ctx, "probe", Obj{"room_id": b.RoomID, "ping_room_id": b.PingRoomID, "stream_id": b.StreamID})
		if err != nil {
			t.Event("room", "probe_error", Obj{"error": "error"})
		} else {
			flat := trace.Flatten(result, "")
			if last == nil || time.Since(lastFull) >= 60*time.Second {
				t.Event("room", "full", asObj(result))
				lastFull = time.Now()
			} else if delta := trace.Changes(last, flat); len(delta) > 0 {
				items := make([]any, 0, len(delta))
				for _, c := range delta {
					items = append(items, Obj{"path": c.Path, "old": c.Old, "new": c.New})
				}
				t.Event("room", "changes", Obj{"changes": items})
			}
			last = flat
		}
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
		}
	}
	return nil
}

// ---- IM (observe only)

// describe is a structural dump of unknown protobuf bytes; nil if they do not parse cleanly.
func describe(data []byte, depth int) []any {
	parsed, err := wire.ParseFields(data)
	if err != nil {
		return nil
	}
	numbers := make([]uint64, 0, len(parsed))
	for n := range parsed {
		numbers = append(numbers, n)
	}
	sort.Slice(numbers, func(i, j int) bool { return numbers[i] < numbers[j] })
	out := []any{}
	for _, number := range numbers {
		items := parsed[number]
		if len(items) > 20 {
			items = items[:20]
		}
		for _, f := range items {
			switch f.Wire {
			case 0:
				out = append(out, Obj{"f": number, "v": f.Num})
			case 1, 5:
				out = append(out, Obj{"f": number, "w": f.Wire, "hex": fmt.Sprintf("%x", f.Data)})
			default:
				entry := Obj{"f": number, "len": len(f.Data)}
				if utf8.Valid(f.Data) && printable(string(f.Data)) && len(f.Data) > 0 {
					entry["s"] = truncate(string(f.Data), 200)
				} else if sub := describe(f.Data, depth-1); depth > 0 && sub != nil {
					entry["sub"] = sub
				} else {
					n := 16
					if len(f.Data) < n {
						n = len(f.Data)
					}
					entry["hex"] = fmt.Sprintf("%x", f.Data[:n])
				}
				out = append(out, entry)
			}
		}
	}
	return out
}

func printable(s string) bool {
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return s != ""
}

func (t *Telemetry) IMBatch(raw []byte, response *wire.Response, side controller.Obj, seen map[string]bool) {
	t.mu.Lock()
	t.stats.imPolls++
	polls := t.stats.imPolls
	t.mu.Unlock()
	entry := Obj{"http": side, "size": len(raw)}
	if response == nil {
		t.Event("im", "empty", entry)
		return
	}
	var blob any
	if t.active() && (len(response.Messages) > 0 || polls%10 == 0) {
		blob = t.blob("im-fetch", raw)
	}
	batch := Obj{"http": side, "size": len(raw), "blob": blob, "messages": len(response.Messages), "cursor_len": len(response.Cursor),
		"fetch_type": response.FetchType, "interval_ms": response.FetchInterval, "need_ack": response.NeedAck,
		"push_server": response.PushServer, "top": describe(raw, 1)}
	t.Event("im", "batch", batch)
	for _, m := range response.Messages {
		t.mu.Lock()
		t.stats.imMessages++
		t.mu.Unlock()
		var classified any
		if signal, err := wire.Classify(m, "observe"); err == nil && signal != nil {
			classified = string(signal.Kind)
		}
		key := m.Method + "|" + m.MessageID
		t.Event("im", "message", Obj{"method": m.Method, "id": m.MessageID, "history": m.History, "duplicate": seen[key],
			"size": len(m.Payload), "classified": classified, "structure": describe(m.Payload, 3)})
		seen[key] = true
	}
}

func (t *Telemetry) IMError(err error, side controller.Obj) {
	t.mu.Lock()
	t.stats.imErrors++
	t.mu.Unlock()
	t.Event("im", "error", Obj{"error": fmt.Sprintf("%T", err), "detail": truncate(err.Error(), 200), "http": side})
}

// imLoop is the observe-only poller, used when the automatic shutdown guard is off.
func (t *Telemetry) imLoop(ctx context.Context, b controller.Binding) error {
	side := Obj{}
	fetch := func(ctx context.Context, u string) ([]byte, error) {
		reply, err := t.cfg.Browser(ctx, "imfetch", Obj{"url": u})
		if err != nil {
			return nil, err
		}
		r := asObj(reply)
		side["status"], side["type"], side["size"] = r["status"], r["type"], r["size"]
		return base64.StdEncoding.DecodeString(str(r, "b64"))
	}
	poller, err := impoll.New(observeGuard{room: b.RoomID}, fetch, nil, 0, nil)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	errs := 0
	for ctx.Err() == nil {
		delay := 3 * time.Second
		raw, response, err := poller.ObserveOnce(ctx)
		if err != nil {
			errs++
			t.IMError(err, side)
			delay = 5 * time.Second
			if errs >= 5 {
				delay = 15 * time.Second
			}
		} else {
			errs = 0
			t.IMBatch(raw, response, side, seen)
			if response != nil {
				delay = time.Duration(response.FetchInterval) * time.Millisecond
				if delay > 10*time.Second {
					delay = 10 * time.Second
				}
				if delay < time.Second {
					delay = time.Second
				}
			}
		}
		select {
		case <-time.After(delay):
		case <-ctx.Done():
		}
	}
	return nil
}
