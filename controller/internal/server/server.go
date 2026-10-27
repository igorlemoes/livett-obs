// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// Package server is the loopback-only HTTP server of the OBS dock. It serves the
// panel and the read-only live view, proxies viewer photos, and exposes the controller's operations to POST
// requests that carry the per-process token and a same-origin Origin header.
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"time"

	"tiktoklive/internal/avatar"
	"tiktoklive/internal/camera"
	"tiktoklive/internal/controller"
	"tiktoklive/internal/session"
)

// DefaultPort is where the dock lives (the OBS panels and the plugin point at it).
const DefaultPort = 8765

type Obj = map[string]any

// API is what the server needs from the controller (*controller.Controller implements it).
type API interface {
	Snapshot() Obj
	Connect(ctx context.Context) (Obj, error)
	SetContext(ctx context.Context, text string) (Obj, error)
	StartNative(ctx context.Context, req controller.StartRequest) (controller.NativeTarget, error)
	OpenStartDialog(ctx context.Context) (Obj, error)
	AssistantInfo() Obj
	UseAccount(ctx context.Context, id string) (Obj, error)
	AddAccount(ctx context.Context) (Obj, error)
	RemoveAccount(ctx context.Context, id string) (Obj, error)
	Logout(ctx context.Context) (Obj, error)
	End(ctx context.Context) (Obj, error)
	Start(ctx context.Context, req controller.StartRequest) (Obj, error)
	SetSchedule(ctx context.Context, minutes any) (Obj, error)
	SetAutoEnd(enabled bool) Obj
	SaveAboutMe(ctx context.Context, text string) (Obj, error)
	SendChat(ctx context.Context, req controller.ChatRequest) (Obj, error)
	SetAutopilot(thanks, answers *bool) (Obj, error)
}

// CameraAPI is the wireless camera part of the controller; the panel gets it when the controller offers it.
type CameraAPI interface {
	CameraPair(ctx context.Context, id string) (Obj, error)
	CameraEnsure() Obj
	CameraClaim(id string) (Obj, error)
	CameraStop(id string) Obj
	CameraRevoke(id string) (Obj, error)
	CameraRemove(id string) (Obj, error)
	CameraAddSource(ctx context.Context) (Obj, error)
}

type Server struct {
	API       API
	Web       fs.FS // index.html, app.js, style.css, live.html, live.js, live.css
	Avatars   *avatar.Proxy
	Camera    *camera.Hub // wireless camera signalling (nil turns the routes off)
	CameraWeb fs.FS       // the pages of the wireless camera
	Port      int
	Token     string
	OpTimeout time.Duration // a slow operation keeps running; the client is told to refresh, not to repeat
	// OnError is told about failures the panel can only describe generically (the real reason is for the log).
	OnError func(path string, err error)
}

func New(api API, web fs.FS, port int) *Server {
	b := make([]byte, 32)
	rand.Read(b)
	return &Server{API: api, Web: web, Avatars: avatar.New(), Port: port, Token: base64.RawURLEncoding.EncodeToString(b),
		OpTimeout: 90 * time.Second}
}

func (s *Server) hostOK(host string) bool {
	return host == fmt.Sprintf("127.0.0.1:%d", s.Port) || host == fmt.Sprintf("localhost:%d", s.Port)
}

// ValidRequest: a mutating request must come from the loopback origin of this same server and carry the token.
func (s *Server) ValidRequest(host, origin, token string) bool {
	return s.hostOK(host) && origin == "http://"+host && subtle.ConstantTimeCompare([]byte(token), []byte(s.Token)) == 1 && token != ""
}

const csp = "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self'; img-src 'self' data: " +
	"https://*.tiktokcdn.com https://*.tiktokcdn-us.com https://*.tiktokcdn-eu.com https://*.ibytedtos.com; connect-src 'self'; frame-ancestors 'self'"

func (s *Server) send(w http.ResponseWriter, status int, body any, contentType string) {
	var payload []byte
	switch b := body.(type) {
	case []byte:
		payload = b
	default:
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.Encode(b)
		payload = bytes.TrimRight(buf.Bytes(), "\n")
		if contentType == "" {
			contentType = "application/json; charset=utf-8"
		}
	}
	if contentType == "" {
		contentType = "application/json; charset=utf-8"
	}
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", s.policy())
	h.Set("Content-Length", strconv.Itoa(len(payload)))
	w.WriteHeader(status)
	w.Write(payload)
}

func (s *Server) fail(w http.ResponseWriter, status int, msg string) {
	s.send(w, status, Obj{"error": msg}, "")
}

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.get(w, r)
		case http.MethodPost:
			s.post(w, r)
		default:
			s.fail(w, http.StatusNotImplemented, "Método não suportado.")
		}
	})
}

