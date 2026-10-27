// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"sync"
	"time"
)

// PanelClient talks to the loopback panel API. The page token is read from the page itself (same trust as the dock).
type PanelClient struct {
	Base string
	// Launch starts the panel in the background when it is not answering (nil = never start it).
	Launch func() error

	mu    sync.Mutex
	token string
}

func NewPanelClient(base string, launch func() error) *PanelClient {
	return &PanelClient{Base: base, Launch: launch}
}

var tokenRE = regexp.MustCompile(`name="panel-token" content="([^"]+)"`)

func (p *PanelClient) call(ctx context.Context, method, path string, data Obj, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var body io.Reader
	if data != nil {
		raw, _ := json.Marshal(data)
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.Base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Origin", p.Base)
	p.mu.Lock()
	token := p.token
	p.mu.Unlock()
	if token != "" {
		req.Header.Set("X-Panel-Token", token)
	}
	if data != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, terr("O painel local não está respondendo. Use a tool connect_panel para iniciá-lo.")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 400 {
		var detail struct {
			Error string `json:"error"`
		}
		json.Unmarshal(raw, &detail)
		if resp.StatusCode == http.StatusForbidden && method == http.MethodPost {
			p.mu.Lock()
			p.token = ""
			p.mu.Unlock()
		}
		if detail.Error != "" {
			return nil, terr(detail.Error)
		}
		return nil, terrf("O painel recusou o pedido (HTTP %d).", resp.StatusCode)
	}
	return raw, nil
}

func (p *PanelClient) ensureToken(ctx context.Context) error {
	p.mu.Lock()
	have := p.token != ""
	p.mu.Unlock()
	if have {
		return nil
	}
	page, err := p.call(ctx, http.MethodGet, "/", nil, 20*time.Second)
	if err != nil {
		return err
	}
	m := tokenRE.FindSubmatch(page)
	if m == nil {
		return terr("Não foi possível obter o token do painel.")
	}
	p.mu.Lock()
	p.token = string(m[1])
	p.mu.Unlock()
	return nil
}

func decode(raw []byte) (Obj, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out Obj
	if err := dec.Decode(&out); err != nil {
		return nil, terr("Resposta inválida do painel.")
	}
	return out, nil
}

func (p *PanelClient) State(ctx context.Context) (Obj, error) {
	raw, err := p.call(ctx, http.MethodGet, "/api/state", nil, 20*time.Second)
	if err != nil {
		return nil, err
	}
	return decode(raw)
}

func (p *PanelClient) Post(ctx context.Context, path string, data Obj, timeout time.Duration) (Obj, error) {
	if err := p.ensureToken(ctx); err != nil {
		return nil, err
	}
	if data == nil {
		data = Obj{}
	}
	raw, err := p.call(ctx, http.MethodPost, path, data, timeout)
	if err != nil {
		return nil, err
	}
	return decode(raw)
}

// EnsureRunning starts the panel when it does not answer and waits (up to 10 s) for it.
func (p *PanelClient) EnsureRunning(ctx context.Context) (bool, error) {
	if _, err := p.State(ctx); err == nil {
		return false, nil
	}
	if p.Launch == nil {
		return false, terr("Não consegui iniciar o painel local.")
	}
	if err := p.Launch(); err != nil {
		return false, terr("Não consegui iniciar o painel local.")
	}
	for i := 0; i < 40; i++ {
		select {
		case <-time.After(250 * time.Millisecond):
		case <-ctx.Done():
			return false, ctx.Err()
		}
		if _, err := p.State(ctx); err == nil {
			p.mu.Lock()
			p.token = ""
			p.mu.Unlock()
			return true, nil
		}
	}
	return false, terr("Não consegui iniciar o painel local.")
}
