// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// Package camera lets phones act as wireless cameras for OBS, with nothing installed on the phone. A phone's browser and an OBS
// Browser Source (the "receiver") speak WebRTC to each other; this package only carries their small signalling messages and,
// when there is no relay, serves the phone page. The video never passes through here.
//
// Each camera is its own pairing (an id, a receiver key, a phone code) and its own signalling room, so several phones can feed
// several OBS sources at once; every OBS source remembers the id of its camera.
//
// Two doors lead to the same Hub: the loopback panel server (the receiver page that OBS loads) and, without the relay, a
// listener for the phone (a Cloudflare tunnel or the local network). The phone door answers only to private addresses and
// needs the camera's id and secret on every request.
package camera

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Obj = map[string]any

// DefaultRelay is the signalling relay the project publishes (worker/). While it
// is empty the phone camera uses the tunnel or the local network. TIKTOK_CAMERA_RELAY overrides it.
var DefaultRelay = "https://tiktok-live-camera.tiktok-live-camera-relay.workers.dev"

// The two ends of the signalling: the OBS Browser Source and the phone's page.
const (
	RoleReceiver = "rx"
	RolePhone    = "phone"
)

const (
	maxMessage   = 64 * 1024
	maxQueued    = 300
	maxCameras   = 8
	receiverLive = 8 * time.Second  // the receiver polls at least every ~20 s while idle, but reports itself on each call
	reservation  = 30 * time.Second // a camera handed to a source that is still loading is not handed to another
)

