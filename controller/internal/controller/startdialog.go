// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package controller

import "context"

// OpenStartDialog asks the plugin to show the same window the OBS "Iniciar transmissão" button opens, so the panel does
// not need a second start form. It only opens a window: nothing is created or published until the user confirms in it.
func (c *Controller) OpenStartDialog(ctx context.Context) (Obj, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.obs == nil {
		return nil, perr("Conecte o OBS primeiro.")
	}
	switch c.getPhase() {
	case Ready, Closed, Disconnected, Login:
	default:
		return nil, perr("Já existe uma LIVE em andamento ou uma pendência a resolver.")
	}
	if _, err := c.obs.Call(ctx, "OpenStartDialog", nil); err != nil {
		return nil, perr("Não consegui abrir a janela de início no OBS. Atualize o plugin.")
	}
	return c.Snapshot(), nil
}
