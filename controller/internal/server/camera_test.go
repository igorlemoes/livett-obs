// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"

	"tiktoklive/internal/camera"
)

type cameraAPI struct {
	*fakeAPI
	calls []string
}

func (c *cameraAPI) CameraPair(_ context.Context, id string) (Obj, error) {
	c.calls = append(c.calls, "pair:"+id)
	return Obj{"ok": true}, nil
}
func (c *cameraAPI) CameraStop(id string) Obj {
	c.calls = append(c.calls, "stop:"+id)
	return Obj{"ok": true}
}
func (c *cameraAPI) CameraRevoke(id string) (Obj, error) {
	c.calls = append(c.calls, "revoke:"+id)
	return Obj{"ok": true}, nil
}
func (c *cameraAPI) CameraRemove(id string) (Obj, error) {
	c.calls = append(c.calls, "remove:"+id)
	return Obj{"ok": true}, nil
}
func (c *cameraAPI) CameraClaim(id string) (Obj, error) {
	c.calls = append(c.calls, "claim:"+id)
	return Obj{"id": "novo"}, nil
}
func (c *cameraAPI) CameraAddSource(context.Context) (Obj, error) {
	c.calls = append(c.calls, "add")
	return Obj{"ok": true}, nil
}

func cameraRig(t *testing.T) (*rig, *camera.Hub) {
	r := newRig(t)
	pages := fstest.MapFS{
		"rx.html": {Data: []byte("<p>rx</p>")}, "rx.js": {Data: []byte("//rx")}, "rx.css": {Data: []byte("/*rx*/")},
		"phone.html": {Data: []byte("<p>phone</p>")},
	}
	hub := camera.Open(t.TempDir(), pages, r.srv.Port)
	hub.Ensure()
	r.srv.Camera, r.srv.CameraWeb = hub, pages
	return r, hub
}

func TestReceiverPagesAndSignallingAreServedToOBS(t *testing.T) {
	r, hub := cameraRig(t)
	for path, want := range map[string]string{"/camera/rx?c=" + hub.ID(): "<p>rx</p>", "/camera/rx.js": "//rx", "/camera/rx.css": "/*rx*/"} {
		code, body, header := r.get(path)
		if code != 200 || body != want {
			t.Errorf("%s: %d %q", path, code, body)
		}
		if header.Get("Content-Security-Policy") == "" {
			t.Errorf("%s has no content policy", path)
		}
	}
	if code, _, _ := r.get("/camera/rx/recv?c=nope&after=0"); code != 404 {
		t.Errorf("an unknown camera id got %d", code)
	}
	code, body, _ := r.get("/camera/rx/recv?c=" + hub.ID() + "&after=0")
	if code != 200 || !strings.Contains(body, `"messages":[]`) {
		t.Errorf("%d %s", code, body)
	}
}

