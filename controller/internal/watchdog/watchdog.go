// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// Package watchdog is the independent dead-man switch for a broadcast started by the panel.
//
// It runs as a separate process, so it survives the panel dying. The panel writes runtime/heartbeat.json every
// couple of seconds. If a broadcast is live and the heartbeat goes stale (or the panel process is gone), this
// process stops the OBS output it can positively identify, asks TikTok to end the room and confirms it. It never
// starts anything and never touches an output it cannot tie to the journaled profile.
package watchdog

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	StaleAfter   = 15 * time.Second // a HUNG panel (alive but silent) is declared dead after this long
	OverrunGrace = 20 * time.Second // after a scheduled end, before the watchdog steps in
	Poll         = time.Second      // a DEAD panel process is detected within one poll
)

var (
	idlePhases   = []string{"closed", "idle", "disconnected"}
	activePhases = []string{"live", "ending", "uncertain"}
)

type Obj = map[string]any

// Obs is the slice of the OBS client the watchdog uses.
type Obs interface {
	Call(ctx context.Context, name string, data map[string]any) (map[string]any, error)
	Close()
}

// Browser is the slice of the Chrome bridge the watchdog uses.
type Browser interface {
	Call(ctx context.Context, method string, data any) (any, error)
}

type Watchdog struct {
	Root           string
	ObsFactory     func(ctx context.Context) (Obs, error)
	BrowserFactory func(ctx context.Context) (Browser, error) // (nil, nil) = no browser: never start one from here
	Clock          func() time.Time
	StaleAfter     time.Duration
	Alive          func(pid int) bool
	PollEvery      time.Duration
	RoomAttempts   int           // tries to end and confirm the room
	RoomRetryEvery time.Duration // pause between those tries
}

func New(root string, obsFactory func(context.Context) (Obs, error), browserFactory func(context.Context) (Browser, error)) *Watchdog {
	return &Watchdog{Root: root, ObsFactory: obsFactory, BrowserFactory: browserFactory, Clock: time.Now,
		StaleAfter: StaleAfter, Alive: PidAlive, PollEvery: Poll, RoomAttempts: 6, RoomRetryEvery: 5 * time.Second}
}

func (w *Watchdog) journal() string   { return filepath.Join(w.Root, "runtime", "broadcast.json") }
func (w *Watchdog) heartbeat() string { return filepath.Join(w.Root, "runtime", "heartbeat.json") }
func (w *Watchdog) result() string    { return filepath.Join(w.Root, "runtime", "watchdog.json") }

