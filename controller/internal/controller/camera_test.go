// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"tiktoklive/internal/camera"
	"tiktoklive/internal/obs"
)

func cameraRig(t *testing.T) (*Controller, *fakeObs, *camera.Hub) {
	t.Helper()
	hub := camera.Open(t.TempDir(), fstest.MapFS{}, 8765)
	hub.BindAddr = "127.0.0.1" // never open the test machine to its network
	fo := newFakeObs()
	fo.scene = "Brio 4k"
	c := New(Options{Root: t.TempDir(), Browser: newFakeBrowser(), Camera: hub,
		ObsFactory: func(context.Context) (Obs, error) { return fo, nil }})
	return c, fo, hub
}

func cameraList(c *Controller) []Obj { return c.Snapshot()["camera"].(Obj)["cameras"].([]Obj) }

func TestAddingTheCameraPutsANewCameraSourceInTheSceneOnAir(t *testing.T) {
	c, fo, hub := cameraRig(t)
	var made []Obj
	used := map[string]bool{} // like OBS, which refuses a second source with the same name
	fo.override = func(name string, data map[string]any) (map[string]any, error, bool) {
		if name == "CreateInput" {
			n, _ := data["inputName"].(string)
			if used[n] {
				return nil, &obs.Error{Msg: "A source with that name already exists", Code: 601}, true
			}
			used[n] = true
			made = append(made, data)
			return Obj{}, nil, true
		}
		return nil, nil, false
	}
	out, err := c.CameraAddSource(context.Background())
	if err != nil || out["scene"] != "Brio 4k" || out["source"] != cameraSourceName {
		t.Fatalf("%v %v", out, err)
	}
	m := made[0]
	settings := m["inputSettings"].(Obj)
	id, _ := settings["camera_id"].(string)
	if m["inputKind"] != cameraSourceKind || m["sceneName"] != "Brio 4k" || m["inputName"] != cameraSourceName || id == "" {
		t.Fatalf("%v", m)
	}
	if !hub.KnownID(id) {
		t.Fatal("the source must be tied to a camera that exists")
	}
	if !strings.HasPrefix(hub.ReceiverURL(id), "http://127.0.0.1:8765/camera/rx?c=") {
		t.Fatalf("the receiver must load from the loopback server: %v", hub.ReceiverURL(id))
	}
	// a second one is a different camera in a differently named source
	out2, err := c.CameraAddSource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	id2, _ := made[1]["inputSettings"].(Obj)["camera_id"].(string)
	if id2 == "" || id2 == id || len(cameraList(c)) != 2 {
		t.Fatalf("two sources must never share a camera: %q %q %v", id, id2, cameraList(c))
	}
	if out2["source"] == out["source"] {
		t.Fatalf("the sources need different names: %v", out2)
	}
}

func TestAddingTheCameraTriesTheNextNameWhenOneIsTaken(t *testing.T) {
	c, fo, _ := cameraRig(t)
	var names []string
	fo.override = func(name string, data map[string]any) (map[string]any, error, bool) {
		if name == "CreateInput" {
			n, _ := data["inputName"].(string)
			names = append(names, n)
			if n == cameraSourceName {
				return nil, &obs.Error{Msg: "A source with that name already exists", Code: 601}, true
			}
			return Obj{}, nil, true
		}
		return nil, nil, false
	}
	out, err := c.CameraAddSource(context.Background())
	if err != nil || out["source"] != cameraSourceName+" 2" || len(names) != 2 {
		t.Fatalf("%v %v %v", out, err, names)
	}
}

func TestAddingTheCameraIsRefusedOnAirAndOBSRefusalsLeaveNoCameraBehind(t *testing.T) {
	c, fo, _ := cameraRig(t)
	for _, phase := range []Phase{Creating, Live, Ending, Uncertain, Recovery} {
		c.setState(phase, "")
		if _, err := c.CameraAddSource(context.Background()); err == nil {
			t.Errorf("%s: the camera must not enter a scene that may be on air", phase)
		}
	}
	if fo.has("CreateInput") || len(cameraList(c)) != 0 {
		t.Fatal("nothing may be created while refusing")
	}
	c.setState(Ready, "")
	fo.override = func(name string, data map[string]any) (map[string]any, error, bool) {
		if name == "CreateInput" {
			return nil, &obs.Error{Msg: "Unknown input kind", Code: 602}, true
		}
		return nil, nil, false
	}
	if _, err := c.CameraAddSource(context.Background()); err == nil {
		t.Fatal("an OBS that cannot create the source must be reported")
	}
	if len(cameraList(c)) != 0 {
		t.Fatalf("a camera made for a source that failed must be dropped: %v", cameraList(c))
	}
}

