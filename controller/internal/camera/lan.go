// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package camera

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	firstPort    = 8766
	lastPort     = 8775
	maxWait      = 25 * time.Second
	maxFailures  = 20 // wrong secrets per address per minute before it is turned away
	failureReset = time.Minute
)

type lanServer struct {
	port      int
	publicURL string // https address of the tunnel ("" when the door is on the local network)
	relay     bool   // the phone goes through the relay: nothing is open on this computer
	tunnel    *exec.Cmd
	server    *http.Server
}

type attemptWindow struct {
	count int
	since time.Time
}

// Listen opens a camera's pairing ("" = the first camera, made if there is none) and returns the door's port; the door is shared
// by all cameras and is opened only once. With the relay nothing is opened on this computer. With cloudflared installed the
// door stays on this computer (127.0.0.1) and is reached through a Cloudflare quick tunnel with a trusted HTTPS address. Without
// either, the door listens on the local network with a certificate made here (HTTPS, first free port from 8766): the phone warns
// about it and some phones (iPhone) then refuse the camera. The first time, Windows may ask whether to allow the network.
func (h *Hub) Listen(id string) (int, error) {
	h.listenMu.Lock()
	defer h.listenMu.Unlock()
	h.mu.Lock()
	c := h.camLocked(id)
	if c == nil && id == "" {
		c = h.newLocked()
	}
	if c == nil {
		h.mu.Unlock()
		return 0, errors.New("câmera desconhecida ou limite de câmeras atingido")
	}
	wasOpen := c.open
	c.open = true
	if h.lan != nil {
		port := h.lan.port
		h.mu.Unlock()
		return port, nil
	}
	h.mu.Unlock()
	port, err := h.openDoor()
	if err != nil {
		h.mu.Lock()
		c.open = wasOpen
		h.mu.Unlock()
		return 0, err
	}
	return port, nil
}

func (h *Hub) openDoor() (int, error) {
	if h.relayActive() { // the relay replaces any door on this computer
		h.mu.Lock()
		h.lan = &lanServer{relay: true}
		h.mu.Unlock()
		return 0, nil
	}
	if h.BindAddr == "" { // a set address is a diagnostic: never start a tunnel then
		if exe := h.cloudflaredPath(); exe != "" {
			return h.listenTunnel(exe)
		}
	}
	return h.listenLAN()
}

func (h *Hub) newServer() *http.Server {
	return &http.Server{
		Handler:           http.HandlerFunc(h.serveLAN),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0), // a phone that rejects the certificate is not an error worth a line
	}
}

func (h *Hub) listenTunnel(exe string) (int, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, errors.New("não consegui abrir a porta local da câmera do celular")
	}
	port := listener.Addr().(*net.TCPAddr).Port
	srv := h.newServer()
	go srv.Serve(listener)
	cmd, public, err := startTunnel(exe, port)
	if err != nil {
		srv.Close()
		return 0, err
	}
	h.mu.Lock()
	h.lan = &lanServer{port: port, server: srv, tunnel: cmd, publicURL: public}
	h.mu.Unlock()
	return port, nil
}

func (h *Hub) listenLAN() (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cert, err := loadOrCreateCert(h.Dir, h.addresses())
	if err != nil {
		return 0, fmt.Errorf("não consegui criar o certificado: %w", err)
	}
	var listener net.Listener
	candidates := []int{}
	if h.port != 0 {
		candidates = append(candidates, h.port)
	}
	for p := firstPort; p <= lastPort; p++ {
		candidates = append(candidates, p)
	}
	for _, p := range candidates {
		if l, err := net.Listen("tcp4", h.bind()+":"+strconv.Itoa(p)); err == nil {
			listener = l
			break
		}
	}
	if listener == nil {
		return 0, errors.New("não achei uma porta livre para a câmera do celular")
	}
	port := listener.Addr().(*net.TCPAddr).Port
	srv := h.newServer()
	if h.Plain && net.ParseIP(h.bind()).IsLoopback() {
		go srv.Serve(listener)
	} else {
		go srv.Serve(tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}))
	}
	h.lan = &lanServer{port: port, server: srv}
	if h.port != port {
		h.port = port
		h.saveLocked()
	}
	return port, nil
}

// Stop closes a camera's pairing ("" = every camera). When no pairing is left open the door (and the tunnel, if there is one) closes.
func (h *Hub) Stop(id string) {
	h.listenMu.Lock()
	defer h.listenMu.Unlock()
	h.mu.Lock()
	for _, c := range h.cams {
		if id == "" || c.ID == id {
			c.open = false
			c.state, c.width, c.height = "idle", 0, 0
		}
	}
	if h.anyOpenLocked() {
		h.mu.Unlock()
		return
	}
	lan := h.lan
	h.lan = nil
	h.mu.Unlock()
	if lan != nil {
		if lan.server != nil {
			lan.server.Close()
		}
		if lan.tunnel != nil && lan.tunnel.Process != nil {
			lan.tunnel.Process.Kill()
			lan.tunnel.Wait()
		}
	}
}

