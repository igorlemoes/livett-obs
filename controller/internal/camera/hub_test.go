// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package camera

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func newHub(t *testing.T) *Hub {
	t.Helper()
	web := fstest.MapFS{
		"phone.html": {Data: []byte("<html>phone</html>")},
		"phone.js":   {Data: []byte("// phone")},
		"camera.css": {Data: []byte("/* css */")},
	}
	h := Open(t.TempDir(), web, 8765)
	h.Ensure()
	return h
}

func recvNow(t *testing.T, h *Hub, id, role string, after int) []Message {
	t.Helper()
	messages, _, err := h.Recv(context.Background(), id, role, after, 0)
	if err != nil {
		t.Fatal(err)
	}
	return messages
}

func camStatus(h *Hub, id string) Obj {
	for _, c := range h.Status()["cameras"].([]Obj) {
		if c["id"] == id {
			return c
		}
	}
	return nil
}

func secretOf(h *Hub, id string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.camLocked(id).Secret
}

func rxSecretOf(h *Hub, id string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.camLocked(id).RxSecret
}

func TestMessagesReachTheOtherEndInOrder(t *testing.T) {
	h := newHub(t)
	id := h.ID()
	h.Send(id, RolePhone, "hello", json.RawMessage(`"Pixel 8"`))
	h.Send(id, RolePhone, "offer", json.RawMessage(`{"sdp":"x"}`))
	h.Send(id, RolePhone, "ice", json.RawMessage(`{"candidate":"c1"}`))
	got := recvNow(t, h, id, RoleReceiver, 0)
	if len(got) != 3 || got[0].Type != "hello" || got[1].Type != "offer" || got[2].Type != "ice" || got[0].Seq >= got[1].Seq {
		t.Fatalf("%+v", got)
	}
	if again := recvNow(t, h, id, RoleReceiver, got[2].Seq); len(again) != 0 {
		t.Fatalf("already delivered messages came back: %+v", again)
	}
	if own := recvNow(t, h, id, RolePhone, 0); len(own) != 0 {
		t.Fatalf("a message came back to its sender: %+v", own)
	}
	h.Send(id, RoleReceiver, "answer", json.RawMessage(`{"sdp":"y"}`))
	if back := recvNow(t, h, id, RolePhone, 0); len(back) != 1 || back[0].Type != "answer" {
		t.Fatalf("%+v", back)
	}
	if err := h.Send("desconhecida", RolePhone, "offer", nil); err == nil {
		t.Fatal("an unknown camera must be refused")
	}
}

func TestAHelloStartsOverAndTheLabelIsCleaned(t *testing.T) {
	h := newHub(t)
	id := h.ID()
	h.Send(id, RolePhone, "hello", json.RawMessage(`"old"`))
	h.Send(id, RolePhone, "offer", json.RawMessage(`{}`))
	h.Send(id, RolePhone, "hello", json.RawMessage(`"iPhone\u0007 de Ana `+strings.Repeat("x", 80)+`"`))
	got := recvNow(t, h, id, RoleReceiver, 0)
	if len(got) != 1 || got[0].Type != "hello" {
		t.Fatalf("a new hello must drop the older negotiation: %+v", got)
	}
	label := camStatus(h, id)["label"].(string)
	if len([]rune(label)) > 40 || strings.ContainsRune(label, 7) || !strings.HasPrefix(label, "iPhone de Ana") {
		t.Fatalf("label %q", label)
	}
}

