// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"tiktoklive/internal/guard"
	"tiktoklive/internal/impoll"
	"tiktoklive/internal/wire"
)

// gate adapts the shutdown guard for the poller: warnings are configurable, a remote end is always acted upon.
type gate struct {
	c *Controller
	g *guard.Guard
}

func (g *gate) RoomID() string       { return g.g.RoomID() }
func (g *gate) SessionID() string    { return g.g.SessionID() }
func (g *gate) RestartBlocked() bool { return g.g.RestartBlocked() }

func (g *gate) Handle(ctx context.Context, s guard.Signal) (*guard.Result, error) {
	g.c.emit("guard", "signal", Obj{"signal": string(s.Kind), "evidence": s.EvidenceID})
	g.c.st.Lock()
	autoEnd := g.c.autoEnd
	g.c.st.Unlock()
	if s.Kind == guard.Warning && !autoEnd {
		return nil, nil
	}
	g.c.st.Lock()
	g.c.lastTrigger = string(s.Kind)
	g.c.st.Unlock()
	return g.g.Handle(ctx, s)
}

// startGuard listens to the host's IM channel; a remote end/restriction signal ends the LIVE at once.
func (c *Controller) startGuard() {
	c.st.Lock()
	binding, output := c.binding, c.output
	c.st.Unlock()
	if !c.imGuard || binding == nil || output == nil {
		return
	}
	b := *binding
	g, err := guard.New(guard.Config{
		RoomID: b.RoomID, SessionID: "local", OperationTimeout: 8 * time.Second,
		StopMedia: func(ctx context.Context) (bool, error) {
			if c.getPhase() != Live {
				return true, nil
			}
			return output.Stop(ctx)
		},
		RequestRoomEnd: func(ctx context.Context) error {
			if c.getPhase() != Live {
				return nil
			}
			_, err := c.browser.Call(ctx, "ping", Obj{"binding": b.obj(), "status": 4})
			return err
		},
		VerifyRoomEnded: func(ctx context.Context) (bool, error) {
			v, err := c.browser.Call(ctx, "verify", b.obj())
			return err == nil && isTrue(asObj(v)["ended"]), err
		},
	})
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.st.Lock()
	c.guard = g
	c.imCancel = cancel
	c.imRunning = true
	c.st.Unlock()
	c.imGen++
	gen := c.imGen
	go c.imLoop(ctx, &gate{c: c, g: g}, gen)
}