func TestReceiverCanOnlyPostFromItsOwnOriginWithTheCameraId(t *testing.T) {
	r, hub := cameraRig(t)
	send := func(id, origin string) int {
		req, _ := http.NewRequest("POST", r.base+"/camera/rx/send?c="+id, bytes.NewReader([]byte(`{"type":"answer","data":{"sdp":"x"}}`)))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := send(hub.ID(), "http://"+r.host); code != 200 {
		t.Fatalf("the receiver page must be able to answer: %d", code)
	}
	for name, code := range map[string]int{
		"other site": send(hub.ID(), "https://example.com"),
		"no origin":  send(hub.ID(), ""),
		"unknown id": send("nope", "http://"+r.host),
		"wrong host": send(hub.ID(), "http://evil.invalid:1"),
	} {
		if code != 403 {
			t.Errorf("%s: %d", name, code)
		}
	}
}

func TestQRCodeIsAnImageOfThePairingAddress(t *testing.T) {
	r, _ := cameraRig(t)
	if code, _, _ := r.get("/camera/qr.svg"); code != 404 {
		t.Errorf("without an open phone door there is nothing to pair: %d", code)
	}
}

func TestCameraButtonsNeedThePanelTokenAndAnAPIThatOffersThem(t *testing.T) {
	r, _ := cameraRig(t)
	if code, _ := r.post("/api/camera/pair", Obj{}, nil); code != 404 {
		t.Errorf("an API without a camera must say not found, got %d", code)
	}
	api := &cameraAPI{fakeAPI: r.api}
	r.srv.API = api
	for _, path := range []string{"/api/camera/pair", "/api/camera/ensure", "/api/camera/claim", "/api/camera/stop", "/api/camera/revoke", "/api/camera/remove", "/api/camera/add-source"} {
		if code, _ := r.post(path, Obj{"id": "cam2"}, map[string]string{"X-Panel-Token": ""}); code != 403 {
			t.Errorf("%s without the token: %d", path, code)
		}
		if code, _ := r.post(path, Obj{"id": "cam2"}, nil); code != 200 {
			t.Errorf("%s: %d", path, code)
		}
	}
	// every button names the camera it is about
	if strings.Join(api.calls, ",") != "pair:cam2,ensure,claim:cam2,stop:cam2,revoke:cam2,remove:cam2,add" {
		t.Errorf("%v", api.calls)
	}
}

func (c *cameraAPI) CameraEnsure() Obj { c.calls = append(c.calls, "ensure"); return Obj{"ok": true} }

func TestQRRowsAreServedForThePluginDialog(t *testing.T) {
	r, hub := cameraRig(t)
	hub.BindAddr = "127.0.0.1" // never open the test machine to its network
	if _, err := hub.Listen(hub.ID()); err != nil {
		t.Fatal(err)
	}
	defer hub.Stop("")
	code, body, _ := r.get("/camera/qr.txt?c=" + hub.ID() + "&i=0")
	var out struct{ Rows []string }
	if code != 200 || json.Unmarshal([]byte(body), &out) != nil || len(out.Rows) < 21 {
		t.Fatalf("%d %.80s", code, body)
	}
	for _, row := range out.Rows {
		if len(row) != len(out.Rows) || strings.Trim(row, "01") != "" {
			t.Fatalf("not a square of 0 and 1: %q", row)
		}
	}
	if code, _, _ := r.get("/camera/qr.txt?c=" + hub.ID() + "&i=9"); code != 404 {
		t.Fatalf("%d", code)
	}
	if code, _, _ := r.get("/camera/qr.txt?c=desconhecida&i=0"); code != 404 {
		t.Fatalf("a camera that does not exist has no QR code: %d", code)
	}
	other := hub.New()
	if code, _, _ := r.get("/camera/qr.txt?c=" + other + "&i=0"); code != 404 {
		t.Fatalf("a camera whose pairing is not open has no QR code: %d", code)
	}
}

func TestTheReceiverGetsItsConfigAndTheRelayIsAllowedInThePolicy(t *testing.T) {
	r, hub := cameraRig(t)
	code, body, header := r.get("/camera/rx/config?c=" + hub.ID())
	if code != 200 || !strings.Contains(body, `"mode":"local"`) {
		t.Fatalf("%d %s", code, body)
	}
	if strings.Contains(header.Get("Content-Security-Policy"), "wss://") {
		t.Fatal("without a relay the page must not be allowed to open sockets elsewhere")
	}
	if code, _, _ := r.get("/camera/rx/config?c=nope"); code != 404 {
		t.Fatalf("an unknown id got %d", code)
	}
	hub.Relay = "https://camera-teste.workers.dev"
	_, body, header = r.get("/camera/rx/config?c=" + hub.ID())
	if !strings.Contains(body, `"mode":"relay"`) || !strings.Contains(body, "wss://camera-teste.workers.dev/ws?room=") {
		t.Fatal(body)
	}
	if !strings.Contains(header.Get("Content-Security-Policy"), "connect-src 'self' wss://camera-teste.workers.dev;") {
		t.Fatalf("%q", header.Get("Content-Security-Policy"))
	}
	if strings.Contains(body, hub.ID()) && strings.Contains(body, "#k=") {
		t.Fatal("the phone's address must not be in the receiver's config")
	}
}

func TestThePluginCanReadThePictureSize(t *testing.T) {
	r, hub := cameraRig(t)
	id, other := hub.ID(), hub.New()
	code, body, _ := r.get("/camera/size?c=" + id)
	if code != 200 || !strings.Contains(body, `"width":0`) {
		t.Fatalf("%d %s", code, body)
	}
	hub.Send(id, camera.RoleReceiver, "state", json.RawMessage(`"connected"`))
	hub.Send(id, camera.RoleReceiver, "size", json.RawMessage(`{"width":1080,"height":1920}`))
	if _, body, _ := r.get("/camera/size?c=" + id); !strings.Contains(body, `"width":1080`) || !strings.Contains(body, `"height":1920`) {
		t.Fatal(body)
	}
	if _, body, _ := r.get("/camera/size?c=" + other); !strings.Contains(body, `"width":0`) {
		t.Fatalf("another camera's size must not leak: %s", body)
	}
}
