// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tiktoklive/internal/camera"
	"tiktoklive/internal/guard"
	"tiktoklive/internal/obs"
	"tiktoklive/internal/room"
	"tiktoklive/internal/vault"
)

type Phase string

const (
	Disconnected Phase = "disconnected"
	Login        Phase = "login"
	Ready        Phase = "ready"
	Creating     Phase = "creating"
	Live         Phase = "live"
	Ending       Phase = "ending"
	Closed       Phase = "closed"
	Uncertain    Phase = "uncertain"
	Recovery     Phase = "recovery"
)

// Options wires the controller. Tests pass fakes; the real app passes a session adapter and leaves ObsFactory nil.
type Options struct {
	Root       string
	Browser    Browser
	ObsFactory func(ctx context.Context) (Obs, error)
	Recorder   Recorder // optional passive telemetry
	// IMGuard turns on the automatic end driven by the host's IM channel (on in the real app).
	IMGuard            bool
	NoAutoEndOnWarning bool
	// SpawnWatchdog starts the dead-man switch as a separate process (nil in tests).
	SpawnWatchdog func(root string) error
	// Vault keeps the signed-in accounts for the account list (nil turns the list off).
	Vault *vault.Store
	// Camera is the wireless camera hub (nil turns the phone camera off).
	Camera *camera.Hub
}

type Controller struct {
	opts    Options
	root    string
	browser Browser // traced
	raw     Browser // untraced: used for the high-volume imfetch/roomstats calls
	rec     Recorder

	mu sync.Mutex // serialises operations (connect/start/end/chat/...)
	st sync.Mutex // protects everything the snapshot reads; held only briefly

	phase        Phase
	detail       string
	obsInfo      Obj
	account      Obj
	aboutMe      Obj
	binding      *Binding
	view         *view
	obs          Obs
	output       *obs.StreamOutput
	savedService Obj
	expectedProf string
	keyCleanup   bool
	failures     int
	defaults     Obj

	startedAt     time.Time
	liveTitle     string
	productInfo   string
	hasProduct    bool
	endAt         *time.Time
	autoEnd       bool
	imGuard       bool
	lastTrigger   string
	guard         *guard.Guard
	imCancel      context.CancelFunc
	imRunning     bool
	imGen         int
	wake          chan struct{} // nudges Monitor when the phase or the schedule changes
	loginCancel   context.CancelFunc
	contextDraft  string       // the AI context the next LIVE starts with (kept in runtime/context.txt)
	accounts      []Obj        // what the panel lists (no secrets)
	activeAccount string       // vault id of the account the browser holds ("" when unknown)
	pendingTicket *LoginTicket // identity from a finished sign-in window, until the account is recorded
	statsCancel   context.CancelFunc
	autopilot     map[string]bool
	autoSent      int
	autoFailures  int
	chatLog       []chatEntry

	// Tunable limits (fields so tests can adjust them,).
	AutoMinInterval  time.Duration
	AutoPerMinute    int
	AutoMaxPerLive   int
	ChatMax          int
	ChatMinInterval  time.Duration
	ChatPerMinute    int
	ChatRepeatWindow time.Duration
	StartPoll        time.Duration // how often Start looks for OBS to become active
}

func New(opts Options) *Controller {
	c := &Controller{
		opts: opts, root: opts.Root, rec: opts.Recorder, phase: Disconnected,
		detail: "Conecte o OBS e a sessão do TikTok.", obsInfo: Obj{}, account: Obj{}, view: newView(),
		autoEnd: !opts.NoAutoEndOnWarning, imGuard: opts.IMGuard, autopilot: map[string]bool{"thanks": false, "answers": false},
		AutoMinInterval: 8 * time.Second, AutoPerMinute: 4, AutoMaxPerLive: 60,
		ChatMax: 200, ChatMinInterval: 4 * time.Second, ChatPerMinute: 8, ChatRepeatWindow: 120 * time.Second,
		StartPoll: 500 * time.Millisecond,
	}
	c.wake = make(chan struct{}, 1)
	c.raw = opts.Browser
	c.browser = &tracedBrowser{inner: opts.Browser, source: "browser", emit: c.emit}
	c.load()
	c.noteWatchdog()
	c.refreshAccounts()
	c.loadContext()
	return c
}

// ---- helpers

func (c *Controller) emit(source, kind string, data Obj) {
	if c.rec != nil {
		c.rec.Event(source, kind, data)
	}
}

func (c *Controller) journalPath() string { return filepath.Join(c.root, "runtime", "broadcast.json") }

