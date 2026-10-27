// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"tiktoklive/internal/camera"
	"tiktoklive/internal/obs"
)

// Phones as wireless cameras: the hub (package camera) carries the pairings, one per camera; the controller opens and closes them
// on the user's request and puts each camera into OBS as a source of the plugin's own type.

const cameraSourceName = "Câmera do celular"

// cameraSourceKind is the source type the plugin registers in OBS (Sources > + > "Câmera do celular (LiveTT)").
const cameraSourceKind = "tiktok_phone_camera"

func (c *Controller) cameraHub() (*camera.Hub, error) {
	if c.opts.Camera == nil {
		return nil, perr("A câmera do celular não está disponível nesta instalação.")
	}
	return c.opts.Camera, nil
}

// CameraPair opens a camera's pairing ("" = the first) and returns the state with the pairing addresses. Without the relay this
// opens the phone door (the Windows firewall may ask the first time).
func (c *Controller) CameraPair(ctx context.Context, id string) (Obj, error) {
	hub, err := c.cameraHub()
	if err != nil {
		return nil, err
	}
	port, err := hub.Listen(id)
	if err != nil {
		return nil, perr(err.Error())
	}
	c.emit("controller", "camera_listening", Obj{"port": port})
	return c.Snapshot(), nil
}

// CameraStop closes a camera's pairing ("" = every camera): its phone is disconnected and no other can pair.
func (c *Controller) CameraStop(id string) Obj {
	if hub, err := c.cameraHub(); err == nil {
		hub.Stop(id)
		c.emit("controller", "camera_closed", Obj{})
	}
	return c.Snapshot()
}

// CameraRevoke makes a camera's paired phone (and any copy of its QR code) stop working; the OBS source keeps its address.
func (c *Controller) CameraRevoke(id string) (Obj, error) {
	hub, err := c.cameraHub()
	if err != nil {
		return nil, err
	}
	if err := hub.Revoke(id); err != nil {
		return nil, perr("Câmera desconhecida.")
	}
	c.emit("controller", "camera_revoked", Obj{})
	return c.Snapshot(), nil
}

// CameraRemove deletes a camera that nothing uses (its OBS source is gone and no pairing is open).
func (c *Controller) CameraRemove(id string) (Obj, error) {
	hub, err := c.cameraHub()
	if err != nil {
		return nil, err
	}
	if err := hub.Remove(id); err != nil {
		return nil, perr(strings.ToUpper(err.Error()[:1]) + err.Error()[1:] + ".")
	}
	return c.Snapshot(), nil
}

// CameraEnsure makes sure there is a camera (so a source knows a receiver's address) without opening anything to the network:
// nothing opens until the user asks to pair.
func (c *Controller) CameraEnsure() Obj {
	if hub, err := c.cameraHub(); err == nil {
		hub.Ensure()
	}
	return c.Snapshot()
}

// CameraClaim hands a starting OBS source its camera: the one it names, else a camera nobody has taken, else a new one.
func (c *Controller) CameraClaim(id string) (Obj, error) {
	hub, err := c.cameraHub()
	if err != nil {
		return nil, err
	}
	got := hub.Claim(id)
	if got == "" {
		return nil, perr("Há câmeras demais. Apague uma fonte e remova a câmera dela no painel.")
	}
	return Obj{"id": got, "sourceURL": hub.ReceiverURL(got)}, nil
}

// CameraAddSource makes a new camera and adds its source to the scene that is on air in OBS (the pairing window opens by itself).
// It is refused during a LIVE: the new source would show to the public at once.
func (c *Controller) CameraAddSource(ctx context.Context) (Obj, error) {
	hub, err := c.cameraHub()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.getPhase() {
	case Disconnected, Login, Ready, Closed:
	default:
		return nil, perr("Adicione a câmera antes da LIVE ou depois dela: a fonte nova apareceria para o público na hora.")
	}
	if c.obs == nil {
		if err := c.connectObs(ctx); err != nil {
			return nil, perr("Abra o OBS com o plugin LiveTT e tente de novo.")
		}
	}
	scene, err := c.obs.Call(ctx, "GetCurrentProgramScene", nil)
	if err != nil {
		return nil, perr("Não consegui ler a cena do OBS.")
	}
	sceneName := str(scene, "currentProgramSceneName")
	id := hub.New()
	if id == "" {
		return nil, perr("Há câmeras demais. Apague uma fonte e remova a câmera dela no painel.")
	}
	for n := 1; n <= 20; n++ {
		name := cameraSourceName
		if n > 1 {
			name = fmt.Sprintf("%s %d", cameraSourceName, n)
		}
		_, err = c.obs.Call(ctx, "CreateInput", Obj{
			"sceneName": sceneName, "inputName": name, "inputKind": cameraSourceKind,
			// the plugin's own source type: it brings the sound, takes the picture's shape and shows the pairing QR code
			"inputSettings": Obj{"camera_id": id, "pairing_shown": false},
		})
		var oe *obs.Error
		switch {
		case err == nil:
			c.emit("controller", "camera_source_added", Obj{})
			return Obj{"scene": sceneName, "source": name}, nil
		case errors.As(err, &oe) && strings.Contains(strings.ToLower(oe.Msg), "already exists"):
			continue // a source with that name exists: try the next number
		default:
			hub.Discard(id)
			return nil, perr("O OBS não aceitou criar a fonte da câmera.")
		}
	}
	hub.Discard(id)
	return nil, perr("Já existem fontes demais com esse nome.")
}

func (c *Controller) cameraStatus() Obj {
	if c.opts.Camera == nil {
		return Obj{"available": false}
	}
	status := c.opts.Camera.Status()
	status["available"] = true
	return status
}