// Message is one signalling message addressed to the other end. Seq grows per destination.
type Message struct {
	Seq  int             `json:"seq"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// pairing is what is kept on disk about one camera.
type pairing struct {
	ID       string `json:"id"`        // names the camera; the receiver page's address carries it (loopback only)
	Name     string `json:"name"`      // "Câmera 1", for the panel
	Secret   string `json:"secret"`    // proves a phone may feed it (in the QR code)
	RxSecret string `json:"rx_secret"` // proves the receiver is this computer's: the relay's room is its hash (never leaves loopback)
	Claimed  bool   `json:"claimed"`   // some OBS source has taken it (a camera nobody has taken can be adopted by an older source)
}

type store struct {
	Port    int       `json:"port"` // last port the phone door used, tried first so a saved bookmark keeps working
	Cameras []pairing `json:"cameras"`
}

// cam is a camera and what is going on with it right now.
type cam struct {
	pairing
	open          bool                 // the user opened its pairing: phones are accepted
	reservedUntil time.Time            // handed to a source that has not started yet
	queue         map[string][]Message // by destination role
	seq           map[string]int
	state         string // what the receiver last reported: "connected" or "idle"
	label         string // what the phone called itself in its last hello
	rxSeen        time.Time
	phoneSeen     time.Time
	rxWaiting     int // long polls of the receiver in progress: it is there even while nothing is said
	width, height int // size of the picture that arrives (0 = none), as the receiver sees it
}

func (c *cam) receiverLive() bool { return c.rxWaiting > 0 || time.Since(c.rxSeen) < receiverLive }

type Hub struct {
	Dir       string // where cameras.json and the certificate live (runtime/camera)
	Web       fs.FS  // phone.html, phone.js, rx.html, rx.js, camera.css
	PanelPort int    // loopback panel port, for the receiver's address
	// BindAddr is where the phone door listens ("" = every address of the local network). Tests and diagnostics set
	// 127.0.0.1 so nothing is opened to the network.
	BindAddr string
	// Tunnel is the cloudflared program ("" = look for it).
	Tunnel string
	// Relay is the base address of the signalling relay (a Cloudflare Worker, see worker/), such as https://x.workers.dev. When set the
	// phone opens the relay's page and nothing is opened on this computer at all. "" = tunnel or local network.
	Relay string
	// Plain serves the phone door without TLS, and only when BindAddr is the loopback: a diagnostic for driving the page from a
	// browser that refuses self-signed certificates. It never applies to the local network.
	Plain bool

	listenMu  sync.Mutex // one Listen or Stop at a time (a tunnel takes seconds to start; mu is never held that long)
	mu        sync.Mutex
	cams      []*cam
	port      int
	wake      chan struct{} // closed (and replaced) whenever a message arrives
	exe       string        // cloudflared as last looked for
	exeLooked time.Time
	lan       *lanServer
	attempts  map[string]*attemptWindow
}

// Open loads the saved cameras. A single pairing from before there were several becomes the first camera.
func Open(dir string, web fs.FS, panelPort int) *Hub {
	h := &Hub{Dir: dir, Web: web, PanelPort: panelPort, wake: make(chan struct{}), attempts: map[string]*attemptWindow{}}
	var saved store
	if raw, err := os.ReadFile(filepath.Join(dir, "cameras.json")); err == nil {
		json.Unmarshal(raw, &saved)
	} else if raw, err := os.ReadFile(filepath.Join(dir, "pairing.json")); err == nil {
		var old struct {
			pairing
			Port int `json:"port"`
		}
		if json.Unmarshal(raw, &old) == nil && old.ID != "" && old.Secret != "" {
			old.Name, old.Claimed = "Câmera 1", false
			if old.RxSecret == "" { // a pairing made before the relay existed
				old.RxSecret = randomHex(16)
			}
			saved = store{Port: old.Port, Cameras: []pairing{old.pairing}}
		}
	}
	h.port = saved.Port
	for _, p := range saved.Cameras {
		if p.ID != "" && p.Secret != "" && p.RxSecret != "" {
			h.cams = append(h.cams, h.newCam(p))
		}
	}
	if len(saved.Cameras) > 0 {
		h.mu.Lock()
		h.saveLocked()
		h.mu.Unlock()
	}
	return h
}

func (h *Hub) newCam(p pairing) *cam {
	return &cam{pairing: p, queue: map[string][]Message{}, seq: map[string]int{}, state: "idle"}
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (h *Hub) saveLocked() {
	if len(h.cams) == 0 {
		return
	}
	saved := store{Port: h.port}
	for _, c := range h.cams {
		saved.Cameras = append(saved.Cameras, c.pairing)
	}
	os.MkdirAll(h.Dir, 0o755)
	raw, _ := json.Marshal(saved)
	os.WriteFile(filepath.Join(h.Dir, "cameras.json"), raw, 0o600)
}

// camLocked finds a camera by id; an empty id means the first one.
func (h *Hub) camLocked(id string) *cam {
	if id == "" {
		if len(h.cams) > 0 {
			return h.cams[0]
		}
		return nil
	}
	for _, c := range h.cams {
		if subtle.ConstantTimeCompare([]byte(id), []byte(c.ID)) == 1 {
			return c
		}
	}
	return nil
}

// newLocked makes a camera ("" with an error when there are already too many).
func (h *Hub) newLocked() *cam {
	if len(h.cams) >= maxCameras {
		return nil
	}
	taken := map[string]bool{}
	for _, c := range h.cams {
		taken[c.Name] = true
	}
	name := ""
	for n := 1; ; n++ {
		if name = fmt.Sprintf("Câmera %d", n); !taken[name] {
			break
		}
	}
	c := h.newCam(pairing{ID: randomHex(8), Name: name, Secret: randomHex(16), RxSecret: randomHex(16)})
	h.cams = append(h.cams, c)
	h.saveLocked()
	return c
}

// Ensure makes sure there is at least one camera and returns the first one's id.
func (h *Hub) Ensure() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.cams) == 0 {
		h.newLocked()
	}
	return h.cams[0].ID
}

// New makes another camera and returns its id ("" when the limit is reached).
func (h *Hub) New() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.newLocked()
	if c == nil {
		return ""
	}
	c.Claimed, c.reservedUntil = true, time.Now().Add(reservation)
	h.saveLocked()
	return c.ID
}

// Claim hands a starting OBS source its camera: the one it names (id), else a camera nobody has taken yet (the one that existed
// before sources had ids), else a new one. It returns "" only when there are too many cameras.
func (h *Hub) Claim(id string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	take := func(c *cam) string {
		c.reservedUntil = time.Now().Add(reservation)
		if !c.Claimed {
			c.Claimed = true
			h.saveLocked()
		}
		return c.ID
	}
	if id != "" {
		if c := h.camLocked(id); c != nil {
			return take(c)
		}
	}
	for _, c := range h.cams {
		if !c.Claimed && time.Now().After(c.reservedUntil) && !c.receiverLive() {
			return take(c)
		}
	}
	if c := h.newLocked(); c != nil {
		return take(c)
	}
	return ""
}

// Remove deletes a camera that nothing is using (its source is gone and no pairing is open).
func (h *Hub) Remove(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, c := range h.cams {
		if c.ID != id {
			continue
		}
		if c.open || c.receiverLive() || time.Now().Before(c.reservedUntil) {
			return errors.New("esta câmera está em uso: apague a fonte no OBS e pare o pareamento antes")
		}
		h.cams = append(h.cams[:i:i], h.cams[i+1:]...)
		h.saveLocked()
		if len(h.cams) == 0 {
			os.Remove(filepath.Join(h.Dir, "cameras.json"))
		}
		return nil
	}
	return errors.New("câmera desconhecida")
}

// Revoke gives a camera's phone door a new secret: the phone that was paired stops working and a new QR code is needed. The
// receiver's address (the OBS source) keeps working.
func (h *Hub) Revoke(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.camLocked(id)
	if c == nil {
		return errors.New("câmera desconhecida")
	}
	c.Secret = randomHex(16)
	c.state, c.label, c.width, c.height = "idle", "", 0, 0
	c.queue = map[string][]Message{}
	h.saveLocked()
	h.broadcastLocked()
	return nil
}

func (h *Hub) broadcastLocked() {
	close(h.wake)
	h.wake = make(chan struct{})
}

// ID is the first camera's id ("" when there is none).
func (h *Hub) ID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.cams) == 0 {
		return ""
	}
	return h.cams[0].ID
}

// KnownID reports whether id is a camera's id (constant time).
func (h *Hub) KnownID(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return id != "" && h.camLocked(id) != nil
}

// Authorized reports whether id and secret are a camera's (constant time).
func (h *Hub) Authorized(id, secret string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.camLocked(id)
	if id == "" || c == nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(secret), []byte(c.Secret)) == 1
}

// ReceiverURL is the address a camera's OBS Browser Source loads (loopback, plain HTTP: OBS's browser needs no certificate).
func (h *Hub) ReceiverURL(id string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.camLocked(id)
	if c == nil {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d/camera/rx?c=%s", h.PanelPort, c.ID)
}

func cleanLabel(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	r := []rune(s)
	if len(r) > 40 {
		r = r[:40]
	}
	out := make([]rune, 0, len(r))
	for _, c := range r {
		if c >= ' ' && c != 0x7f {
			out = append(out, c)
		}
	}
	return string(out)
}

// Send delivers a camera's message from one end to the other. Messages of the receiver that only tell the hub something
// (state, size, label) update the status instead.
func (h *Hub) Send(id, from, typ string, data json.RawMessage) error {
	if from != RoleReceiver && from != RolePhone {
		return errors.New("papel desconhecido")
	}
	if len(data) > maxMessage || len(typ) == 0 || len(typ) > 16 {
		return errors.New("mensagem inválida")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.camLocked(id)
	if id == "" || c == nil {
		return errors.New("câmera desconhecida")
	}
	now := time.Now()
	if from == RoleReceiver {
		c.rxSeen = now
		switch typ {
		case "size": // the picture's size (null when there is none)
			var size struct{ Width, Height int }
			c.width, c.height = 0, 0
			if json.Unmarshal(data, &size) == nil && size.Width >= 16 && size.Width <= 8192 && size.Height >= 16 && size.Height <= 8192 {
				c.width, c.height = size.Width, size.Height
			}
			return nil
		case "label": // the phone's name, reported by the receiver when the signalling runs through the relay
			c.label = cleanLabel(data)
			return nil
		case "state":
			var s string
			if json.Unmarshal(data, &s) == nil && (s == "connected" || s == "idle") {
				c.state = s
				if s == "idle" {
					c.width, c.height = 0, 0
				}
			}
			return nil
		}
	} else {
		c.phoneSeen = now
		if typ == "hello" {
			// a new negotiation: whatever was queued belongs to an older one
			c.label = cleanLabel(data)
			c.queue = map[string][]Message{}
		}
	}
	to := RolePhone
	if from == RolePhone {
		to = RoleReceiver
	}
	c.seq[to]++
	q := append(c.queue[to], Message{Seq: c.seq[to], Type: typ, Data: data})
	if len(q) > maxQueued {
		q = q[len(q)-maxQueued:]
	}
	c.queue[to] = q
	h.broadcastLocked()
	return nil
}

// Recv returns a camera's messages for role after seq `after`, waiting up to `wait` when there are none. after < 0 only
// reports the latest seq (so a page that has just opened ignores what was said before it came). last is the newest seq
// of the role's queue.
func (h *Hub) Recv(ctx context.Context, id, role string, after int, wait time.Duration) (messages []Message, last int, err error) {
	if role != RoleReceiver && role != RolePhone {
		return nil, 0, errors.New("papel desconhecido")
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	if role == RoleReceiver && after >= 0 {
		h.mu.Lock()
		if c := h.camLocked(id); c != nil {
			c.rxWaiting++
		}
		h.mu.Unlock()
		defer func() {
			h.mu.Lock()
			if c := h.camLocked(id); c != nil {
				c.rxWaiting--
				c.rxSeen = time.Now()
			}
			h.mu.Unlock()
		}()
	}
	for {
		h.mu.Lock()
		c := h.camLocked(id)
		if id == "" || c == nil {
			h.mu.Unlock()
			return nil, 0, errors.New("câmera desconhecida")
		}
		if role == RoleReceiver {
			c.rxSeen = time.Now()
		} else {
			c.phoneSeen = time.Now()
		}
		last = c.seq[role]
		if after < 0 {
			h.mu.Unlock()
			return []Message{}, last, nil
		}
		for _, m := range c.queue[role] {
			if m.Seq > after {
				messages = append(messages, m)
			}
		}
		wake := h.wake
		h.mu.Unlock()
		if len(messages) > 0 {
			return messages, last, nil
		}
		select {
		case <-wake:
		case <-deadline.C:
			return []Message{}, last, nil
		case <-ctx.Done():
			return []Message{}, last, ctx.Err()
		}
	}
}

func (h *Hub) anyOpenLocked() bool {
	for _, c := range h.cams {
		if c.open {
			return true
		}
	}
	return false
}

// Status is what the panel shows (never a secret).
func (h *Hub) Status() Obj {
	h.mu.Lock()
	listening := h.lan != nil && h.anyOpenLocked()
	port := 0
	if listening {
		port = h.lan.port
	}
	mode := ""
	if h.lan != nil {
		mode = "lan"
		if h.lan.relay {
			mode = "relay"
		} else if h.lan.publicURL != "" {
			mode = "tunnel"
		}
	}
	cameras := []Obj{}
	anyConnected := false
	for _, c := range h.cams {
		connected := c.receiverLive() && c.state == "connected"
		anyConnected = anyConnected || connected
		cameras = append(cameras, Obj{"id": c.ID, "name": c.Name, "open": c.open, "receiver": c.receiverLive(), "connected": connected,
			"label": c.label, "width": c.width, "height": c.height,
			"sourceURL": fmt.Sprintf("http://127.0.0.1:%d/camera/rx?c=%s", h.PanelPort, c.ID)})
	}
	paired := len(h.cams) > 0
	h.mu.Unlock()
	status := Obj{"paired": paired, "listening": listening, "port": port, "connected": anyConnected, "mode": mode, "cameras": cameras,
		// without cloudflared the phone gets a certificate made here, which an iPhone does not accept for the camera
		"needsTunnel": h.BindAddr == "" && h.Relay == "" && mode != "tunnel" && h.cloudflaredCached() == ""}
	if listening {
		status["hosts"] = h.hostList() // the addresses only: the pairing secret stays out of the state
	}
	return status
}

// PhoneURLs are the addresses a phone can use for a camera whose pairing is open (on the local network: one per usable local
// address, best first).
func (h *Hub) PhoneURLs(id string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.camLocked(id)
	if c == nil || h.lan == nil || !c.open {
		return []string{}
	}
	if h.lan.relay {
		// the secret goes in the fragment: the browser never sends it with the page request
		return []string{fmt.Sprintf("%s/c/%s#k=%s", h.Relay, h.relayRoomLocked(c), c.Secret)}
	}
	path := fmt.Sprintf("/c/%s?k=%s", c.ID, c.Secret)
	if h.lan.publicURL != "" {
		return []string{h.lan.publicURL + path}
	}
	urls := []string{}
	for _, ip := range h.addresses() {
		urls = append(urls, fmt.Sprintf("https://%s:%d%s", ip, h.lan.port, path))
	}
	return urls
}

func (h *Hub) hostList() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lan != nil && h.lan.relay {
		return []string{strings.TrimPrefix(strings.TrimPrefix(h.Relay, "https://"), "http://")}
	}
	if h.lan != nil && h.lan.publicURL != "" {
		return []string{strings.TrimPrefix(h.lan.publicURL, "https://")}
	}
	hosts := []string{}
	for _, ip := range h.addresses() {
		hosts = append(hosts, ip.String())
	}
	return hosts
}

func (h *Hub) relayRoomLocked(c *cam) string {
	sum := sha256.Sum256([]byte(c.RxSecret))
	return hex.EncodeToString(sum[:16])
}

func phoneHash(c *cam) string {
	sum := sha256.Sum256([]byte(c.Secret))
	return hex.EncodeToString(sum[:])
}

func (h *Hub) relayActive() bool { return h.Relay != "" && h.BindAddr == "" }

// RelayHost is the relay's host name ("" when there is none), for the pages' content policy.
func (h *Hub) RelayHost() string {
	if !h.relayActive() {
		return ""
	}
	return strings.TrimPrefix(strings.TrimPrefix(h.Relay, "https://"), "http://")
}

// RxConfig tells a camera's receiver page how to reach the phone: through this hub ("local") or through the relay.
// It is only for the loopback receiver, which proves it is the right one by knowing the camera's id.
func (h *Hub) RxConfig(id string) Obj {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.camLocked(id)
	if id == "" || c == nil || !h.relayActive() {
		return Obj{"mode": "local"}
	}
	wsBase := strings.Replace(strings.Replace(h.Relay, "https://", "wss://", 1), "http://", "ws://", 1)
	accepting := c.open && h.lan != nil && h.lan.relay
	hash := ""
	if accepting {
		hash = phoneHash(c) // an empty hash makes the relay refuse every phone
	}
	return Obj{"mode": "relay", "ws": wsBase + "/ws?room=" + h.relayRoomLocked(c), "key": c.RxSecret,
		"phoneHash": hash, "accepting": accepting}
}

// TouchReceiver notes that a camera's receiver page is alive (in relay mode it does not poll this hub, it reads its config).
func (h *Hub) TouchReceiver(id string) {
	h.mu.Lock()
	if c := h.camLocked(id); id != "" && c != nil {
		c.rxSeen = time.Now()
	}
	h.mu.Unlock()
}

// RelayIsPlain reports a relay reached without TLS (only a local one, for tests).
func (h *Hub) RelayIsPlain() bool { return strings.HasPrefix(h.Relay, "http://") }

// Size is the size of the picture that arrives from a camera's phone (0, 0 when none), for its OBS source to take its shape.
func (h *Hub) Size(id string) Obj {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.camLocked(id)
	if id == "" || c == nil || c.state != "connected" {
		return Obj{"width": 0, "height": 0}
	}
	return Obj{"width": c.width, "height": c.height}
}

// Discard drops a camera made a moment ago whose OBS source could not be created.
func (h *Hub) Discard(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, c := range h.cams {
		if c.ID == id && !c.open {
			h.cams = append(h.cams[:i:i], h.cams[i+1:]...)
			h.saveLocked()
			return
		}
	}
}