func privateRemote(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// turnedAway counts a wrong secret against the caller's address and reports whether it has had too many.
func (h *Hub) turnedAway(host string, failed bool) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	w := h.attempts[host]
	if w == nil || time.Since(w.since) > failureReset {
		w = &attemptWindow{since: time.Now()}
		h.attempts[host] = w
	}
	if failed {
		w.count++
	}
	return w.count > maxFailures
}

func notFound(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "", http.StatusNotFound)
}

var phoneCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; media-src 'self' blob:; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func (h *Hub) static(w http.ResponseWriter, name, contentType, csp string) {
	data, err := fs.ReadFile(h.Web, name)
	if err != nil {
		notFound(w)
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", contentType)
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Referrer-Policy", "no-referrer")
	if csp != "" {
		hd.Set("Content-Security-Policy", csp)
	}
	hd.Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
}

func (h *Hub) serveLAN(w http.ResponseWriter, r *http.Request) {
	key := h.clientKey(r)
	if !privateRemote(r.RemoteAddr) || h.turnedAway(key, false) {
		notFound(w)
		return
	}
	switch p := r.URL.Path; {
	case p == "/s/phone.js" && r.Method == http.MethodGet:
		h.static(w, "phone.js", "text/javascript; charset=utf-8", "")
	case p == "/s/camera.css" && r.Method == http.MethodGet:
		h.static(w, "camera.css", "text/css; charset=utf-8", "")
	case strings.HasPrefix(p, "/c/"):
		parts := strings.Split(strings.TrimPrefix(p, "/c/"), "/")
		if !h.Authorized(parts[0], r.URL.Query().Get("k")) {
			h.turnedAway(key, true)
			notFound(w)
			return
		}
		sub := ""
		if len(parts) > 1 {
			sub = parts[1]
		}
		switch {
		case sub == "" && r.Method == http.MethodGet:
			h.static(w, "phone.html", "text/html; charset=utf-8", phoneCSP)
		case sub == "recv" && r.Method == http.MethodGet:
			h.ServeRecv(w, r, parts[0], RolePhone)
		case sub == "send" && r.Method == http.MethodPost:
			h.ServeSend(w, r, parts[0], RolePhone)
		default:
			notFound(w)
		}
	default:
		notFound(w)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	raw, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	w.Write(raw)
}

// ServeRecv answers a long poll for the messages addressed to role (the caller has already checked who may ask).
func (h *Hub) ServeRecv(w http.ResponseWriter, r *http.Request, id, role string) {
	after, err := strconv.Atoi(r.URL.Query().Get("after"))
	if err != nil {
		after = 0
	}
	wait := time.Duration(0)
	if secs, err := strconv.Atoi(r.URL.Query().Get("wait")); err == nil && secs > 0 {
		wait = min(time.Duration(secs)*time.Second, maxWait)
	}
	messages, last, err := h.Recv(r.Context(), id, role, after, wait)
	if err != nil {
		return // the caller went away
	}
	writeJSON(w, http.StatusOK, Obj{"messages": messages, "last": last})
}

// ServeSend takes one message from role (the caller has already checked who may send).
func (h *Hub) ServeSend(w http.ResponseWriter, r *http.Request, id, role string) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxMessage+1024))
	var body struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err != nil || len(raw) > maxMessage+1024 || json.Unmarshal(raw, &body) != nil {
		writeJSON(w, http.StatusBadRequest, Obj{"error": "mensagem inválida"})
		return
	}
	if err := h.Send(id, role, body.Type, body.Data); err != nil {
		writeJSON(w, http.StatusBadRequest, Obj{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, Obj{"ok": true})
}

// Serve is the phone door's handler, for tests.
func (h *Hub) Serve() http.Handler { return http.HandlerFunc(h.serveLAN) }

func (h *Hub) bind() string {
	if h.BindAddr != "" {
		return h.BindAddr
	}
	return "0.0.0.0"
}

// addresses are the ones a phone is told to use: this computer's local addresses, or the single BindAddr when set.
func (h *Hub) addresses() []net.IP {
	if h.BindAddr != "" {
		if ip := net.ParseIP(h.BindAddr); ip != nil {
			return []net.IP{ip}
		}
	}
	return localAddresses()
}

// clientKey names the caller for the wrong-secret count: its own address, or, behind the tunnel (where every request comes
// from this computer), the address Cloudflare reports, so one guesser cannot lock the real phone out.
func (h *Hub) clientKey(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	h.mu.Lock()
	tunnel := h.lan != nil && h.lan.publicURL != ""
	h.mu.Unlock()
	if tunnel {
		if from := net.ParseIP(host); from != nil && from.IsLoopback() {
			if ip := net.ParseIP(r.Header.Get("Cf-Connecting-Ip")); ip != nil {
				return ip.String()
			}
		}
	}
	return host
}
