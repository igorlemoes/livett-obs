// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"tiktoklive/internal/vault"
)

type plainSealer struct{}

func (plainSealer) Seal(p []byte) ([]byte, error) { return append([]byte("sealed:"), p...), nil }
func (plainSealer) Open(s []byte) ([]byte, error) {
	return []byte(strings.TrimPrefix(string(s), "sealed:")), nil
}

// acctBrowser fakes the managed Chrome with one session cookie at a time (the value is the account's letter).
type acctBrowser struct {
	*fakeBrowser
	mu       sync.Mutex
	session  string
	expired  map[string]bool
	ticket   *LoginTicket
	pkce     int
	signouts int
}

func newAcctBrowser(session string) *acctBrowser {
	b := &acctBrowser{fakeBrowser: newFakeBrowser(), session: session, expired: map[string]bool{}}
	b.override = func(method string, _ any) (any, error, bool) {
		b.mu.Lock()
		defer b.mu.Unlock()
		switch method {
		case "check":
			if b.session == "" || b.expired[b.session] {
				return Obj{"code": json.Number("20003")}, nil, true
			}
		case "profile":
			if b.session == "" || b.expired[b.session] {
				return Obj{"code": json.Number("20003")}, nil, true
			}
			return Obj{"code": json.Number("0"), "name": "Perfil " + b.session, "handle": "h" + b.session, "avatar": "https://p16.tiktokcdn.com/p-" + b.session + ".jpg"}, nil, true
		case "pkce":
			b.pkce++
			return Obj{"ok": true, "user": Obj{"id": "id-" + b.session, "name": "Nome " + b.session}}, nil, true
		}
		return nil, nil, false
	}
	return b
}

func (b *acctBrowser) signIn(letter string, withTicket bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.session = letter
	if withTicket {
		b.ticket = &LoginTicket{Identity: AccountIdentity{ID: "id-" + letter, Name: "Nome " + letter, Handle: "h" + letter},
			Ticket: "ticket-" + letter, Verifier: "v", Challenge: "c"}
	}
}
func (b *acctBrowser) SignOut(context.Context) error {
	b.mu.Lock()
	b.session, b.signouts = "", b.signouts+1
	b.mu.Unlock()
	return nil
}
func (b *acctBrowser) AccountCookies(context.Context) ([]map[string]any, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.session == "" {
		return nil, nil
	}
	return []map[string]any{{"name": "sessionid_ss", "value": b.session, "domain": ".tiktok.com"}}, nil
}
func (b *acctBrowser) RestoreCookies(_ context.Context, cookies []map[string]any) error {
	b.mu.Lock()
	b.session, _ = cookies[0]["value"].(string)
	b.mu.Unlock()
	return nil
}
func (b *acctBrowser) Fingerprint(context.Context) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.session == "" {
		return ""
	}
	return "fp-" + b.session
}
func (b *acctBrowser) Identify(context.Context) (AccountIdentity, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return AccountIdentity{ID: "id-" + b.session, Name: "Nome " + b.session, Handle: "h" + b.session, Avatar: "https://p16.tiktokcdn.com/" + b.session + ".jpg"}, nil
}
func (b *acctBrowser) LoginPending() bool { return false }
func (b *acctBrowser) TakeLogin() *LoginTicket {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.ticket
	b.ticket = nil
	return t
}

func acctRig(t *testing.T, session string) (*Controller, *acctBrowser, *vault.Store) {
	t.Helper()
	root := t.TempDir()
	b := newAcctBrowser(session)
	v := vault.Open(root, plainSealer{})
	obs := newFakeObs()
	c := New(Options{Root: root, Browser: b, Vault: v, ObsFactory: func(context.Context) (Obs, error) { return obs, nil }})
	return c, b, v
}

func activeID(c *Controller) string {
	for _, a := range c.Snapshot()["accounts"].([]Obj) {
		if a["active"] == true {
			return a["id"].(string)
		}
	}
	return ""
}