func TestRecvWaitsForTheNextMessage(t *testing.T) {
	h := newHub(t)
	id := h.ID()
	done := make(chan []Message, 1)
	go func() {
		m, _, _ := h.Recv(context.Background(), id, RoleReceiver, 0, 5*time.Second)
		done <- m
	}()
	time.Sleep(50 * time.Millisecond)
	h.Send(id, RolePhone, "offer", json.RawMessage(`{}`))
	select {
	case m := <-done:
		if len(m) != 1 {
			t.Fatalf("%+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the long poll did not wake up")
	}
	start := time.Now()
	if m, _, _ := h.Recv(context.Background(), id, RoleReceiver, 99, 150*time.Millisecond); len(m) != 0 || time.Since(start) < 100*time.Millisecond {
		t.Fatalf("should have waited and found nothing: %+v", m)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := h.Recv(ctx, id, RoleReceiver, 99, 5*time.Second); err == nil {
		t.Fatal("a cancelled caller must stop waiting")
	}
	if _, _, err := h.Recv(context.Background(), "desconhecida", RoleReceiver, 0, 0); err == nil {
		t.Fatal("an unknown camera must be refused")
	}
}

func TestANewPageSkipsWhatWasSaidBeforeIt(t *testing.T) {
	h := newHub(t)
	id := h.ID()
	h.Send(id, RolePhone, "offer", json.RawMessage(`{}`))
	_, last, _ := h.Recv(context.Background(), id, RoleReceiver, -1, 0)
	if last == 0 {
		t.Fatal("last must report the newest seq")
	}
	if m := recvNow(t, h, id, RoleReceiver, last); len(m) != 0 {
		t.Fatalf("%+v", m)
	}
}

func TestReceiverStateDrivesTheStatus(t *testing.T) {
	h := newHub(t)
	id := h.ID()
	if c := camStatus(h, id); c["receiver"].(bool) || c["connected"].(bool) {
		t.Fatal("nothing has talked yet")
	}
	h.Send(id, RoleReceiver, "state", json.RawMessage(`"connected"`))
	if c := camStatus(h, id); !c["receiver"].(bool) || !c["connected"].(bool) {
		t.Fatalf("%v", c)
	}
	h.Send(id, RoleReceiver, "state", json.RawMessage(`"idle"`))
	if camStatus(h, id)["connected"].(bool) {
		t.Fatal("idle means not connected")
	}
	h.Send(id, RoleReceiver, "state", json.RawMessage(`"anything else"`))
	if camStatus(h, id)["connected"].(bool) {
		t.Fatal("an unknown state must not count")
	}
	if got := recvNow(t, h, id, RolePhone, 0); len(got) != 0 {
		t.Fatalf("state is for the hub, not for the phone: %+v", got)
	}
}

func TestRevokeChangesTheSecretButKeepsTheReceiverAddress(t *testing.T) {
	h := newHub(t)
	id := h.ID()
	url := h.ReceiverURL(id)
	secret := secretOf(h, id)
	if !h.Authorized(id, secret) {
		t.Fatal("the pairing must authorise itself")
	}
	if err := h.Revoke(id); err != nil {
		t.Fatal(err)
	}
	if h.Authorized(id, secret) {
		t.Fatal("the old secret must stop working")
	}
	if h.ID() != id || h.ReceiverURL(id) != url {
		t.Fatal("the OBS source's address must survive a new QR code")
	}
	if !strings.HasPrefix(url, "http://127.0.0.1:8765/camera/rx?c=") {
		t.Fatal(url)
	}
	// the pairing survives a restart
	again := Open(h.Dir, h.Web, 8765)
	if again.ID() != id || again.Authorized(id, secret) || !again.Authorized(id, secretOf(h, id)) {
		t.Fatal("cameras.json was not kept")
	}
	if h.Revoke("desconhecida") == nil {
		t.Fatal("an unknown camera cannot be revoked")
	}
}

func TestThePhoneDoorNeedsThePairingSecret(t *testing.T) {
	h := newHub(t)
	id, secret := h.ID(), secretOf(h, h.ID())
	srv := httptest.NewServer(h.Serve())
	defer srv.Close()
	get := func(path string) *http.Response {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if get("/c/"+id+"?k="+secret).StatusCode != 200 {
		t.Fatal("the paired phone must get its page")
	}
	page := get("/c/" + id + "?k=" + secret)
	if csp := page.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("phone page without a strict policy: %q", csp)
	}
	for _, path := range []string{"/c/" + id, "/c/" + id + "?k=wrong", "/c/other?k=" + secret, "/c/" + id + "/recv?k=bad", "/", "/api/state", "/camera/rx"} {
		if code := get(path).StatusCode; code != 404 {
			t.Errorf("%s answered %d", path, code)
		}
	}
	if get("/s/phone.js").StatusCode != 200 || get("/s/camera.css").StatusCode != 200 {
		t.Fatal("static files of the phone page must load")
	}
	// the phone sends, the receiver gets it
	resp, err := http.Post(srv.URL+"/c/"+id+"/send?k="+secret, "application/json", strings.NewReader(`{"type":"offer","data":{"sdp":"v=0"}}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v", err, resp)
	}
	resp.Body.Close()
	if got := recvNow(t, h, id, RoleReceiver, 0); len(got) != 1 || got[0].Type != "offer" {
		t.Fatalf("%+v", got)
	}
	// and the long poll returns JSON
	h.Send(id, RoleReceiver, "answer", json.RawMessage(`{"sdp":"v=0"}`))
	r2, err := http.Get(srv.URL + "/c/" + id + "/recv?k=" + secret + "&after=0&wait=1")
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	var out struct {
		Messages []Message `json:"messages"`
	}
	if json.NewDecoder(r2.Body).Decode(&out) != nil || len(out.Messages) != 1 || out.Messages[0].Type != "answer" {
		t.Fatalf("%+v", out)
	}
}

func TestOnlyPrivateAddressesAreServed(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:5000": true, "192.168.1.20:5000": true, "10.1.2.3:1": true, "172.20.0.5:9": true, "169.254.1.1:1": true,
		"8.8.8.8:1": false, "203.0.113.9:1": false, "[2001:db8::1]:1": false, "nonsense": false,
	} {
		if privateRemote(addr) != want {
			t.Errorf("%s: want %v", addr, want)
		}
	}
}

func TestTooManyWrongSecretsTurnTheCallerAway(t *testing.T) {
	h := newHub(t)
	srv := httptest.NewServer(h.Serve())
	defer srv.Close()
	id := h.ID()
	for i := 0; i <= maxFailures+2; i++ {
		resp, _ := http.Get(srv.URL + "/c/" + id + "?k=guess")
		resp.Body.Close()
	}
	resp, _ := http.Get(srv.URL + "/c/" + id + "?k=" + secretOf(h, id))
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("a caller that guessed too often got %d even with the right secret", resp.StatusCode)
	}
}

func TestTheReceiverCountsAsPresentWhileItWaitsForMessages(t *testing.T) {
	h := newHub(t)
	id := h.ID()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.Recv(ctx, id, RoleReceiver, 0, 30*time.Second)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	h.mu.Lock()
	h.camLocked(id).rxSeen = time.Now().Add(-time.Minute) // as if its last message was long ago
	h.mu.Unlock()
	if !camStatus(h, id)["receiver"].(bool) {
		t.Fatal("a receiver in the middle of a long poll is present")
	}
	cancel()
	<-done
	if !camStatus(h, id)["receiver"].(bool) {
		t.Fatal("it was seen a moment ago")
	}
	h.mu.Lock()
	h.camLocked(id).rxSeen = time.Now().Add(-time.Minute)
	h.mu.Unlock()
	if camStatus(h, id)["receiver"].(bool) {
		t.Fatal("a receiver that stopped asking must not count as present")
	}
}

// fakeCloudflared is a script that behaves like cloudflared: it prints the public address on stderr and keeps running.
func fakeCloudflared(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cloudflared.cmd")
	if err := os.WriteFile(path, []byte("@echo off\r\n"+body+"\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestWithCloudflaredThePhoneGetsATrustedAddressAndNothingOpensToTheNetwork(t *testing.T) {
	h := newHub(t)
	id := h.ID()
	h.Tunnel = fakeCloudflared(t, "echo 2026 INF https://calm-fox-test.trycloudflare.com 1>&2\r\nping -n 60 127.0.0.1 >nul")
	port, err := h.Listen(id)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop("")
	st := h.Status()
	if st["mode"] != "tunnel" || st["needsTunnel"] != false {
		t.Fatalf("%v", st)
	}
	if hosts := st["hosts"].([]string); len(hosts) != 1 || hosts[0] != "calm-fox-test.trycloudflare.com" {
		t.Fatalf("%v", hosts)
	}
	urls := h.PhoneURLs(id)
	if len(urls) != 1 || !strings.HasPrefix(urls[0], "https://calm-fox-test.trycloudflare.com/c/"+id+"?k=") {
		t.Fatalf("%v", urls)
	}
	if strings.Contains(fmt.Sprint(st), "k=") {
		t.Fatal("the state must not carry the secret")
	}
	// the door itself only listens on this computer: the tunnel is what reaches it
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/s/phone.js", port))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v", err, resp)
	}
	resp.Body.Close()
	if _, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", firstLocalAddress(t), port), 300*time.Millisecond); err == nil {
		t.Fatal("the tunnel's door must not be reachable from the local network address")
	}
	h.Stop(id)
	if h.Status()["listening"].(bool) {
		t.Fatal("stop must close the door")
	}
}

func firstLocalAddress(t *testing.T) string {
	for _, ip := range localAddresses() {
		return ip.String()
	}
	t.Skip("no local network address on this machine")
	return ""
}

func TestWithoutCloudflaredTheHintAsksForIt(t *testing.T) {
	h := newHub(t)
	h.Tunnel = filepath.Join(t.TempDir(), "nao-existe.exe")
	if h.Status()["needsTunnel"] != true {
		t.Fatalf("%v", h.Status())
	}
	if h.cloudflaredPath() != "" {
		t.Fatal("a missing program must not be reported as found")
	}
}

func TestATunnelThatFailsToStartIsReportedQuicklyAndLeavesNothingOpen(t *testing.T) {
	h := newHub(t)
	h.Tunnel = fakeCloudflared(t, "echo erro 1>&2\r\nexit /b 1")
	tunnelWait = 5 * time.Second
	start := time.Now()
	if _, err := h.Listen(h.ID()); err == nil || !strings.Contains(err.Error(), "Cloudflare") {
		t.Fatalf("%v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("a program that exits at once must not make the user wait for the timeout")
	}
	if h.Status()["listening"].(bool) || camStatus(h, h.ID())["open"].(bool) {
		t.Fatal("a failed start must leave no open door and no open pairing")
	}
}

func TestBehindTheTunnelAGuesserIsCountedByItsOwnAddress(t *testing.T) {
	h := newHub(t)
	id := h.ID()
	h.Tunnel = fakeCloudflared(t, "echo https://guess-test.trycloudflare.com 1>&2\r\nping -n 60 127.0.0.1 >nul")
	port, err := h.Listen(id)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Stop("")
	call := func(from, k string) int {
		req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/c/%s?k=%s", port, id, k), nil)
		req.Header.Set("Cf-Connecting-Ip", from)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for i := 0; i <= maxFailures+2; i++ {
		call("203.0.113.7", "wrong")
	}
	secret := secretOf(h, id)
	if code := call("203.0.113.7", secret); code != 404 {
		t.Fatalf("the guesser must be turned away, got %d", code)
	}
	if code := call("198.51.100.4", secret); code != 200 {
		t.Fatalf("the real phone must not be locked out by someone else's guesses, got %d", code)
	}
}

func TestWithTheRelayNothingOpensOnThisComputer(t *testing.T) {
	h := newHub(t)
	id := h.ID()
	h.Relay = "https://camera-teste.workers.dev"
	h.Tunnel = filepath.Join(t.TempDir(), "nao-existe.exe") // even with no tunnel program at all
	if cfg := h.RxConfig(id); cfg["accepting"] != false || cfg["phoneHash"] != "" {
		t.Fatalf("before pairing the relay must refuse every phone: %v", cfg)
	}
	port, err := h.Listen(id)
	if err != nil || port != 0 {
		t.Fatalf("the relay needs no port: %d %v", port, err)
	}
	defer h.Stop("")
	st := h.Status()
	if st["mode"] != "relay" || st["listening"] != true || st["needsTunnel"] != false {
		t.Fatalf("%v", st)
	}
	if hosts := st["hosts"].([]string); len(hosts) != 1 || hosts[0] != "camera-teste.workers.dev" {
		t.Fatalf("%v", hosts)
	}
	secret, rxSecret := secretOf(h, id), rxSecretOf(h, id)
	if strings.Contains(fmt.Sprint(st), "#k=") || strings.Contains(fmt.Sprint(st), secret) || strings.Contains(fmt.Sprint(st), rxSecret) {
		t.Fatal("the state must not carry the phone's secret or the receiver's key")
	}

	// the room is the hash of a key that never leaves this computer; the phone's key is checked by its hash
	room := sha256.Sum256([]byte(rxSecret))
	wantRoom := hex.EncodeToString(room[:16])
	hash := sha256.Sum256([]byte(secret))
	cfg := h.RxConfig(id)
	if cfg["mode"] != "relay" || cfg["key"] != rxSecret || cfg["accepting"] != true || cfg["phoneHash"] != hex.EncodeToString(hash[:]) ||
		cfg["ws"] != "wss://camera-teste.workers.dev/ws?room="+wantRoom {
		t.Fatalf("%v", cfg)
	}
	urls := h.PhoneURLs(id)
	if len(urls) != 1 || urls[0] != "https://camera-teste.workers.dev/c/"+wantRoom+"#k="+secret {
		t.Fatalf("the secret must travel in the fragment: %v", urls)
	}
	if strings.Contains(urls[0], rxSecret) {
		t.Fatal("the receiver's key must never be in the QR code")
	}
	if h.RelayHost() != "camera-teste.workers.dev" {
		t.Fatal(h.RelayHost())
	}

	// a new code changes only the phone's hash; stopping makes it empty (the relay then refuses everyone)
	before := cfg["phoneHash"]
	h.Revoke(id)
	if after := h.RxConfig(id); after["phoneHash"] == before || after["key"] != rxSecret {
		t.Fatalf("revoke must change the phone's hash and keep the room: %v", after)
	}
	h.Stop(id)
	if stopped := h.RxConfig(id); stopped["accepting"] != false || stopped["phoneHash"] != "" {
		t.Fatalf("%v", stopped)
	}
}

func TestTheReceiverReportsThePhoneNameWhenSignallingGoesElsewhere(t *testing.T) {
	h := newHub(t)
	id := h.ID()
	h.Send(id, RoleReceiver, "label", json.RawMessage(`"iPhone de Ana"`))
	if camStatus(h, id)["label"] != "iPhone de Ana" {
		t.Fatalf("%v", camStatus(h, id))
	}
	if got := recvNow(t, h, id, RolePhone, 0); len(got) != 0 {
		t.Fatalf("a label is for the hub: %+v", got)
	}
}

func TestTheReceiverReportsTheSizeOfThePicture(t *testing.T) {
	h := newHub(t)
	id := h.ID()
	if s := h.Size(id); s["width"] != 0 || s["height"] != 0 {
		t.Fatalf("nothing arrives yet: %v", s)
	}
	h.Send(id, RoleReceiver, "state", json.RawMessage(`"connected"`))
	h.Send(id, RoleReceiver, "size", json.RawMessage(`{"width":1080,"height":1920}`))
	if s := h.Size(id); s["width"] != 1080 || s["height"] != 1920 {
		t.Fatalf("a picture upright must give a portrait source: %v", s)
	}
	if c := camStatus(h, id); c["width"] != 1080 || c["height"] != 1920 {
		t.Fatalf("%v", c)
	}
	for _, bad := range []string{`{"width":0,"height":1920}`, `{"width":99999,"height":10}`, `"x"`, `null`, `{"width":-5,"height":100}`} {
		h.Send(id, RoleReceiver, "size", json.RawMessage(bad))
		if s := h.Size(id); s["width"] != 0 || s["height"] != 0 {
			t.Fatalf("%s must clear the size, got %v", bad, s)
		}
		h.Send(id, RoleReceiver, "size", json.RawMessage(`{"width":1280,"height":720}`))
	}
	h.Send(id, RoleReceiver, "state", json.RawMessage(`"idle"`))
	if s := h.Size(id); s["width"] != 0 {
		t.Fatalf("when the phone leaves the size goes away: %v", s)
	}
	if got := recvNow(t, h, id, RolePhone, 0); len(got) != 0 {
		t.Fatalf("size is for the hub, not for the phone: %+v", got)
	}
}

// ---- several cameras

func TestTwoCamerasNeverShareARoomASecretOrAMessage(t *testing.T) {
	h := newHub(t)
	a := h.ID()
	b := h.New()
	if b == "" || b == a {
		t.Fatalf("a second camera must be a different one: %q %q", a, b)
	}
	if secretOf(h, a) == secretOf(h, b) || rxSecretOf(h, a) == rxSecretOf(h, b) {
		t.Fatal("each camera needs its own secrets")
	}
	h.Send(a, RolePhone, "offer", json.RawMessage(`{"sdp":"a"}`))
	if got := recvNow(t, h, b, RoleReceiver, 0); len(got) != 0 {
		t.Fatalf("a message of camera A reached camera B: %+v", got)
	}
	h.Send(b, RoleReceiver, "state", json.RawMessage(`"connected"`))
	if camStatus(h, a)["connected"].(bool) || !camStatus(h, b)["connected"].(bool) {
		t.Fatalf("A=%v B=%v", camStatus(h, a), camStatus(h, b))
	}
	if h.Authorized(a, secretOf(h, b)) || h.Authorized(b, secretOf(h, a)) {
		t.Fatal("a phone's code must open only its own camera")
	}
	if h.ReceiverURL(a) == h.ReceiverURL(b) {
		t.Fatal("each OBS source needs its own receiver address")
	}
	// the relay's rooms differ too
	h.Relay = "https://camera-teste.workers.dev"
	h.Listen(a)
	h.Listen(b)
	defer h.Stop("")
	if h.RxConfig(a)["ws"] == h.RxConfig(b)["ws"] || h.PhoneURLs(a)[0] == h.PhoneURLs(b)[0] {
		t.Fatal("each camera needs its own relay room")
	}
}

func TestStoppingOneCameraKeepsTheOthersOpen(t *testing.T) {
	h := newHub(t)
	a := h.ID()
	b := h.New()
	h.Relay = "https://camera-teste.workers.dev"
	h.Listen(a)
	h.Listen(b)
	h.Stop(a)
	if h.RxConfig(a)["accepting"] != false || h.RxConfig(b)["accepting"] != true {
		t.Fatalf("A=%v B=%v", h.RxConfig(a), h.RxConfig(b))
	}
	if !h.Status()["listening"].(bool) {
		t.Fatal("B is still open")
	}
	h.Stop(b)
	if h.Status()["listening"].(bool) {
		t.Fatal("with no pairing open nothing is listening")
	}
}

func TestASourceWithoutACameraTakesTheOldOneOnceThenGetsNewOnes(t *testing.T) {
	dir := t.TempDir()
	old := `{"id":"aaaaaaaaaaaaaaaa","secret":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","port":8766}` // before there were several cameras
	if err := os.WriteFile(filepath.Join(dir, "pairing.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	h := Open(dir, fstest.MapFS{}, 8765)
	if h.ID() != "aaaaaaaaaaaaaaaa" || rxSecretOf(h, "aaaaaaaaaaaaaaaa") == "" {
		t.Fatal("the old pairing must become the first camera, with a receiver key")
	}
	first := h.Claim("")
	if first != "aaaaaaaaaaaaaaaa" {
		t.Fatalf("the first source without a camera takes the camera that already existed: %q", first)
	}
	second := h.Claim("")
	if second == "" || second == first {
		t.Fatalf("the second source without a camera must get a new one, not fight for the first: %q", second)
	}
	if again := h.Claim(first); again != first {
		t.Fatalf("a source that remembers its camera gets it back: %q", again)
	}
	if third := h.Claim("desconhecida"); third == "" || third == first || third == second {
		t.Fatalf("an id this computer does not know is replaced by a fresh camera: %q", third)
	}
	// it is all kept across a restart
	again := Open(dir, fstest.MapFS{}, 8765)
	if len(again.Status()["cameras"].([]Obj)) != 3 {
		t.Fatalf("%v", again.Status()["cameras"])
	}
	if _, err := os.Stat(filepath.Join(dir, "cameras.json")); err != nil {
		t.Fatal("cameras.json must be written")
	}
}

func TestRemovingACameraOnlyWhenNothingUsesIt(t *testing.T) {
	h := newHub(t)
	a := h.ID()
	b := h.New()
	if h.Remove(b) == nil {
		t.Fatal("a camera just handed to a source is reserved: it must not be removed")
	}
	h.mu.Lock()
	h.camLocked(b).reservedUntil = time.Time{}
	h.mu.Unlock()
	h.Relay = "https://camera-teste.workers.dev"
	h.Listen(b)
	if h.Remove(b) == nil {
		t.Fatal("a camera whose pairing is open must not be removed")
	}
	h.Stop(b)
	h.Send(b, RoleReceiver, "state", json.RawMessage(`"idle"`)) // the receiver was just seen
	if h.Remove(b) == nil {
		t.Fatal("a camera whose receiver is alive must not be removed")
	}
	h.mu.Lock()
	h.camLocked(b).rxSeen = time.Now().Add(-time.Minute)
	h.mu.Unlock()
	if err := h.Remove(b); err != nil {
		t.Fatal(err)
	}
	if h.KnownID(b) || !h.KnownID(a) {
		t.Fatal("only the removed camera goes away")
	}
	if h.Remove("desconhecida") == nil {
		t.Fatal("an unknown camera cannot be removed")
	}
}

func TestThereIsALimitOfCameras(t *testing.T) {
	h := newHub(t)
	for i := 1; i < maxCameras; i++ {
		if h.New() == "" {
			t.Fatalf("camera %d should fit", i+1)
		}
	}
	if h.New() != "" {
		t.Fatal("past the limit no more cameras are made")
	}
}
