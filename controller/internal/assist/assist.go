// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// Package assist is the temporary remote sign-in assistant: it lets the
// user finish the TikTok sign-in from a phone when the PC cannot be reached any other way.
//
// It serves screenshots of the TikTok sign-in tab of the managed Chrome and forwards the OWN taps and typing of the
// user to it. The program (and any assistant) never sees or stores what is typed: text goes straight to the browser
// and is never logged. Safeguards: a long random secret in the link; only the sign-in tab can be driven, with
// navigation limited to two fixed pages; it stops by itself after the deadline or when the user presses "Encerrar";
// the optional tunnel (a cloudflared quick tunnel) exists only while this runs. Use it only with the explicit consent
// of the user, every time.
package assist

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Login is the slice of the browser session the assistant drives (session.Session implements it).
type Login interface {
	LoginShot(ctx context.Context) ([]byte, error)
	LoginTap(ctx context.Context, fx, fy float64) error
	LoginText(ctx context.Context, text string) error
	LoginKey(ctx context.Context, key string) error
	LoginGo(ctx context.Context, where string) error
}

// Server is the HTTP side, protected by the secret in every path.
type Server struct {
	Login    Login
	Token    string
	Deadline time.Time
	mu       sync.Mutex
	stopped  bool
	done     chan struct{}
}

func NewServer(login Login, minutes float64) *Server {
	b := make([]byte, 24)
	rand.Read(b)
	return &Server{Login: login, Token: base64.RawURLEncoding.EncodeToString(b),
		Deadline: time.Now().Add(time.Duration(minutes * float64(time.Minute))), done: make(chan struct{})}
}

// Done is closed when the session ends (deadline or "Encerrar").
func (s *Server) Done() <-chan struct{} { return s.done }

func (s *Server) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.stopped = true
		close(s.done)
	}
}

func (s *Server) expired() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped || time.Now().After(s.Deadline)
}

func reply(w http.ResponseWriter, status int, body []byte, kind string) {
	h := w.Header()
	h.Set("Content-Type", kind)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Robots-Tag", "noindex")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(status)
	w.Write(body)
}

func firstSegment(path string) string {
	path = strings.TrimPrefix(path, "/")
	if i := strings.Index(path, "/"); i >= 0 {
		return path[:i]
	}
	return path
}

// Handler serves only /<token>/... and never logs paths or bodies.
func (s *Server) Handler() http.Handler {
	prefix := "/" + s.Token
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if subtle.ConstantTimeCompare([]byte(firstSegment(path)), []byte(s.Token)) != 1 {
			reply(w, http.StatusNotFound, nil, "text/plain")
			return
		}
		if s.expired() {
			reply(w, http.StatusGone, []byte(`{"error":"expired"}`), "application/json")
			return
		}
		route := strings.TrimPrefix(path, prefix)
		if route == "" {
			route = "/"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		switch {
		case r.Method == http.MethodGet && route == "/":
			reply(w, http.StatusOK, []byte(pageHTML), "text/html; charset=utf-8")
		case r.Method == http.MethodGet && route == "/shot":
			shot, err := s.Login.LoginShot(ctx)
			if err != nil {
				reply(w, http.StatusServiceUnavailable, nil, "text/plain")
				return
			}
			reply(w, http.StatusOK, shot, "image/jpeg")
		case r.Method == http.MethodPost:
			s.post(ctx, w, r, route)
		default:
			reply(w, http.StatusNotFound, nil, "text/plain")
		}
	})
}

func (s *Server) post(ctx context.Context, w http.ResponseWriter, r *http.Request, route string) {
	bad := func() { reply(w, http.StatusBadRequest, []byte(`{"error":"bad request"}`), "application/json") }
	var data map[string]any
	if r.ContentLength > 0 && r.ContentLength <= 4096 {
		if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&data) != nil {
			bad()
			return
		}
	} else if r.ContentLength > 4096 {
		bad()
		return
	}
	if data == nil {
		data = map[string]any{}
	}
	var err error
	switch route {
	case "/tap":
		fx, ok1 := data["fx"].(float64)
		fy, ok2 := data["fy"].(float64)
		if !ok1 || !ok2 || fx < 0 || fx > 1 || fy < 0 || fy > 1 {
			bad()
			return
		}
		err = s.Login.LoginTap(ctx, fx, fy)
	case "/text":
		text, ok := data["text"].(string)
		if !ok || text == "" || len([]rune(text)) > 64 {
			bad()
			return
		}
		err = s.Login.LoginText(ctx, text)
	case "/key":
		key, _ := data["key"].(string)
		if key != "Enter" && key != "Backspace" && key != "Tab" {
			bad()
			return
		}
		err = s.Login.LoginKey(ctx, key)
	case "/go":
		to, _ := data["to"].(string)
		if to != "login" && to != "qr" {
			bad()
			return
		}
		err = s.Login.LoginGo(ctx, to)
	case "/stop":
		s.stop()
	default:
		reply(w, http.StatusNotFound, nil, "text/plain")
		return
	}
	if err != nil {
		reply(w, http.StatusServiceUnavailable, []byte(`{"error":"browser"}`), "application/json")
		return
	}
	reply(w, http.StatusOK, []byte(`{"ok":true}`), "application/json")
}

var tunnelURL = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

// StartTunnel runs a cloudflared quick tunnel (no account) to the local port and returns its public https base URL.
func StartTunnel(ctx context.Context, exe string, port int) (*exec.Cmd, string, error) {
	abs, err := filepath.Abs(exe)
	if err != nil {
		return nil, "", err
	}
	cmd := exec.CommandContext(ctx, abs, "tunnel", "--no-autoupdate", "--url", fmt.Sprintf("http://127.0.0.1:%d", port))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, "", err
	}
	if err := cmd.Start(); err != nil {
		return nil, "", err
	}
	found := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			if m := tunnelURL.FindString(scanner.Text()); m != "" {
				select {
				case found <- m:
				default:
				}
			}
		}
	}()
	select {
	case u := <-found:
		return cmd, u, nil
	case <-time.After(30 * time.Second):
	case <-ctx.Done():
	}
	cmd.Process.Kill()
	return nil, "", errors.New("o túnel não ficou pronto")
}

// Run serves until the deadline, "Encerrar" or ctx ends. cloudflared may be empty (loopback only). The link is written
// to linkFile (deleted at the end) and handed to ready.
func Run(ctx context.Context, login Login, minutes float64, port int, cloudflared, linkFile string, ready func(link string)) error {
	srv := NewServer(login, minutes)
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	actual := listener.Addr().(*net.TCPAddr).Port
	httpServer := &http.Server{Handler: srv.Handler()}
	go httpServer.Serve(listener)
	defer httpServer.Close()
	base := fmt.Sprintf("http://127.0.0.1:%d", actual)
	if cloudflared != "" {
		cmd, public, err := StartTunnel(ctx, cloudflared, actual)
		if err != nil {
			return err
		}
		defer cmd.Process.Kill()
		base = public
	}
	link := base + "/" + srv.Token + "/"
	if linkFile != "" {
		os.MkdirAll(filepath.Dir(linkFile), 0o755)
		os.WriteFile(linkFile, []byte(link), 0o600)
		defer os.Remove(linkFile)
	}
	ready(link)
	timer := time.NewTimer(time.Until(srv.Deadline))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-srv.Done():
	case <-ctx.Done():
	}
	srv.stop()
	return nil
}