func TestConnectingSavesTheCurrentAccountOnce(t *testing.T) {
	c, _, v := acctRig(t, "A")
	if state, err := c.Connect(ctx()); err != nil || state["phase"] != "ready" {
		t.Fatalf("%v %v", state["phase"], err)
	}
	if _, err := c.Connect(ctx()); err != nil {
		t.Fatal(err)
	}
	list := v.List()
	if len(list) != 1 || list[0].ID != "id-A" || list[0].Handle != "hA" || activeID(c) != "id-A" {
		t.Fatalf("%+v active=%s", list, activeID(c))
	}
}

func TestAddingAnAccountKeepsTheOldOneAndOpensTheSignIn(t *testing.T) {
	c, b, v := acctRig(t, "A")
	c.Connect(ctx())
	state, err := c.AddAccount(ctx())
	if err != nil || state["phase"] != "login" {
		t.Fatalf("%v %v", state["phase"], err)
	}
	if b.loginShown == 0 || b.signouts != 1 || activeID(c) != "" {
		t.Fatalf("loginShown=%d signouts=%d active=%q", b.loginShown, b.signouts, activeID(c))
	}
	b.signIn("B", true) // the user signs in with the other account in the window
	if state, err := c.Connect(ctx()); err != nil || state["phase"] != "ready" {
		t.Fatalf("%v %v", state["phase"], err)
	}
	if b.pkce != 1 {
		t.Fatalf("the ticket must be exchanged once, got %d", b.pkce)
	}
	if len(v.List()) != 2 || activeID(c) != "id-B" {
		t.Fatalf("%+v active=%s", v.List(), activeID(c))
	}
}

func TestUsingASavedAccountRestoresItsSessionWithoutSigningIn(t *testing.T) {
	c, b, v := acctRig(t, "A")
	c.Connect(ctx())
	c.AddAccount(ctx())
	b.signIn("B", true)
	c.Connect(ctx())
	state, err := c.UseAccount(ctx(), "id-A")
	if err != nil || state["phase"] != "ready" {
		t.Fatalf("%v %v", state["phase"], err)
	}
	if b.session != "A" || activeID(c) != "id-A" || len(v.List()) != 2 {
		t.Fatalf("session=%s active=%s accounts=%d", b.session, activeID(c), len(v.List()))
	}
	if _, err := c.UseAccount(ctx(), "id-B"); err != nil || b.session != "B" {
		t.Fatalf("%v session=%s", err, b.session)
	}
}

func TestAnExpiredSavedSessionOpensTheSignInAndIsFlagged(t *testing.T) {
	c, b, v := acctRig(t, "A")
	c.Connect(ctx())
	c.AddAccount(ctx())
	b.signIn("B", true)
	c.Connect(ctx())
	b.mu.Lock()
	b.expired["A"] = true // TikTok no longer accepts A's saved session
	b.mu.Unlock()
	shown := b.loginShown
	state, err := c.UseAccount(ctx(), "id-A")
	if err != nil || state["phase"] != "login" || b.loginShown == shown {
		t.Fatalf("%v %v shown=%d", state["phase"], err, b.loginShown)
	}
	if a, _ := v.Find("id-A"); !a.Expired {
		t.Fatal("the account must be flagged as expired")
	}
}

func TestLogoutForgetsTheActiveAccount(t *testing.T) {
	c, b, v := acctRig(t, "A")
	c.Connect(ctx())
	state, err := c.Logout(ctx())
	if err != nil || state["phase"] != "disconnected" {
		t.Fatalf("%v %v", state["phase"], err)
	}
	if len(v.List()) != 0 || b.session != "" || activeID(c) != "" {
		t.Fatalf("%+v session=%q", v.List(), b.session)
	}
}