func TestASourceClaimsItsCameraAndAnUnknownOneGetsAFreshOne(t *testing.T) {
	c, _, hub := cameraRig(t)
	first, err := c.CameraClaim("")
	if err != nil || first["id"] == "" || first["sourceURL"] == "" {
		t.Fatalf("%v %v", first, err)
	}
	second, _ := c.CameraClaim("")
	if second["id"] == first["id"] {
		t.Fatal("two sources must not get the same camera")
	}
	again, _ := c.CameraClaim(first["id"].(string))
	if again["id"] != first["id"] {
		t.Fatal("a source gets back the camera it remembers")
	}
	if !hub.KnownID(first["id"].(string)) || !hub.KnownID(second["id"].(string)) {
		t.Fatal("claimed cameras exist")
	}
}

func TestTheSnapshotCarriesTheCameraStateWithoutSecrets(t *testing.T) {
	c, _, hub := cameraRig(t)
	id := hub.Ensure()
	if _, err := c.CameraRevoke(id); err != nil {
		t.Fatal(err)
	}
	cam := c.Snapshot()["camera"].(Obj)
	if cam["available"] != true || cam["paired"] != true || cam["listening"] != false || len(cameraList(c)) != 1 {
		t.Fatalf("%v", cam)
	}
	for k, v := range cam {
		if strings.Contains(strings.ToLower(k), "secret") || strings.Contains(fmt.Sprint(v), "k=") {
			t.Fatalf("the state leaks the pairing secret: %s=%v", k, v)
		}
	}
	if _, err := c.CameraRevoke("desconhecida"); err == nil {
		t.Fatal("an unknown camera cannot be revoked")
	}
	none := New(Options{Root: t.TempDir(), Browser: newFakeBrowser()})
	if none.Snapshot()["camera"].(Obj)["available"] != false {
		t.Fatal("without a hub the camera is not offered")
	}
	if _, err := none.CameraAddSource(context.Background()); err == nil {
		t.Fatal("without a hub there is nothing to add")
	}
}

func TestOpeningAPairingShowsAddressesButNeverTheSecret(t *testing.T) {
	c, _, hub := cameraRig(t)
	id := hub.Ensure()
	state, err := c.CameraPair(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CameraStop("")
	cam := state["camera"].(Obj)
	if cam["listening"] != true || cam["port"].(int) == 0 {
		t.Fatalf("%v", cam)
	}
	if hosts := cam["hosts"].([]string); len(hosts) != 1 || hosts[0] != "127.0.0.1" {
		t.Fatalf("%v", cam["hosts"])
	}
	if strings.Contains(fmt.Sprint(cam), "k=") || strings.Contains(fmt.Sprint(state), "k=") {
		t.Fatalf("the secret leaked into the state: %v", cam)
	}
	if urls := hub.PhoneURLs(id); len(urls) != 1 || !strings.Contains(urls[0], "?k=") {
		t.Fatalf("the QR code still needs the full address: %v", urls)
	}
	if after := c.CameraStop(id)["camera"].(Obj); after["listening"] != false {
		t.Fatalf("%v", after)
	}
}

func TestACameraThatNothingUsesCanBeRemoved(t *testing.T) {
	c, _, hub := cameraRig(t)
	a := hub.Ensure()
	b := hub.New()
	if _, err := c.CameraRemove(b); err == nil {
		t.Fatal("a camera just handed to a source cannot be removed")
	}
	if _, err := c.CameraRemove("desconhecida"); err == nil {
		t.Fatal("an unknown camera cannot be removed")
	}
	if !hub.KnownID(a) || !hub.KnownID(b) {
		t.Fatal("a refused removal must change nothing")
	}
}