func read(path string) Obj {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m Obj
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

func write(path string, data Obj) error {
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func num(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

// PidAlive reports whether the process exists (Windows: OpenProcess + WaitForSingleObject, no subprocess).
func PidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	const synchronize = 0x00100000
	handle, err := syscall.OpenProcess(synchronize, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(handle)
	event, err := syscall.WaitForSingleObject(handle, 0)
	if err != nil {
		return true // cannot tell: fall back to the heartbeat age only
	}
	return event == syscall.WAIT_TIMEOUT // still running
}

// IsTikTokDestination is true for a custom RTMP destination that points at TikTok's ingest.
func IsTikTokDestination(service Obj) bool {
	if service == nil || service["streamServiceType"] != "rtmp_custom" {
		return false
	}
	settings, _ := service["streamServiceSettings"].(map[string]any)
	server, _ := settings["server"].(string)
	server = strings.ToLower(server)
	parts := strings.Split(server, "/")
	return strings.HasPrefix(server, "rtmp") && len(parts) > 2 && strings.Contains(parts[2], "tiktok")
}

// IsNativeDestination: the stream was started by the OBS button through the plugin's own service. Its key lives in the
// plugin, never in the OBS settings, so there is nothing to compare and nothing to wipe afterwards.
func IsNativeDestination(service Obj) bool {
	return service != nil && service["streamServiceType"] == "tiktok_live"
}

func (w *Watchdog) PanelAlive() bool {
	beat := read(w.heartbeat())
	if beat == nil {
		return false
	}
	pid, _ := num(beat["pid"])
	if !w.Alive(int(pid)) {
		return false // the panel process is gone: no need to wait for the heartbeat to age
	}
	t, _ := num(beat["t"])
	age := float64(w.Clock().UnixNano())/1e9 - t
	return age <= w.StaleAfter.Seconds()
}

func (w *Watchdog) ShouldAct() bool {
	data := read(w.journal())
	phase, _ := data["phase"].(string)
	if data == nil || !contains(activePhases, phase) || data["binding"] == nil {
		return false
	}
	if endAt, ok := num(data["end_at"]); ok && phase == "live" {
		if float64(w.Clock().UnixNano())/1e9 > endAt+OverrunGrace.Seconds() {
			return true // the scheduled end passed and the panel did not end it
		}
	}
	return !w.PanelAlive()
}

func (w *Watchdog) ShouldExit() bool {
	data := read(w.journal())
	phase, _ := data["phase"].(string)
	return data == nil || contains(idlePhases, phase)
}

func status(m Obj) (active, reconnecting bool) {
	a, _ := m["outputActive"].(bool)
	r, _ := m["outputReconnecting"].(bool)
	return a, r
}

func (w *Watchdog) stopObs(ctx context.Context, data Obj) Obj {
	result := Obj{}
	obs, err := w.ObsFactory(ctx)
	if err != nil {
		result["obs"] = "error"
		return result
	}
	defer obs.Close()
	fail := func() Obj { result["obs"] = "error"; return result }
	st, err := obs.Call(ctx, "GetStreamStatus", nil)
	if err != nil {
		return fail()
	}
	prof, err := obs.Call(ctx, "GetProfileList", nil)
	if err != nil {
		return fail()
	}
	profile, _ := prof["currentProfileName"].(string)
	service, err := obs.Call(ctx, "GetStreamServiceSettings", nil)
	if err != nil {
		return fail()
	}
	ours, _ := data["profile"].(string)
	active, reconnecting := status(st)
	switch {
	case !active && !reconnecting:
		result["obs"] = "already_stopped"
	case profile == ours && (IsTikTokDestination(service) || IsNativeDestination(service)):
		if _, err := obs.Call(ctx, "StopStream", nil); err != nil {
			return fail()
		}
		for i := 0; i < 40; i++ {
			if st, err = obs.Call(ctx, "GetStreamStatus", nil); err != nil {
				return fail()
			}
			if a, r := status(st); !a && !r {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if a, _ := status(st); !a {
			result["obs"] = "stopped"
		} else {
			result["obs"] = "stop_unconfirmed"
		}
	default:
		result["obs"] = "not_ours" // another profile/destination: never touch it
	}
	if result["obs"] == "stopped" || result["obs"] == "already_stopped" {
		current, err := obs.Call(ctx, "GetStreamServiceSettings", nil)
		if err != nil {
			return fail()
		}
		if IsTikTokDestination(current) && profile == ours {
			if _, err := obs.Call(ctx, "SetStreamServiceSettings", Obj{"streamServiceType": "rtmp_custom",
				"streamServiceSettings": Obj{"server": "", "key": "", "use_auth": false}}); err != nil {
				return fail()
			}
			result["key_wiped"] = true
		}
	}
	return result
}

func (w *Watchdog) endRoom(ctx context.Context, data Obj) Obj {
	browser, err := w.BrowserFactory(ctx)
	if err != nil {
		return Obj{"room": "error"}
	}
	if browser == nil {
		return Obj{"room": "browser_unavailable"}
	}
	binding := data["binding"]
	// A single attempt is not enough: the request may be lost or the service slow right when the panel dies.
	result := Obj{"room": "not_confirmed"}
	attempts := w.RoomAttempts
	if attempts < 1 {
		attempts = 1
	}
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(w.RoomRetryEvery):
			case <-ctx.Done():
				return result
			}
		}
		browser.Call(ctx, "ping", Obj{"binding": binding, "status": 4}) // the response may be lost; verification decides
		verified, err := browser.Call(ctx, "verify", binding)
		if err != nil {
			result = Obj{"room": "error"}
			continue
		}
		if m, ok := verified.(map[string]any); ok && m["ended"] == true {
			return Obj{"room": "ended"}
		}
		result = Obj{"room": "not_confirmed"}
	}
	return result
}

// Act stops our OBS output and ends the room, independently and at once, then records the outcome.
func (w *Watchdog) Act(ctx context.Context) Obj {
	started := w.Clock()
	data := read(w.journal())
	var obsPart, roomPart Obj
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); obsPart = w.stopObs(ctx, data) }()
	go func() { defer wg.Done(); roomPart = w.endRoom(ctx, data) }()
	wg.Wait()
	result := Obj{"at": float64(started.UnixNano()) / 1e9, "binding_phase": data["phase"], "obs": nil, "room": nil,
		"seconds": float64(int(w.Clock().Sub(started).Seconds()*10+0.5)) / 10}
	for k, v := range obsPart {
		result[k] = v
	}
	for k, v := range roomPart {
		result[k] = v
	}
	obsOK := result["obs"] == "stopped" || result["obs"] == "already_stopped"
	if obsOK && result["room"] == "ended" {
		data["phase"] = "closed"
		data["key_cleanup_pending"] = false
	} else {
		data["phase"] = "uncertain"
	}
	write(w.journal(), data)
	write(w.result(), result)
	return result
}

// Run polls until the broadcast is over (or the watchdog has acted once).
func (w *Watchdog) Run(ctx context.Context) error {
	for {
		if w.ShouldExit() && (w.PanelAlive() || read(w.heartbeat()) == nil) {
			return nil
		}
		if w.ShouldAct() {
			w.Act(ctx)
			return nil
		}
		select {
		case <-time.After(w.PollEvery):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

var _ = fmt.Sprint
