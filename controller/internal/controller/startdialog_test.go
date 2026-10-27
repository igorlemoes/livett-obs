// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package controller

import (
	"testing"
	"time"

	"tiktoklive/internal/obs"
)

func buttonEnv(t *testing.T) *env {
	e := newEnv(t, nil)
	e.c.StartPoll = 5 * time.Millisecond
	return e
}

func serviceNow(e *env) (string, Obj) {
	e.obs.mu.Lock()
	defer e.obs.mu.Unlock()
	return str(e.obs.service, "streamServiceType"), asObj(e.obs.service["streamServiceSettings"])
}

func setService(e *env, service Obj) {
	e.obs.mu.Lock()
	e.obs.service = service
	e.obs.mu.Unlock()
}

func TestTheStartWindowIsOpenedThroughThePluginOnlyWhenNothingIsOnAir(t *testing.T) {
	e := buttonEnv(t)
	if _, err := e.c.OpenStartDialog(ctx()); err != nil || !e.obs.has("OpenStartDialog") {
		t.Fatalf("%v", err)
	}
	e.obs.mu.Lock()
	e.obs.calls = nil
	e.obs.mu.Unlock()
	for _, phase := range []Phase{Creating, Live, Ending, Recovery, Uncertain} {
		e.c.setState(phase, "")
		if _, err := e.c.OpenStartDialog(ctx()); err == nil {
			t.Fatalf("no start window in %s", phase)
		}
	}
	if e.obs.has("OpenStartDialog") {
		t.Fatal("the plugin must not be asked when a LIVE exists")
	}
}

func TestThePanelStartWorksWhilePluginServiceIsInTheProfileAndGivesItBack(t *testing.T) {
	e := buttonEnv(t)
	setService(e, Obj{"streamServiceType": obs.NativeServiceType, "streamServiceSettings": Obj{}})
	e.mustStart() // the panel's own start: it configures a normal destination and starts OBS itself
	if typ, settings := serviceNow(e); typ != "rtmp_custom" || str(settings, "key") != "synthetic-key" {
		t.Fatalf("the panel start must use a normal destination while it runs: %s %v", typ, settings)
	}
	if _, err := e.c.End(ctx()); err != nil {
		t.Fatal(err)
	}
	if typ, settings := serviceNow(e); typ != obs.NativeServiceType || str(settings, "key") != "" {
		t.Fatalf("at the end the service must come back, without any key: %s %v", typ, settings)
	}
}