func (s *Server) file(w http.ResponseWriter, name, contentType string) {
	data, err := fs.ReadFile(s.Web, name)
	if err != nil {
		s.fail(w, http.StatusNotFound, "Não encontrado.")
		return
	}
	s.send(w, http.StatusOK, data, contentType)
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	if !s.hostOK(r.Host) {
		s.fail(w, http.StatusForbidden, "Host inválido.")
		return
	}
	switch p := r.URL.Path; p {
	case "/":
		data, err := fs.ReadFile(s.Web, "index.html")
		if err != nil {
			s.fail(w, http.StatusNotFound, "Não encontrado.")
			return
		}
		s.send(w, http.StatusOK, bytes.ReplaceAll(data, []byte("__PANEL_TOKEN__"), []byte(s.Token)), "text/html; charset=utf-8")
	case "/app.js", "/live.js":
		s.file(w, p[1:], "text/javascript; charset=utf-8")
	case "/style.css", "/live.css":
		s.file(w, p[1:], "text/css; charset=utf-8")
	case "/live":
		// Read-only floating view (stats + chat); needs no token because it never writes.
		s.file(w, "live.html", "text/html; charset=utf-8")
	case "/avatar":
		// Photo proxy for the native overlay (Qt inside OBS has no HTTPS). Read-only; TikTok image CDNs only.
		data, kind, err := s.Avatars.Fetch(r.URL.Query().Get("u"))
		if err != nil {
			s.fail(w, http.StatusNotFound, "Foto indisponível.")
			return
		}
		h := w.Header()
		h.Set("Content-Type", kind)
		h.Set("Cache-Control", "private, max-age=3600")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	case "/camera/rx", "/camera/rx.js", "/camera/rx.css", "/camera/rx/recv", "/camera/rx/config", "/camera/size", "/camera/qr.svg", "/camera/qr.txt":
		if !s.cameraGet(w, r) {
			s.fail(w, http.StatusNotFound, "Não encontrado.")
		}
	case "/api/state":
		s.send(w, http.StatusOK, s.API.Snapshot(), "")
	case "/api/session":
		// Same token that "/" embeds in the panel page, for the "Ao vivo" view and the native overlay (both loopback,
		// same host check as every GET). Another site cannot read it: no CORS headers are sent.
		s.send(w, http.StatusOK, Obj{"token": s.Token}, "")
	default:
		s.fail(w, http.StatusNotFound, "Não encontrado.")
	}
}

func boolPtr(v any) (*bool, error) {
	if v == nil {
		return nil, nil
	}
	b, ok := v.(bool)
	if !ok {
		return nil, errors.New("Valor inválido.")
	}
	return &b, nil
}

func str(v any) string { s, _ := v.(string); return s }