func (c *Controller) setState(phase Phase, detail string) {
	c.st.Lock()
	c.phase, c.detail = phase, detail
	c.st.Unlock()
	c.kick()
}

// kick wakes the monitor so it re-reads the phase and schedule instead of sleeping out an old delay.
func (c *Controller) kick() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Controller) setDetail(detail string) {
	c.st.Lock()
	c.detail = detail
	c.st.Unlock()
}

func (c *Controller) appendDetail(extra string) {
	c.st.Lock()
	c.detail += extra
	c.st.Unlock()
}

func (c *Controller) getPhase() Phase {
	c.st.Lock()
	defer c.st.Unlock()
	return c.phase
}

// Phase and Detail expose the current state (tests and the server).
func (c *Controller) Phase() Phase {
	return c.getPhase()
}

func (c *Controller) Detail() string {
	c.st.Lock()
	defer c.st.Unlock()
	return c.detail
}

type journal struct {
	Phase      string   `json:"phase"`
	Binding    *Binding `json:"binding"`
	Profile    string   `json:"profile"`
	KeyCleanup bool     `json:"key_cleanup_pending"`
	EndAt      *float64 `json:"end_at"`
	Format     int      `json:"format"`
}

func epoch(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

// load restores the journal: any pending phase latches into recovery so a crash can never allow a second creation.
func (c *Controller) load() {
	raw, err := os.ReadFile(c.journalPath())
	if err != nil {
		return
	}
	var j journal
	if json.Unmarshal(raw, &j) != nil {
		return
	}
	switch j.Phase {
	case "closed", "idle", "disconnected":
	default:
		c.phase = Recovery
		c.detail = "Há uma operação anterior pendente. Conecte e verifique antes de criar outra LIVE."
		if j.Binding != nil {
			b := *j.Binding
			c.binding = &b
		}
		c.expectedProf = j.Profile
	}
	c.keyCleanup = j.KeyCleanup
}

func (c *Controller) noteWatchdog() {
	path := filepath.Join(c.root, "runtime", "watchdog.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var data Obj
	if json.Unmarshal(raw, &data) != nil {
		return
	}
	c.detail = fmt.Sprintf("O watchdog encerrou a LIVE porque o painel parou (OBS: %v, sala: %v). Confira a Central de LIVE.",
		data["obs"], data["room"])
	os.Rename(path, filepath.Join(c.root, "runtime", "watchdog.last.json"))
}

// save persists the journal atomically and records the phase change. Callers hold c.mu.
func (c *Controller) save() {
	c.st.Lock()
	phase, detail, binding, profile, cleanup, endAt := c.phase, c.detail, c.binding, c.expectedProf, c.keyCleanup, c.endAt
	c.st.Unlock()
	var bobj any
	if binding != nil {
		bobj = binding.obj()
	}
	c.emit("controller", "phase", Obj{"phase": string(phase), "detail": detail, "binding": bobj})
	j := journal{Phase: string(phase), Binding: binding, Profile: profile, KeyCleanup: cleanup, Format: 1}
	if endAt != nil {
		f := epoch(*endAt)
		j.EndAt = &f
	}
	raw, _ := json.MarshalIndent(j, "", "  ")
	os.MkdirAll(filepath.Dir(c.journalPath()), 0o755)
	tmp := c.journalPath() + ".tmp"
	if os.WriteFile(tmp, raw, 0o644) == nil {
		os.Rename(tmp, c.journalPath())
	}
}

// Snapshot is the JSON-ready state shown by the panel and the MCP server.
func (c *Controller) Snapshot() Obj {
	c.st.Lock()
	defer c.st.Unlock()
	return c.snapshotLocked()
}

func (c *Controller) snapshotLocked() Obj {
	live := c.phase == Live
	liveOrEnding := c.phase == Live || c.phase == Ending
	var endAt, remaining, startedAt, title, product any
	if c.endAt != nil && live {
		endAt = epoch(*c.endAt)
		rem := time.Until(*c.endAt).Seconds()
		if rem < 0 {
			rem = 0
		}
		remaining = int64(rem + 0.5)
	}
	if liveOrEnding {
		startedAt = epoch(c.startedAt)
		title = c.liveTitle
		if c.hasProduct {
			product = c.productInfo
		}
	}
	monitor := Obj{"active": false}
	if c.rec != nil {
		monitor = c.rec.Snapshot()
	}
	var aboutMe any
	if c.aboutMe != nil {
		aboutMe = c.aboutMe
	}
	autopilot := Obj{"thanks": c.autopilot["thanks"], "answers": c.autopilot["answers"], "sent": c.autoSent}
	return Obj{
		"phase": string(c.phase), "detail": c.detail, "obs": c.obsInfo, "tiktok": c.account,
		"hasRoom":        c.binding != nil,
		"canStart":       (c.phase == Ready || c.phase == Closed) && c.obs != nil,
		"canEnd":         c.binding != nil && c.phase != Closed,
		"warningMonitor": c.imRunning, "autoEndOnWarning": c.autoEnd, "autopilot": autopilot,
		"endAt": endAt, "startedAt": startedAt, "liveTitle": title, "productDetails": product,
		"remainingSec": remaining, "view": c.viewSnapshotLocked(), "aboutMe": aboutMe, "monitor": monitor,
		"accounts": c.accountsOrEmpty(), "accountList": c.opts.Vault != nil, "context": c.contextDraft, "camera": c.cameraStatus(),
	}
}

func (c *Controller) accountsOrEmpty() []Obj {
	if c.accounts == nil {
		return []Obj{}
	}
	return c.accounts
}

// ---- OBS

func (c *Controller) connectObs(ctx context.Context) error {
	if c.obs != nil {
		c.obs.Close()
		c.obs = nil
	}
	var raw Obs
	if c.opts.ObsFactory != nil {
		var err error
		if raw, err = c.opts.ObsFactory(ctx); err != nil {
			return err
		}
	} else {
		client, err := obs.Open(ctx)
		if err != nil {
			return errObsUnreachable
		}
		raw = client
	}
	c.obs = &tracedObs{inner: raw, source: "obs", emit: c.emit}
	return nil
}

// errObsUnreachable is what the user sees when the plugin's control API (inside OBS) does not answer.
var errObsUnreachable = perr("Não consegui falar com o OBS. Abra o OBS (versão 32 ou mais nova) com o plugin LiveTT instalado.")

// OpenObs connects to the running OBS through the plugin's control API (used by the controller and the watchdog).
func OpenObs(ctx context.Context) (Obs, error) {
	client, err := obs.Open(ctx)
	if err != nil {
		return nil, errObsUnreachable
	}
	return client, nil
}
func (c *Controller) readObs(ctx context.Context) (Obj, error) {
	profile, err := c.obs.Call(ctx, "GetProfileList", nil)
	if err != nil {
		return nil, err
	}
	scene, err := c.obs.Call(ctx, "GetCurrentProgramScene", nil)
	if err != nil {
		return nil, err
	}
	status, err := c.obs.Call(ctx, "GetStreamStatus", nil)
	if err != nil {
		return nil, err
	}
	scenes, err := c.obs.Call(ctx, "GetSceneList", nil)
	if err != nil {
		return nil, err
	}
	names := []any{}
	if list, ok := scenes["scenes"].([]any); ok {
		for _, s := range list {
			names = append(names, str(asObj(s), "sceneName"))
		}
	}
	info := Obj{"profile": str(profile, "currentProfileName"), "scene": str(scene, "currentProgramSceneName"),
		"scenes": names, "active": isTrue(status["outputActive"]), "reconnecting": isTrue(status["outputReconnecting"])}
	if service, err := c.obs.Call(ctx, "GetStreamServiceSettings", nil); err == nil {
		info["serviceType"] = str(service, "streamServiceType") // tells the panel whether the OBS button is tied to this plugin
	}
	c.st.Lock()
	c.obsInfo = info
	c.st.Unlock()
	return status, nil
}

func outputBusy(status Obj) bool {
	return isTrue(status["outputActive"]) || isTrue(status["outputReconnecting"])
}

func (c *Controller) obsString(key string) string {
	c.st.Lock()
	defer c.st.Unlock()
	return str(c.obsInfo, key)
}

// ---- connect

// Connect reads OBS and the TikTok session without publishing or changing anything.
func (c *Controller) Connect(ctx context.Context) (Obj, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.getPhase() {
	case Creating, Live, Ending:
		return nil, perr("A LIVE já está sob controle do painel. Use Encerrar LIVE.")
	}
	if err := c.connectObs(ctx); err != nil {
		return nil, err
	}
	if _, err := c.readObs(ctx); err != nil {
		return nil, err
	}
	if c.getPhase() != Live {
		c.sweepStaleKey(ctx) // best effort
	}
	if err := c.browser.Connect(ctx); err != nil {
		return nil, err
	}
	c.consumeLogin(ctx) // a sign-in window that just finished: turn its ticket into a LIVE-service session
	result, err := c.browser.Call(ctx, "check", nil)
	if err != nil {
		return nil, err
	}
	account := asObj(result)
	if !isZero(account["code"]) {
		// A fresh web login only exists on .tiktok.com; copy it to the LIVE service's domain and look again.
		if syncer, ok := c.opts.Browser.(SessionSyncer); ok {
			if synced, err := syncer.SyncWebcastSession(ctx); err == nil && synced {
				c.emit("controller", "session_synced", Obj{})
				if again, err := c.browser.Call(ctx, "check", nil); err == nil {
					account = asObj(again)
				}
			}
		}
	}
	c.st.Lock()
	c.account = account
	c.st.Unlock()
	c.readAboutMe(ctx)
	if isZero(account["code"]) {
		c.recordAccount(ctx, c.takePending())
		c.browser.ReleaseLogin(ctx)
	} else {
		c.setDetail("Entre na sua conta TikTok na janela do Chrome que foi aberta e clique em Conectar novamente.")
		c.browser.ShowLogin(ctx)
	}
	c.st.Lock()
	phase, hasBinding := c.phase, c.binding != nil
	c.st.Unlock()
	switch {
	case phase != Recovery && phase != Uncertain && !hasBinding:
		if isZero(account["code"]) {
			c.setState(Ready, "Conectado. Confira a cena, o áudio e o título antes de iniciar.")
		} else {
			// The TikTok session is not valid (e.g. 20003 after it expired): not ready to publish.
			c.setState(Login, c.Detail())
			c.startLoginWatch()
		}
	case hasBinding:
		verified, err := c.browser.Call(ctx, "verify", c.binding.obj())
		if err != nil {
			return nil, err
		}
		if isTrue(asObj(verified)["ended"]) {
			c.setState(Closed, "A sala anterior está encerrada. Confira também a saída do OBS.")
			c.save()
		}
	}
	return c.Snapshot(), nil
}

func parseMinutes(v any) (*float64, error) {
	if v == nil {
		return nil, nil
	}
	f, ok := number(v)
	if !ok || f < 1 || f > 720 {
		return nil, perr("A duração máxima deve estar entre 1 e 720 minutos.")
	}
	return &f, nil
}

// SetSchedule sets/replaces/clears (nil) the automatic end of the running broadcast.
func (c *Controller) SetSchedule(ctx context.Context, minutes any) (Obj, error) {
	m, err := parseMinutes(minutes)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.getPhase() != Live {
		return nil, perr("Só é possível programar o encerramento durante uma LIVE.")
	}
	c.st.Lock()
	if m == nil {
		c.endAt = nil
	} else {
		t := time.Now().Add(time.Duration(*m * float64(time.Minute)))
		c.endAt = &t
	}
	c.st.Unlock()
	c.emit("controller", "schedule", Obj{"minutes": m})
	c.kick()
	c.save()
	return c.Snapshot(), nil
}

// SetAutoEnd chooses whether a TikTok warning ends the LIVE (a remote end signal always does).
func (c *Controller) SetAutoEnd(enabled bool) Obj {
	c.st.Lock()
	c.autoEnd = enabled
	c.st.Unlock()
	c.emit("controller", "auto_end_on_warning", Obj{"enabled": enabled})
	return c.Snapshot()
}

// ---- start

type StartRequest struct {
	Title          string
	Scene          string
	Confirmed      bool
	MaxMinutes     any // nil, or a number of minutes
	ProductDetails *string

	native *nativeHandoff // set by StartNative: the OBS button starts the output, this flow only hands over the destination
}

// Start publishes a LIVE: it is the only path that does, and it requires explicit confirmation.
func (c *Controller) Start(ctx context.Context, req StartRequest) (snapshot Obj, err error) {
	maxMinutes, err := parseMinutes(req.MaxMinutes)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !req.Confirmed {
		return nil, perr("Confirme que deseja publicar a cena e o áudio no TikTok.")
	}
	title := strings.TrimSpace(req.Title)
	if n := len([]rune(title)); n < 1 || n > 80 {
		return nil, perr("Informe um título de 1 a 80 caracteres.")
	}
	if p := c.getPhase(); (p != Ready && p != Closed) || c.obs == nil {
		return nil, perr("Conecte e resolva qualquer operação pendente antes de iniciar.")
	}
	status, err := c.readObs(ctx)
	if err != nil {
		return nil, err
	}
	if outputBusy(status) {
		return nil, perr("O OBS já está transmitindo ou reconectando. Nenhum destino foi alterado.")
	}
	if c.obsString("scene") != req.Scene {
		return nil, perr("A cena ativa mudou. Confira a prévia e tente novamente.")
	}
	checked, err := c.browser.Call(ctx, "check", nil)
	if err != nil {
		return nil, err
	}
	account := asObj(checked)
	c.defaults = nil
	if d, ok := account["defaults"].(map[string]any); ok {
		c.defaults = d
	}
	delete(account, "defaults")
	c.st.Lock()
	c.account = account
	c.st.Unlock()
	c.readAboutMe(ctx)
	if !isZero(account["code"]) || !isInt(account["liveStatus"], 4) || !isTrue(account["enabled"]) || account["banned"] != false {
		return nil, perr("O TikTok não confirmou que a conta está livre e habilitada para criar uma sala.")
	}
	if c.binding != nil {
		verified, err := c.browser.Call(ctx, "verify", c.binding.obj())
		if err != nil {
			return nil, err
		}
		if !isTrue(asObj(verified)["ended"]) {
			return nil, perr("A sala anterior ainda não tem término confirmado.")
		}
		if _, err := c.browser.Call(ctx, "drop", c.binding.obj()); err != nil {
			return nil, err
		}
	}
	c.st.Lock()
	c.binding, c.output = nil, nil
	c.expectedProf = str(c.obsInfo, "profile")
	c.st.Unlock()
	saved, err := c.obs.Call(ctx, "GetStreamServiceSettings", nil)
	if err != nil {
		return nil, err
	}
	if IsTikTokLeftover(saved) {
		saved = blankService() // a stale TikTok key is not a destination to restore
	}
	c.savedService = saved
	c.st.Lock()
	c.keyCleanup = req.native == nil // native: the key never enters the OBS settings, so there is nothing to wipe later
	c.st.Unlock()
	if req.native != nil {
		c.savedService = nil
	}
	if c.rec != nil {
		c.rec.Begin(ctx, title, c.obsInfo)
		c.emit("controller", "preflight", Obj{"obs": c.obsInfo, "account": c.account})
	}
	c.setState(Creating, "Criando a sala. Não repita o pedido se a resposta demorar.")
	c.save() // a crash/timeout cannot silently permit a second creation

	defer func() {
		r := recover()
		if err == nil && r == nil {
			return
		}
		// No automatic creation retry; close any positively identified room.
		c.setState(Uncertain, "O início não foi concluído. A criação não será repetida automaticamente.")
		c.save()
		if c.binding != nil {
			c.endLocked(context.WithoutCancel(ctx))
		} else {
			c.finishTrace()
		}
		if r != nil {
			panic(r)
		}
	}()

	fields, err := room.CreateFields(title, c.defaults)
	if err != nil {
		return nil, err
	}
	response, err := c.browser.Call(ctx, "create", Obj{"fields": fields})
	if err != nil {
		return nil, err
	}
	resp := asObj(response)
	if !isZero(resp["status_code"]) {
		if c.rejectedCleanly(ctx, resp) {
			return c.Snapshot(), nil
		}
		c.setState(Uncertain, "O pedido de criação não foi aceito. Verifique o estado antes de tentar outra vez.")
		c.save()
		return c.Snapshot(), nil
	}
	rb, err := room.BindCreatedRoom(resp)
	if err != nil {
		return nil, err
	}
	b := Binding{RoomID: rb.RoomID, PingRoomID: rb.PingRoomID, StreamID: rb.StreamID}
	c.st.Lock()
	c.binding = &b
	c.st.Unlock()
	c.save() // persist the recovery IDs before any media configuration
	if c.rec != nil {
		c.rec.AttachRoom(ctx, b, !c.imGuard)
	}
	targets, err := room.ExtractPushTargets(asObj(resp["data"]))
	if err != nil {
		return nil, err
	}
	var target *room.PushTarget
	for i := range targets {
		if targets[i].Protocol == "rtmp" {
			target = &targets[i]
			break
		}
	}
	if target == nil {
		return nil, perr("O servidor não forneceu a alternativa RTMP já validada.")
	}
	// Recheck after the server request, before modifying any output.
	status, err = c.readObs(ctx)
	if err != nil {
		return nil, err
	}
	if outputBusy(status) || c.obsString("profile") != c.expectedProf || c.obsString("scene") != req.Scene {
		return nil, perr("O estado do OBS mudou durante a criação. A sala será encerrada.")
	}
	if _, err := c.browser.Call(ctx, "ping", Obj{"binding": b.obj(), "status": 1}); err != nil {
		return nil, err
	}
	attempts := 16
	if req.native != nil {
		// The OBS button is starting the output itself, through the plugin's service: give the plugin the destination
		// and wait for OBS to connect. The key is held in the plugin's memory only.
		out := obs.NewStreamOutput(c.obs, c.expectedProf, "", "")
		out.Native = true
		c.st.Lock()
		c.output = out
		c.st.Unlock()
		req.native.deliver(NativeTarget{server: target.Server(), key: target.Key()})
		attempts = 60 // OBS connects to TikTok on its own: allow for a slow handshake
	} else {
		if _, err := c.obs.Call(ctx, "SetStreamServiceSettings", Obj{"streamServiceType": "rtmp_custom",
			"streamServiceSettings": Obj{"server": target.Server(), "key": target.Key(), "use_auth": false}}); err != nil {
			return nil, err
		}
		out := obs.NewStreamOutput(c.obs, c.expectedProf, target.Server(), target.Key())
		c.st.Lock()
		c.output = out
		c.st.Unlock()
		if err := c.output.Start(ctx); err != nil {
			return nil, err
		}
	}
	// OBS may need a moment, or show a dialog (e.g. a YouTube account linked to the profile).
	active := false
	for i := 0; i < attempts; i++ {
		status, err = c.readObs(ctx)
		if err != nil {
			return nil, err
		}
		if active = isTrue(status["outputActive"]); active {
			break
		}
		select {
		case <-time.After(c.StartPoll):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if !active {
		return nil, perr("O OBS não iniciou a saída. Feche qualquer diálogo aberto no OBS e use um perfil sem conta do YouTube vinculada.")
	}
	c.st.Lock()
	c.phase = Live
	c.startedAt = time.Now()
	c.liveTitle = title
	c.hasProduct = false
	if req.ProductDetails == nil && c.contextDraft != "" {
		draft := c.contextDraft // saved in the panel (Assistente de IA); a start from the OBS button has no field for it
		req.ProductDetails = &draft
	}
	if req.ProductDetails != nil { // facts the user gave for this LIVE; valid for this broadcast only
		details := strings.TrimSpace(*req.ProductDetails)
		if r := []rune(details); len(r) > 2000 {
			details = string(r[:2000])
		}
		c.productInfo, c.hasProduct = details, true
	}
	c.endAt = nil
	if maxMinutes != nil {
		t := time.Now().Add(time.Duration(*maxMinutes * float64(time.Minute)))
		c.endAt = &t
	}
	monitoring := "Encerramento manual."
	if c.imGuard {
		monitoring = "Vigilância automática de avisos ativa."
	}
	c.detail = "OBS transmitindo. Confira a recepção na Central de LIVE. " + monitoring
	c.failures = 0
	c.st.Unlock()
	c.kick()
	c.save()
	if _, err := c.browser.Call(ctx, "ping", Obj{"binding": b.obj(), "status": 2}); err != nil {
		return nil, err
	}
	c.st.Lock()
	c.view = newView()
	c.resetAutopilotLocked()
	c.st.Unlock()
	c.startGuard()
	c.startStats()
	c.spawnWatchdog()
	return c.Snapshot(), nil
}

// NativeTarget is where OBS must send the stream when the OBS button starts the LIVE. It never prints its key.
type NativeTarget struct{ server, key string }

// TestNativeTarget builds a target for tests of other packages.
func TestNativeTarget(server, key string) NativeTarget { return NativeTarget{server: server, key: key} }

func (t NativeTarget) Server() string   { return t.server }
func (t NativeTarget) Key() string      { return t.key }
func (t NativeTarget) String() string   { return "NativeTarget{server: " + t.server + ", key: [hidden]}" }
func (t NativeTarget) GoString() string { return t.String() }

type nativeResult struct {
	target NativeTarget
	err    error
}

// nativeHandoff carries the destination from the start flow to the caller (the plugin) as soon as the room exists.
type nativeHandoff struct {
	once sync.Once
	ch   chan nativeResult
}

func (h *nativeHandoff) deliver(t NativeTarget) {
	h.once.Do(func() { h.ch <- nativeResult{target: t} })
}
func (h *nativeHandoff) fail(err error) {
	if err != nil {
		h.once.Do(func() { h.ch <- nativeResult{err: err} })
	}
}

// StartNative is Start for a LIVE begun with the OBS "Iniciar transmissão" button (or its hotkey). It runs the same
// guarded flow, but instead of configuring and starting the OBS output it returns the destination to the plugin
// the moment the room exists; the plugin”s service gives it to OBS, which starts the output itself. The rest of the
// flow (waiting for OBS, pings, guards, watchdog) goes on in the background. Confirmation is the caller”s job: it must
// have been asked of the user (req.Confirmed).
func (c *Controller) StartNative(ctx context.Context, req StartRequest) (NativeTarget, error) {
	handoff := &nativeHandoff{ch: make(chan nativeResult, 1)}
	req.native = handoff
	go func() {
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
		_, err := c.Start(bg, req)
		handoff.fail(err) // matters only when the destination was never handed over
	}()
	select {
	case r := <-handoff.ch:
		return r.target, r.err
	case <-ctx.Done():
		return NativeTarget{}, ctx.Err() // the caller gave up; the flow rolls itself back if OBS never starts
	}
}

// rejectedCleanly: a definite server rejection (error code, no room) is safe to retry after reconciling.
func (c *Controller) rejectedCleanly(ctx context.Context, response Obj) bool {
	code, ok := number(response["status_code"])
	if !ok {
		return false
	}
	checked, err := c.browser.Call(ctx, "check", nil)
	if err != nil {
		return false
	}
	account := asObj(checked)
	if isZero(account["code"]) && isInt(account["liveStatus"], 4) && !isTrue(account["pendingCreation"]) {
		delete(account, "defaults")
		c.st.Lock()
		c.account = account
		c.st.Unlock()
		c.setState(Ready, fmt.Sprintf("O TikTok recusou a criação (código %d). Nenhuma sala foi criada; nada foi alterado no OBS.", int64(code)))
		c.save()
		c.finishTrace()
		return true
	}
	return false
}

func (c *Controller) finishTrace() {
	if c.rec != nil {
		c.rec.Finish()
	}
}

func (c *Controller) spawnWatchdog() {
	if c.opts.SpawnWatchdog == nil {
		return
	}
	if err := c.opts.SpawnWatchdog(c.root); err != nil {
		c.emit("controller", "watchdog_failed", Obj{"error": err.Error()})
		return
	}
	c.emit("controller", "watchdog_started", Obj{})
}

// ---- destination key hygiene

// restoreOrBlankService puts the previous destination back; otherwise it wipes the TikTok key. Never leaves a used key.
func (c *Controller) restoreOrBlankService(ctx context.Context) {
	if !c.keyCleanup {
		return
	}
	if c.output == nil || c.output.ValidateTarget(ctx) != nil {
		c.appendDetail(" Confira o destino de transmissão no OBS (a chave do TikTok pode ter ficado).")
		return
	}
	for _, step := range []struct {
		target Obj
		note   string
	}{{c.savedService, ""}, {blankService(), " A chave temporária do TikTok foi removida do OBS."}} {
		if step.target == nil {
			continue
		}
		if _, err := c.obs.Call(ctx, "SetStreamServiceSettings", step.target); err != nil {
			continue
		}
		c.savedService = nil
		c.st.Lock()
		c.keyCleanup = false
		c.detail += step.note
		c.st.Unlock()
		return
	}
	c.appendDetail(" Confira o destino de transmissão no OBS (a chave do TikTok pode ter ficado).")
}

// sweepStaleKey, after a crash mid-LIVE, wipes the leftover TikTok key if OBS is idle on the profile we configured.
func (c *Controller) sweepStaleKey(ctx context.Context) {
	c.st.Lock()
	phase := c.phase
	pending := c.keyCleanup
	c.st.Unlock()
	if !pending || c.obs == nil || (phase != Closed && phase != Ready && phase != Uncertain && phase != Recovery) {
		return
	}
	status, err := c.obs.Call(ctx, "GetStreamStatus", nil)
	if err != nil || outputBusy(status) {
		return
	}
	profile, err := c.obs.Call(ctx, "GetProfileList", nil)
	if err != nil {
		return
	}
	service, err := c.obs.Call(ctx, "GetStreamServiceSettings", nil)
	if err != nil {
		return
	}
	if str(profile, "currentProfileName") == c.expectedProf {
		if IsTikTokLeftover(service) {
			if _, err := c.obs.Call(ctx, "SetStreamServiceSettings", blankService()); err != nil {
				return
			}
			c.emit("controller", "stale_key_wiped", Obj{})
		}
		c.st.Lock()
		c.keyCleanup = false
		c.st.Unlock()
		c.save()
	}
}

// ---- Sobre mim

// readAboutMe is read-only: shows the account's "Sobre mim".
func (c *Controller) readAboutMe(ctx context.Context) {
	var about Obj
	reply, err := c.browser.Call(ctx, "aboutme", nil)
	if err == nil {
		r := asObj(reply)
		if texts, _ := r["texts"].([]any); isZero(r["code"]) && len(texts) > 0 {
			max := any(int64(240))
			if v, ok := r["max"]; ok && v != nil {
				max = normalize(v)
			}
			text, _ := texts[0].(string)
			about = Obj{"text": text, "enabled": isTrue(r["enabled"]), "max": max}
		}
	}
	c.st.Lock()
	c.aboutMe = about
	c.st.Unlock()
}

// SaveAboutMe is an explicit user action: it changes the account's "Sobre mim".
func (c *Controller) SaveAboutMe(ctx context.Context, text string) (Obj, error) {
	if strings.TrimSpace(text) == "" {
		return nil, perr("Digite o texto do Sobre mim.")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if p := c.getPhase(); p == Creating || p == Ending {
		return nil, perr("Aguarde a operação em andamento terminar.")
	}
	limit := 240
	c.st.Lock()
	if c.aboutMe != nil {
		limit = int(numOr(c.aboutMe["max"], 240))
	}
	c.st.Unlock()
	if len([]rune(text)) > limit {
		return nil, perrf("O Sobre mim aceita até %d caracteres.", limit)
	}
	result, err := c.browser.Call(ctx, "aboutmesave", Obj{"text": text})
	if err != nil {
		return nil, err
	}
	r := asObj(result)
	if !isTrue(r["saved"]) {
		if str(r, "reason") == "unavailable" {
			return nil, perr("O Sobre mim está desativado ou indisponível na conta. Ative-o no app do TikTok.")
		}
		return nil, perr("O TikTok não aceitou o Sobre mim.")
	}
	c.readAboutMe(ctx)
	note := "Sobre mim enviado ao TikTok."
	if f, ok := number(r["audit"]); ok && f != 0 {
		note = "Sobre mim enviado ao TikTok (pode passar por moderação)."
	}
	c.setDetail(note)
	return c.Snapshot(), nil
}

// ---- end

// End stops OBS and confirms the end on TikTok.
func (c *Controller) End(ctx context.Context) (Obj, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.endLocked(ctx)
}

// endLocked is the shutdown path used by every trigger (manual, signal, schedule, OBS stop, control loss).
// c.mu must be held.
func (c *Controller) endLocked(ctx context.Context) (Obj, error) {
	if c.binding == nil {
		return nil, perr("Não há sala identificada para encerrar. Confira a Central de LIVE.")
	}
	c.stopIM()
	c.st.Lock()
	c.resetAutopilotLocked() // the permission is for this broadcast only
	c.phase = Ending
	c.detail = "Parando o OBS e verificando o encerramento no TikTok…"
	c.st.Unlock()
	c.save()

	var wg sync.WaitGroup
	var mediaStopped, ended bool
	wg.Add(2)
	go func() {
		defer wg.Done()
		if c.output != nil {
			ok, err := c.output.Stop(ctx)
			mediaStopped = ok && err == nil
			return
		}
		// Recovery after restart: only a confirmed-idle output counts as stopped.
		if c.obs != nil {
			if status, err := c.readObs(ctx); err == nil && !outputBusy(status) {
				mediaStopped = true
			}
		}
	}()
	go func() {
		defer wg.Done()
		c.browser.Call(ctx, "ping", Obj{"binding": c.binding.obj(), "status": 4}) // best effort: verify decides
		if verified, err := c.browser.Call(ctx, "verify", c.binding.obj()); err == nil {
			ended = isTrue(asObj(verified)["ended"])
		}
	}()
	wg.Wait()

	if mediaStopped && ended {
		c.st.Lock()
		c.phase = Closed
		c.endAt = nil
		c.detail = "LIVE encerrada: OBS parado e término confirmado no TikTok."
		c.st.Unlock()
		c.restoreOrBlankService(ctx)
		c.browser.Call(ctx, "drop", c.binding.obj())
	} else {
		detail := "Parada do OBS não confirmada. "
		if mediaStopped {
			detail = "OBS parado. "
		}
		if ended {
			detail += "Sala encerrada."
		} else {
			detail += "Término remoto não confirmado; confira a Central de LIVE e tente Encerrar novamente."
		}
		c.setState(Uncertain, detail)
	}
	c.save()
	c.finishTrace()
	if c.obs != nil {
		c.readObs(ctx)
	}
	return c.Snapshot(), nil
}