func TestRemovingAnInactiveAccountLeavesTheSessionAlone(t *testing.T) {
	c, b, v := acctRig(t, "A")
	c.Connect(ctx())
	c.AddAccount(ctx())
	b.signIn("B", false)
	c.Connect(ctx())
	if _, err := c.RemoveAccount(ctx(), "id-A"); err != nil {
		t.Fatal(err)
	}
	if len(v.List()) != 1 || b.session != "B" || c.Phase() != Ready {
		t.Fatalf("%+v session=%s phase=%s", v.List(), b.session, c.Phase())
	}
}

func TestAccountChangesAreRefusedWhileALiveExists(t *testing.T) {
	c, b, v := acctRig(t, "A")
	c.Connect(ctx())
	for _, phase := range []Phase{Creating, Live, Ending, Recovery, Uncertain} {
		c.setState(phase, "")
		if _, err := c.AddAccount(ctx()); err == nil {
			t.Fatalf("add must be refused in %s", phase)
		}
		if _, err := c.UseAccount(ctx(), "id-A"); err == nil {
			t.Fatalf("use must be refused in %s", phase)
		}
		if _, err := c.Logout(ctx()); err == nil {
			t.Fatalf("logout must be refused in %s", phase)
		}
	}
	if b.signouts != 0 || b.session != "A" || len(v.List()) != 1 {
		t.Fatal("the session must be left alone")
	}
}

func TestTheSnapshotListsAccountsWithoutAnySessionData(t *testing.T) {
	c, _, _ := acctRig(t, "A")
	c.Connect(ctx())
	raw, _ := json.Marshal(c.Snapshot()["accounts"])
	if strings.Contains(string(raw), "sessionid") || strings.Contains(string(raw), "value") || !strings.Contains(string(raw), "Perfil A") {
		t.Fatalf("%s", raw)
	}
}

func TestAnAccountSavedWithoutIdentityIsRepairedOnTheNextConnect(t *testing.T) {
	c, _, v := acctRig(t, "A")
	v.Save(vault.Account{ID: "fp-fp-A", Fingerprint: "fp-A"}, []map[string]any{{"name": "sessionid_ss", "value": "A", "domain": ".tiktok.com"}})
	c.refreshAccounts()
	if _, err := c.Connect(ctx()); err != nil {
		t.Fatal(err)
	}
	list := v.List()
	if len(list) != 1 || list[0].ID != "id-A" || list[0].Name != "Perfil A" || activeID(c) != "id-A" {
		t.Fatalf("%+v active=%s", list, activeID(c))
	}
}

func TestAnAccountSavedWithoutAPhotoGetsOneOnTheNextConnect(t *testing.T) {
	c, _, v := acctRig(t, "A")
	v.Save(vault.Account{ID: "id-A", Name: "Nome A", Handle: "hA", Fingerprint: "fp-A"}, []map[string]any{{"name": "sessionid_ss", "value": "A", "domain": ".tiktok.com"}})
	c.refreshAccounts()
	if _, err := c.Connect(ctx()); err != nil {
		t.Fatal(err)
	}
	if a, _ := v.Find("id-A"); a.Avatar != "https://p16.tiktokcdn.com/p-A.jpg" || a.Name != "Perfil A" {
		t.Fatalf("%+v", a)
	}
}

