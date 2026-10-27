// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"tiktoklive/internal/guard"
)

func getJSON(ctx context.Context, rawurl string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", rawurl, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}

// observeGuard satisfies the poller's guard for observation only: it never restarts blocked and is never asked to handle.
type observeGuard struct{ room string }

func (g observeGuard) RoomID() string       { return g.room }
func (g observeGuard) SessionID() string    { return "observe" }
func (g observeGuard) RestartBlocked() bool { return false }
func (observeGuard) Handle(context.Context, guard.Signal) (*guard.Result, error) {
	panic("observation must never dispatch")
}
