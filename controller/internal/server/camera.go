// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package server

import (
	"context"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

	"tiktoklive/internal/camera"
)

// Routes of the wireless camera on the loopback panel server: the receiver page that an OBS Browser Source loads, its
// signalling, and the pairing QR code. The phone itself never comes here; it uses the hub's own listener.

func (s *Server) cameraFile(w http.ResponseWriter, name, contentType string) {
	data, err := fs.ReadFile(s.CameraWeb, name)
	if err != nil {
		s.fail(w, http.StatusNotFound, "Não encontrado.")
		return
	}
	s.send(w, http.StatusOK, data, contentType)
}

// cameraGet serves /camera/... and reports whether it handled the path.
func (s *Server) cameraGet(w http.ResponseWriter, r *http.Request) bool {
	if s.Camera == nil || s.CameraWeb == nil {
		return false
	}
	switch r.URL.Path {
	case "/camera/rx":
		s.cameraFile(w, "rx.html", "text/html; charset=utf-8")
	case "/camera/rx.js":
		s.cameraFile(w, "rx.js", "text/javascript; charset=utf-8")
	case "/camera/rx.css":
		s.cameraFile(w, "rx.css", "text/css; charset=utf-8")
	case "/camera/size":
		s.send(w, http.StatusOK, s.Camera.Size(r.URL.Query().Get("c")), "")
	case "/camera/rx/config":
		if !s.Camera.KnownID(r.URL.Query().Get("c")) {
			s.fail(w, http.StatusNotFound, "Não encontrado.")
			return true
		}
		s.Camera.TouchReceiver(r.URL.Query().Get("c"))
		s.send(w, http.StatusOK, s.Camera.RxConfig(r.URL.Query().Get("c")), "")
	case "/camera/rx/recv":
		if !s.Camera.KnownID(r.URL.Query().Get("c")) {
			s.fail(w, http.StatusNotFound, "Não encontrado.")
			return true
		}
		s.Camera.ServeRecv(w, r, r.URL.Query().Get("c"), camera.RoleReceiver)
	case "/camera/qr.txt":
		urls := s.Camera.PhoneURLs(r.URL.Query().Get("c"))
		i, _ := strconv.Atoi(r.URL.Query().Get("i"))
		if i < 0 || i >= len(urls) {
			s.fail(w, http.StatusNotFound, "Sem endereço para parear.")
			return true
		}
		modules, err := camera.Encode(urls[i])
		if err != nil {
			s.fail(w, http.StatusInternalServerError, "Não consegui gerar o QR code.")
			return true
		}
		rows := make([]string, len(modules))
		for y, row := range modules {
			b := make([]byte, len(row))
			for x, dark := range row {
				b[x] = '0'
				if dark {
					b[x] = '1'
				}
			}
			rows[y] = string(b)
		}
		s.send(w, http.StatusOK, Obj{"rows": rows}, "")
	case "/camera/qr.svg":
		urls := s.Camera.PhoneURLs(r.URL.Query().Get("c"))
		i, _ := strconv.Atoi(r.URL.Query().Get("i"))
		if i < 0 || i >= len(urls) {
			s.fail(w, http.StatusNotFound, "Sem endereço para parear.")
			return true
		}
		svg, err := camera.SVG(urls[i])
		if err != nil {
			s.fail(w, http.StatusInternalServerError, "Não consegui gerar o QR code.")
			return true
		}
		s.send(w, http.StatusOK, svg, "image/svg+xml")
	default:
		return false
	}
	return true
}

// cameraPost handles the receiver's own POST (it is a page inside OBS, so it has no panel token: it must come from this
// server's own origin and know the camera's id) and reports whether it handled the path.
func (s *Server) cameraPost(w http.ResponseWriter, r *http.Request) bool {
	if s.Camera == nil || r.URL.Path != "/camera/rx/send" {
		return false
	}
	if !s.hostOK(r.Host) || r.Header.Get("Origin") != "http://"+r.Host || !s.Camera.KnownID(r.URL.Query().Get("c")) {
		s.fail(w, http.StatusForbidden, "Pedido local não autorizado.")
		return true
	}
	s.Camera.ServeSend(w, r, r.URL.Query().Get("c"), camera.RoleReceiver)
	return true
}

// cameraOp maps the panel's camera buttons to the controller (nil when the controller does not offer them).
func (s *Server) cameraOp(ctx context.Context, path string, data Obj) func() (Obj, error) {
	api, ok := s.API.(CameraAPI)
	if !ok {
		return nil
	}
	id := str(data["id"]) // the camera the button is about ("" = the first one)
	switch path {
	case "/api/camera/pair":
		return func() (Obj, error) { return api.CameraPair(ctx, id) }
	case "/api/camera/ensure":
		return func() (Obj, error) { return api.CameraEnsure(), nil }
	case "/api/camera/claim":
		return func() (Obj, error) { return api.CameraClaim(id) }
	case "/api/camera/stop":
		return func() (Obj, error) { return api.CameraStop(id), nil }
	case "/api/camera/revoke":
		return func() (Obj, error) { return api.CameraRevoke(id) }
	case "/api/camera/remove":
		return func() (Obj, error) { return api.CameraRemove(id) }
	case "/api/camera/add-source":
		return func() (Obj, error) { return api.CameraAddSource(ctx) }
	}
	return nil
}

// policy is the content policy of every page this server serves; the receiver page may also reach the signalling relay.
func (s *Server) policy() string {
	if s.Camera != nil {
		if host := s.Camera.RelayHost(); host != "" {
			scheme := "wss://"
			if s.Camera.RelayIsPlain() {
				scheme = "ws://" // a local relay used only for tests
			}
			return strings.Replace(csp, "connect-src 'self';", "connect-src 'self' "+scheme+host+";", 1)
		}
	}
	return csp
}