func (c *Controller) stopIM() {
	c.st.Lock()
	cancel := c.imCancel
	c.imCancel = nil
	c.imRunning = false
	statsCancel := c.statsCancel
	c.statsCancel = nil
	c.st.Unlock()
	if cancel != nil {
		cancel()
	}
	if statsCancel != nil {
		statsCancel()
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

func (c *Controller) imLoop(ctx context.Context, gate *gate, gen int) {
	defer func() {
		c.st.Lock()
		if c.imGen == gen { // a newer broadcast's watcher keeps its own flag
			c.imRunning = false
		}
		c.st.Unlock()
	}()
	side := Obj{}
	seen := map[string]bool{}
	fetch := func(ctx context.Context, url string) ([]byte, error) {
		reply, err := c.raw.Call(ctx, "imfetch", Obj{"url": url})
		if err != nil {
			return nil, err
		}
		r := asObj(reply)
		side["status"], side["type"], side["size"] = r["status"], r["type"], r["size"]
		return base64.StdEncoding.DecodeString(str(r, "b64"))
	}
	observe := func(raw []byte, response *wire.Response) {
		c.st.Lock()
		c.ingestLocked(response)
		c.st.Unlock()
		if c.rec != nil {
			c.rec.IMBatch(raw, response, side, seen)
		}
	}
	poller, err := impoll.New(gate, fetch, nil, 0, observe)
	if err != nil {
		return
	}
	errs := 0
	for ctx.Err() == nil && c.getPhase() == Live {
		delay := 3 * time.Second
		result, err := poller.PollOnce(ctx)
		switch {
		case err == nil:
			errs = 0
			for _, r := range result.ShutdownResults {
				if r != nil {
					c.endFromSignal(ctx)
					return
				}
			}
			ms := result.NextIntervalMS
			delay = time.Duration(ms) * time.Millisecond
			if delay > 10*time.Second {
				delay = 10 * time.Second
			}
			if delay < time.Second {
				delay = time.Second
			}
		case errors.Is(err, impoll.ErrShutdownLatched):
			return // shutdown already latched
		case errors.Is(err, context.Canceled):
			return
		default:
			errs++
			if c.rec != nil {
				c.rec.IMError(err, side)
			}
			delay = 5 * time.Second
			if errs >= 5 {
				delay = 15 * time.Second
			}
		}
		if !sleepCtx(ctx, delay) {
			return
		}
	}
}

// endFromSignal ends the broadcast after the guard acted on a TikTok signal.
func (c *Controller) endFromSignal(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.getPhase() != Live {
		return
	}
	c.endLocked(context.WithoutCancel(ctx))
	if c.getPhase() == Closed {
		c.st.Lock()
		c.detail = "Encerrada automaticamente por sinal do TikTok (" + c.lastTrigger + "). " + c.detail
		c.st.Unlock()
	}
}

// startStats polls room/info counters every 5 s while live (display only).
func (c *Controller) startStats() {
	c.st.Lock()
	binding := c.binding
	ctx, cancel := context.WithCancel(context.Background())
	c.statsCancel = cancel
	c.st.Unlock()
	if binding == nil {
		return
	}
	go func() {
		for ctx.Err() == nil && c.getPhase() == Live {
			stats, err := c.raw.Call(ctx, "roomstats", binding.obj())
			if err == nil {
				if s := asObj(stats); isZero(s["code"]) {
					clean := Obj{}
					for k, v := range s {
						if k != "code" && k != "status" && v != nil {
							clean[k] = v
						}
					}
					c.st.Lock()
					c.view.stats = clean
					if c.view.viewers == nil && s["viewers"] != nil {
						c.view.viewers = normalize(s["viewers"])
					}
					c.st.Unlock()
				}
			}
			if !sleepCtx(ctx, 5*time.Second) {
				return
			}
		}
	}()
}

// Monitor runs until ctx ends. While live: a light OBS check every second (a stop made in OBS is noticed within
// ~1 s), and the full check + keep-alive ping every 5 s. A control failure only ends the broadcast after >= 8 s of
// errors; the scheduled end is honoured to within 200 ms.
func (c *Controller) Monitor(ctx context.Context) {
	var lastFull time.Time
	var firstFailure time.Time
	for {
		delay := 5 * time.Second
		c.st.Lock()
		live, endAt := c.phase == Live, c.endAt
		c.st.Unlock()
		if live {
			delay = time.Second
			if endAt != nil {
				if left := time.Until(*endAt); left < delay {
					delay = left
				}
				if delay < 200*time.Millisecond {
					delay = 200 * time.Millisecond
				}
			}
		}
		select {
		case <-time.After(delay):
			c.monitorTick(ctx, &lastFull, &firstFailure)
		case <-c.wake: // the phase or the schedule changed: recompute the delay
		case <-ctx.Done():
			return
		}
	}
}

func (c *Controller) monitorTick(ctx context.Context, lastFull, firstFailure *time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.st.Lock()
	live := c.phase == Live && c.binding != nil
	endAt := c.endAt
	c.st.Unlock()
	if !live {
		return
	}
	if endAt != nil && !time.Now().Before(*endAt) {
		c.emit("controller", "max_duration_reached", Obj{})
		c.endLocked(ctx)
		if c.getPhase() == Closed {
			c.st.Lock()
			c.detail = "Encerrada: a duração máxima programada foi atingida. " + c.detail
			c.st.Unlock()
		}
		return
	}
	fail := func() {
		c.failures++
		if firstFailure.IsZero() {
			*firstFailure = time.Now()
		}
		c.setDetail("Falha no controle da transmissão. Tentando manter o encerramento disponível.")
		if c.failures >= 2 && time.Since(*firstFailure) >= 8*time.Second {
			if _, err := c.endLocked(ctx); err != nil {
				c.setState(Uncertain, "Controle indisponível. Pare o OBS e confira a Central de LIVE.")
				c.save()
			}
		}
	}
	if time.Since(*lastFull) < 5*time.Second {
		quick, err := c.obs.Call(ctx, "GetStreamStatus", nil)
		if err != nil {
			fail()
			return
		}
		if !outputBusy(quick) {
			c.endLocked(ctx)
		}
		*firstFailure = time.Time{}
		return
	}
	*lastFull = time.Now()
	status, err := c.readObs(ctx)
	if err != nil {
		fail()
		return
	}
	if !outputBusy(status) {
		c.endLocked(ctx)
		return
	}
	if c.output == nil || c.output.ValidateTarget(ctx) != nil {
		fail()
		return
	}
	response, err := c.browser.Call(ctx, "ping", Obj{"binding": c.binding.obj(), "status": 2})
	if err != nil {
		fail()
		return
	}
	if code := asObj(response)["code"]; !isZero(code) && !isInt(code, 4003150) {
		c.endLocked(ctx)
	}
	c.failures = 0
	*firstFailure = time.Time{}
}

// Heartbeat tells the independent watchdog that this process (and its scheduler) is alive, every 2 s.
func (c *Controller) Heartbeat(ctx context.Context) {
	path := filepath.Join(c.root, "runtime", "heartbeat.json")
	for {
		os.MkdirAll(filepath.Dir(path), 0o755)
		raw, _ := json.Marshal(Obj{"t": epoch(time.Now()), "pid": os.Getpid(), "phase": string(c.getPhase())})
		tmp := path + ".tmp"
		if os.WriteFile(tmp, raw, 0o644) == nil {
			os.Rename(tmp, path)
		}
		if !sleepCtx(ctx, 2*time.Second) {
			return
		}
	}
}