func TestTheContextIsSavedAnytimeKeptAcrossRestartsAndUpdatedLiveDuringALive(t *testing.T) {
	c, _, _ := acctRig(t, "A")
	state, err := c.SetContext(ctx(), "  Camiseta R$ 59, entrega em 5 dias  ")
	if err != nil || state["context"] != "Camiseta R$ 59, entrega em 5 dias" {
		t.Fatalf("%v %v", state["context"], err)
	}
	if state["productDetails"] != nil {
		t.Fatal("outside a LIVE the context is only saved, not on air")
	}
	again := New(Options{Root: c.root, Browser: newFakeBrowser()})
	if again.Snapshot()["context"] != "Camiseta R$ 59, entrega em 5 dias" {
		t.Fatal("the context must survive a restart")
	}
	c.setState(Live, "")
	if state, _ = c.SetContext(ctx(), "Novo preço R$ 49"); state["productDetails"] != "Novo preço R$ 49" {
		t.Fatalf("during a LIVE the context goes on air at once: %v", state["productDetails"])
	}
	if state, _ = c.SetContext(ctx(), ""); state["productDetails"] != nil || state["context"] != "" {
		t.Fatal("an empty text clears it")
	}
	if _, err := c.SetContext(ctx(), strings.Repeat("a", 2001)); err == nil {
		t.Fatal("the context is limited to 2000 characters")
	}
	c.setState(Creating, "")
	if _, err := c.SetContext(ctx(), "x"); err == nil {
		t.Fatal("no change while a room is being created")
	}
}

func TestALiveStartedFromTheObsButtonCarriesTheSavedContext(t *testing.T) {
	e := nativeEnv(t)
	e.c.SetContext(ctx(), "Gameplay de Free Fire, ranking Mestre")
	if _, err := e.startNative("Título"); err != nil {
		t.Fatal(err)
	}
	e.obs.setActive(true)
	if !e.waitFor(Live, 3*time.Second) {
		t.Fatalf("%v: %s", e.c.Phase(), e.c.Detail())
	}
	if e.c.Snapshot()["productDetails"] != "Gameplay de Free Fire, ranking Mestre" {
		t.Fatalf("%v", e.c.Snapshot()["productDetails"])
	}
}

func TestAnExplicitContextOverridesTheSavedOneForThatLive(t *testing.T) {
	e := nativeEnv(t)
	e.c.SetContext(ctx(), "salvo")
	given := "dado agora"
	if _, err := e.c.StartNative(ctx(), StartRequest{Title: "T", Scene: "Brio 4k", Confirmed: true, ProductDetails: &given}); err != nil {
		t.Fatal(err)
	}
	e.obs.setActive(true)
	e.waitFor(Live, 3*time.Second)
	if e.c.Snapshot()["productDetails"] != "dado agora" {
		t.Fatalf("%v", e.c.Snapshot()["productDetails"])
	}
}
func TestAssistantInfoHasReadyToCopyCommandsForThisInstallation(t *testing.T) {
	c, _, _ := acctRig(t, "A")
	info := c.AssistantInfo()
	exe, _ := os.Executable()
	for key, want := range map[string]string{"claudeCode": "claude mcp add --scope user livett -- ", "codex": "codex mcp add livett -- ",
		"codexToml": "[mcp_servers.livett]", "guide": "start_live"} {
		if got, _ := info[key].(string); !strings.Contains(got, want) {
			t.Fatalf("%s: %q", key, got)
		}
	}
	if !strings.Contains(info["claudeCode"].(string), `"`+exe+`" mcp --root "`+c.root+`"`) {
		t.Fatalf("%v", info["claudeCode"])
	}
	var cfg map[string]map[string]map[string]any
	if err := json.Unmarshal([]byte(info["json"].(string)), &cfg); err != nil || cfg["mcpServers"]["livett"]["command"] != exe {
		t.Fatalf("%v %v", err, info["json"])
	}
}

func TestIdleOnlyWhenNothingIsOnAirOrUndecided(t *testing.T) {
	c, _, _ := acctRig(t, "A")
	for phase, want := range map[Phase]bool{Disconnected: true, Ready: true, Login: true, Closed: true,
		Creating: false, Live: false, Ending: false, Uncertain: false, Recovery: false} {
		c.setState(phase, "")
		if c.Idle() != want {
			t.Errorf("%s: idle=%v, want %v", phase, c.Idle(), want)
		}
	}
	c.setState(Ready, "")
	c.st.Lock()
	c.binding = &Binding{}
	c.st.Unlock()
	if c.Idle() {
		t.Error("a controller that still holds a room it has not closed is not idle")
	}
	c.setState(Closed, "")
	if !c.Idle() {
		t.Error("after a confirmed end the kept room must not keep the controller alive")
	}
}

