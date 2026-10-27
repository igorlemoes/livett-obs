// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// controller is the LiveTT controller as a single executable.
//
//	controller [serve] [--root DIR] [--port N] [--background] [--exit-with-obs]   run the panel server (default)
//	controller watchdog [--root DIR]                            the independent dead-man switch (spawned by serve)
//	controller mcp [--root DIR] [--port N]                      MCP server on stdio for an assistant
//	controller assist [--root DIR] [--cloudflared EXE] [--minutes N] [--port N]
//	                                                            temporary remote sign-in (only with the user's consent)
//	controller version                                          print the version
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"tiktoklive/internal/assets"
	"tiktoklive/internal/assist"
	"tiktoklive/internal/camera"
	"tiktoklive/internal/chrome"
	"tiktoklive/internal/controller"
	"tiktoklive/internal/mcp"
	"tiktoklive/internal/proc"
	"tiktoklive/internal/server"
	"tiktoklive/internal/session"
	"tiktoklive/internal/telemetry"
	"tiktoklive/internal/vault"
	"tiktoklive/internal/watchdog"
)

// version is set by the build (-ldflags "-X main.version=...").
var version = "dev"

func main() {
	args := os.Args[1:]
	mode := "serve"
	if len(args) > 0 && (args[0] == "serve" || args[0] == "watchdog" || args[0] == "mcp" || args[0] == "assist" || args[0] == "version") {
		mode, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet(mode, flag.ExitOnError)
	exe, _ := os.Executable()
	root := fs.String("root", filepath.Dir(exe), "folder that holds runtime/ (journal, traces, heartbeat)")
	port := fs.Int("port", server.DefaultPort, "loopback port of the panel")
	background := fs.Bool("background", false, "do not open a browser window")
	exitWithObs := fs.Bool("exit-with-obs", false, "serve: quit by itself once OBS is gone and nothing is on air (set by the plugin)")
	cloudflared := fs.String("cloudflared", "", "assist: path of cloudflared.exe for a temporary public tunnel (omit for loopback only)")
	minutes := fs.Float64("minutes", 15, "assist: how long the temporary link lives")
	fs.Parse(args)
	if mode == "version" {
		fmt.Println("livett-controller " + version)
		return
	}
	absRoot, err := filepath.Abs(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.MkdirAll(filepath.Join(absRoot, "runtime"), 0o755)
	switch mode {
	case "watchdog":
		runWatchdog(absRoot)
	case "mcp":
		runMCP(absRoot, *port)
	case "assist":
		runAssist(absRoot, *port, *cloudflared, *minutes)
	default:
		runServe(absRoot, *port, *background, *exitWithObs)
	}
}

func runServe(root string, port int, background, exitWithObs bool) {
	sess := session.New(root)
	bridge := &controller.SessionBrowser{S: sess, PanelJS: assets.PanelJS()}
	var ctrl *controller.Controller
	tel := telemetry.New(telemetry.Config{
		Root:    root,
		PageWS:  sess.PageWebSocket,
		ObsOpen: func(ctx context.Context) (telemetry.ObsCaller, error) { return controller.OpenObs(ctx) },
		Browser: bridge.Call,
	})
	hub := camera.Open(filepath.Join(root, "runtime", "camera"), assets.Camera(), port) // the phone as a wireless camera
	hub.BindAddr = os.Getenv("TIKTOK_CAMERA_BIND")                                      // diagnostics: keep the phone door on one address (127.0.0.1 opens nothing to the network)
	hub.Plain = os.Getenv("TIKTOK_CAMERA_PLAIN") == "1"
	hub.Relay = relayAddress()
	ctrl = controller.New(controller.Options{
		Root:          root,
		Browser:       bridge,
		Recorder:      tel,
		IMGuard:       true,
		SpawnWatchdog: spawnWatchdog,
		Vault:         vault.Open(root, vault.DPAPI{}), // saved accounts, sealed with the Windows user's key
		Camera:        hub,
	})
	srv := server.New(ctrl, assets.Web(), port)
	srv.Camera, srv.CameraWeb = hub, assets.Camera()
	srv.OnError = func(path string, err error) { logFailure(root, path, err) }
	listener, err := srv.Listen()
	if err != nil {
		// Single-instance launch: no second controller/journal owner is created.
		if !background {
			openBrowser(fmt.Sprintf("http://127.0.0.1:%d/", port))
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	exitLog := filepath.Join(root, "runtime", "controller-exit.log")
	defer func() {
		reason := "encerrado normalmente"
		if r := recover(); r != nil {
			reason = fmt.Sprintf("pânico: %v\n%s", r, debug.Stack())
		} else if ctx.Err() != nil {
			reason = "sinal de encerramento"
		}
		if f, err := os.OpenFile(exitLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintf(f, "%s pid=%d %s\n", time.Now().Format(time.RFC3339), os.Getpid(), reason)
			f.Close()
		}
	}()
	go ctrl.Monitor(ctx)
	if exitWithObs {
		go quitWhenObsIsGone(ctx, stop, ctrl)
	}
	go ctrl.Heartbeat(ctx)
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	os.WriteFile(filepath.Join(root, "runtime", "panel-url.txt"), []byte(url), 0o644)
	if !background {
		openBrowser(url)
	}
	httpServer := &http.Server{Handler: srv.Handler()}
	go func() { <-ctx.Done(); httpServer.Close() }()
	httpServer.Serve(listener)
	sess.Stop()
}

// quitWhenObsIsGone ends the controller shortly after OBS closes, but only while nothing is on air or undecided.
// Leaving it running after OBS exits would keep a stale copy (and its files locked) for the next OBS and for updates.
func quitWhenObsIsGone(ctx context.Context, stop context.CancelFunc, ctrl *controller.Controller) {
	var goneSince time.Time
	for {
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return
		}
		if proc.Running("obs64.exe") || !ctrl.Idle() {
			goneSince = time.Time{}
			continue
		}
		if goneSince.IsZero() {
			goneSince = time.Now()
		}
		if time.Since(goneSince) >= 20*time.Second {
			stop()
			return
		}
	}
}

// logFailure appends the real reason of a failure the panel could only describe generically to runtime/errors.log
// (the error text comes from this program, never from a cookie, token or stream key; it is bounded and trimmed).
func logFailure(root, path string, err error) {
	file := filepath.Join(root, "runtime", "errors.log")
	if info, statErr := os.Stat(file); statErr == nil && info.Size() > 256<<10 {
		os.Remove(file)
	}
	f, openErr := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if openErr != nil {
		return
	}
	defer f.Close()
	msg := strings.Map(func(r rune) rune {
		if r < 32 {
			return ' '
		}
		return r
	}, err.Error())
	if len(msg) > 300 {
		msg = msg[:300]
	}
	fmt.Fprintf(f, "%s %s %s\n", time.Now().Format("2006-01-02 15:04:05"), path, msg)
}
func openBrowser(url string) {
	exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

// spawnWatchdog starts the dead-man switch as a separate, detached process, once per broadcast.
func spawnWatchdog(root string) error {
	pidFile := filepath.Join(root, "runtime", "watchdog.pid")
	if raw, err := os.ReadFile(pidFile); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && watchdog.PidAlive(pid) {
			return nil // already watching
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "watchdog", "--root", root)
	// DETACHED_PROCESS | CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP: it must outlive this process.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x08000000 | 0x00000200, HideWindow: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func runWatchdog(root string) {
	pidFile := filepath.Join(root, "runtime", "watchdog.pid")
	os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o644)
	defer os.Remove(pidFile)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dog := watchdog.New(root,
		func(ctx context.Context) (watchdog.Obs, error) { return controller.OpenObs(ctx) },
		func(ctx context.Context) (watchdog.Browser, error) {
			if _, _, running := chrome.DebugEndpoint(chrome.PersistentProfile()); !running {
				return nil, nil // never start a browser from here
			}
			s := session.New(root)
			connectCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			if err := s.Connect(connectCtx, assets.PanelJS()); err != nil {
				return nil, err
			}
			return s, nil
		})
	dog.Run(ctx)
}

func runMCP(root string, port int) {
	exe, _ := os.Executable()
	panel := mcp.NewPanelClient(fmt.Sprintf("http://127.0.0.1:%d", port), func() error {
		cmd := exec.Command(exe, "serve", "--background", "--root", root, "--port", strconv.Itoa(port))
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x08000000 | 0x00000200, HideWindow: true}
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	})
	tools := &mcp.Tools{Panel: panel, Policy: mcp.LoadPolicy(root),
		OpenObs: func(ctx context.Context) (mcp.ObsAPI, error) { return controller.OpenObs(ctx) }}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	mcp.NewServer(tools).Serve(ctx, os.Stdin, os.Stdout)
}

// runAssist serves the temporary remote sign-in until the deadline or "Encerrar"; stdout gets "READY <link>".
func runAssist(root string, port int, cloudflared string, minutes float64) {
	if port == server.DefaultPort {
		port = 8790 // the panel owns 8765
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sess := session.New(root)
	link := filepath.Join(root, "runtime", "remote-assist-link.txt")
	err := assist.Run(ctx, sess, minutes, port, cloudflared, link, func(l string) { fmt.Println("READY " + l) })
	if err != nil {
		fmt.Fprintln(os.Stderr, "assist:", err)
		os.Exit(1)
	}
	fmt.Println("STOPPED")
}

// relayAddress is where the phone camera's signalling relay lives: TIKTOK_CAMERA_RELAY (https://...) or the project's own.
func relayAddress() string {
	address := strings.TrimRight(os.Getenv("TIKTOK_CAMERA_RELAY"), "/")
	if address == "" {
		address = camera.DefaultRelay
	}
	if !strings.HasPrefix(address, "https://") && !strings.HasPrefix(address, "http://127.0.0.1") {
		return "" // a relay must be reached over HTTPS (a local one only for tests)
	}
	return address
}