func (s *Server) post(w http.ResponseWriter, r *http.Request) {
	if s.cameraPost(w, r) {
		return
	}
	if !s.ValidRequest(r.Host, r.Header.Get("Origin"), r.Header.Get("X-Panel-Token")) {
		s.fail(w, http.StatusForbidden, "Pedido local não autorizado.")
		return
	}
	size, err := strconv.Atoi(r.Header.Get("Content-Length"))
	if err != nil || size <= 0 || size > 8192 {
		s.fail(w, http.StatusBadRequest, "Pedido inválido.")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, int64(size)))
	var data Obj
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err != nil || dec.Decode(&data) != nil || data == nil {
		s.fail(w, http.StatusBadRequest, "Pedido inválido.")
		return
	}

	ctx := context.Background() // a client that gives up must not cancel a publication in progress
	var op func() (Obj, error)
	switch r.URL.Path {
	case "/api/context":
		op = func() (Obj, error) { return s.API.SetContext(ctx, str(data["text"])) }
	case "/api/assistant-info":
		op = func() (Obj, error) { return s.API.AssistantInfo(), nil }
	case "/api/account/use":
		op = func() (Obj, error) { return s.API.UseAccount(ctx, str(data["id"])) }
	case "/api/account/add":
		op = func() (Obj, error) { return s.API.AddAccount(ctx) }
	case "/api/account/remove":
		op = func() (Obj, error) { return s.API.RemoveAccount(ctx, str(data["id"])) }
	case "/api/account/logout":
		op = func() (Obj, error) { return s.API.Logout(ctx) }
	case "/api/connect":
		op = func() (Obj, error) { return s.API.Connect(ctx) }
	case "/api/end":
		op = func() (Obj, error) { return s.API.End(ctx) }
	case "/api/autopilot":
		thanks, e1 := boolPtr(data["thanks"])
		answers, e2 := boolPtr(data["answers"])
		op = func() (Obj, error) {
			if e1 != nil || e2 != nil {
				return nil, &controller.PanelError{Msg: "Valor inválido."}
			}
			return s.API.SetAutopilot(thanks, answers)
		}
	case "/api/chat":
		req := controller.ChatRequest{Text: data["text"], Via: "", Context: nil}
		if n, ok := data["replyToN"].(float64); ok && n == float64(int(n)) {
			req.ReplyTo = int(n)
		} else if data["replyToN"] != nil {
			req.ReplyTo = data["replyToN"] // not an integer: the controller refuses it
		}
		switch via := data["via"].(type) {
		case nil:
		case string:
			req.Via = via
		default:
			req.Via = "?" // an unknown origin is refused by the controller
		}
		if c := data["context"]; c != nil && c != "" && c != false && c != float64(0) {
			text := fmt.Sprint(c)
			if r := []rune(text); len(r) > 200 {
				text = string(r[:200])
			}
			req.Context = Obj{"comment": text}
		}
		op = func() (Obj, error) { return s.API.SendChat(ctx, req) }
	case "/api/auto-end":
		op = func() (Obj, error) {
			enabled, ok := data["enabled"].(bool)
			if !ok {
				return nil, &controller.PanelError{Msg: "Valor inválido."}
			}
			return s.API.SetAutoEnd(enabled), nil
		}
	case "/api/about":
		op = func() (Obj, error) {
			text, ok := data["text"].(string)
			if !ok {
				return nil, &controller.PanelError{Msg: "Digite o texto do Sobre mim."}
			}
			return s.API.SaveAboutMe(ctx, text)
		}
	case "/api/start":
		req := controller.StartRequest{Title: str(data["title"]), Scene: str(data["scene"]), Confirmed: data["confirmed"] == true,
			MaxMinutes: data["maxMinutes"]}
		if d, ok := data["productDetails"].(string); ok {
			req.ProductDetails = &d
		}
		op = func() (Obj, error) { return s.API.Start(ctx, req) }
	case "/api/start-native":
		// Called by the plugin after the user confirmed in its own window: the OBS button is starting a LIVE.
		req := controller.StartRequest{Title: str(data["title"]), Scene: str(data["scene"]), Confirmed: data["confirmed"] == true,
			MaxMinutes: data["maxMinutes"]}
		if d, ok := data["productDetails"].(string); ok {
			req.ProductDetails = &d
		}
		op = func() (Obj, error) {
			target, err := s.API.StartNative(ctx, req)
			if err != nil {
				return nil, err
			}
			return Obj{"server": target.Server(), "key": target.Key()}, nil
		}
	case "/api/start-dialog":
		op = func() (Obj, error) { return s.API.OpenStartDialog(ctx) }
	case "/api/camera/pair", "/api/camera/ensure", "/api/camera/claim", "/api/camera/stop", "/api/camera/revoke", "/api/camera/remove", "/api/camera/add-source":
		op = s.cameraOp(ctx, r.URL.Path, data)
		if op == nil {
			s.fail(w, http.StatusNotFound, "Não encontrado.")
			return
		}
	case "/api/schedule":
		op = func() (Obj, error) { return s.API.SetSchedule(ctx, data["minutes"]) }
	default:
		s.fail(w, http.StatusNotFound, "Não encontrado.")
		return
	}

	type outcome struct {
		result Obj
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		defer func() {
			if recover() != nil {
				done <- outcome{err: errors.New("panic")}
			}
		}()
		result, err := op()
		done <- outcome{result, err}
	}()
	select {
	case out := <-done:
		if out.err == nil {
			s.send(w, http.StatusOK, out.result, "")
			return
		}
		var pe *controller.PanelError
		var se *session.Error
		if errors.As(out.err, &pe) || errors.As(out.err, &se) {
			s.fail(w, http.StatusConflict, out.err.Error())
			return
		}
		if s.OnError != nil {
			s.OnError(r.URL.Path, out.err)
		}
		s.fail(w, http.StatusConflict, "Não foi possível concluir. Confira OBS, navegador e o estado da LIVE antes de repetir.")
	case <-time.After(s.OpTimeout):
		// Do not cancel or repeat a publication with uncertain outcome.
		s.fail(w, http.StatusGatewayTimeout, "A operação continua em andamento. Atualize o estado; não repita a criação.")
	}
}

// Listen binds the loopback port. A second instance gets an error and must not create a second controller.
func (s *Server) Listen() (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", s.Port))
}