func TestAccountsCanChangeAfterAConfirmedEndButNotWithARoomToConfirm(t *testing.T) {
	c, _, _ := acctRig(t, "A")
	c.setState(Ready, "")
	c.st.Lock()
	c.binding = &Binding{}
	c.st.Unlock()
	c.mu.Lock()
	err := c.accountGateLocked()
	c.mu.Unlock()
	if err == nil {
		t.Fatal("a room that was not confirmed ended must block the change of account")
	}
	c.setState(Closed, "")
	c.mu.Lock()
	err = c.accountGateLocked()
	c.mu.Unlock()
	if err != nil || c.binding != nil {
		t.Fatalf("after a confirmed end the change must be allowed and the room forgotten: err=%v binding=%v", err, c.binding)
	}
}

func TestTheLiveProfileWinsOverThePassportAndIsRefreshedEveryTime(t *testing.T) {
	c, _, v := acctRig(t, "A")
	// saved earlier with the passport's data: the holder's name and personal photo, not the account's profile
	v.Save(vault.Account{ID: "id-A", Name: "Nome pessoal", Handle: "hA", Avatar: "https://old.example/x.jpg", Fingerprint: "fp-A"},
		[]map[string]any{{"name": "sessionid_ss", "value": "A", "domain": ".tiktok.com"}})
	c.refreshAccounts()
	if _, err := c.Connect(ctx()); err != nil {
		t.Fatal(err)
	}
	if a, _ := v.Find("id-A"); a.Name != "Perfil A" || a.Avatar != "https://p16.tiktokcdn.com/p-A.jpg" {
		t.Fatalf("%+v", a)
	}
}

func TestTheSameAccountSigningInAgainKeepsItsEntryEvenWithoutTheId(t *testing.T) {
	c, b, v := acctRig(t, "A")
	c.Connect(ctx())
	c.AddAccount(ctx())
	b.signIn("A2", false) // the same @handle comes back with a brand-new session (and no ticket)
	b.mu.Lock()
	b.override = func(method string, _ any) (any, error, bool) {
		if method == "profile" {
			return Obj{"code": json.Number("0"), "name": "Perfil A", "handle": "hA", "avatar": "https://p16.tiktokcdn.com/p-A.jpg"}, nil, true
		}
		return nil, nil, false
	}
	b.mu.Unlock()
	if _, err := c.Connect(ctx()); err != nil {
		t.Fatal(err)
	}
	if list := v.List(); len(list) != 1 || list[0].Fingerprint != "fp-A2" || activeID(c) != "id-A" {
		t.Fatalf("%+v active=%s", list, activeID(c))
	}
}

type refusedObs struct{}

func (refusedObs) Close() {}
func (refusedObs) Call(context.Context, string, map[string]any) (map[string]any, error) {
	return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
}

func TestAnUnreachableObsIsReportedInWordsAndOtherFailuresAreNot(t *testing.T) {
	traced := &tracedObs{inner: refusedObs{}, source: "obs", emit: func(string, string, Obj) {}}
	if _, err := traced.Call(ctx(), "GetStreamStatus", nil); err != errObsUnreachable {
		t.Fatalf("%v", err)
	}
	plain := &tracedObs{inner: failingObs{errors.New("boom")}, source: "obs", emit: func(string, string, Obj) {}}
	if _, err := plain.Call(ctx(), "GetStreamStatus", nil); err == nil || err == errObsUnreachable {
		t.Fatalf("an unrelated failure must stay as it is: %v", err)
	}
}

type failingObs struct{ err error }

func (failingObs) Close() {}
func (f failingObs) Call(context.Context, string, map[string]any) (map[string]any, error) {
	return nil, f.err
}
