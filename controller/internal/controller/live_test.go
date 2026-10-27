// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tiktoklive/internal/wire"
)

func v(n, x uint64) []byte           { return wire.EncodeVarintField(n, x) }
func blob(n uint64, b []byte) []byte { return wire.EncodeBlob(n, b) }

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func message(method string, payload []byte, id uint64) []byte {
	return blob(1, cat(blob(1, []byte(method)), blob(2, payload), v(3, id)))
}

func displayResponse() []byte {
	user := cat(v(1, 77), blob(3, []byte("Maria")), blob(38, []byte("maria")))
	chat := cat(blob(2, user), blob(3, []byte("<b>oi</b>")))
	seats := cat(v(3, 5), v(7, 9))
	return cat(message("WebcastChatMessage", chat, 11), message("WebcastMemberMessage", blob(2, user), 12),
		message("WebcastRoomUserSeqMessage", seats, 13))
}

func (e *env) ingest(messages ...[]byte) {
	e.t.Helper()
	response, err := wire.DecodeResponse(cat(messages...))
	if err != nil {
		e.t.Fatal(err)
	}
	e.c.st.Lock()
	e.c.ingestLocked(response)
	e.c.st.Unlock()
}

func (e *env) viewSnap() Obj {
	e.c.st.Lock()
	defer e.c.st.Unlock()
	return e.c.viewSnapshotLocked()
}

func offlineEnv(t *testing.T) *env {
	e := &env{t: t, root: t.TempDir(), obs: newFakeObs(), browser: newFakeBrowser()}
	e.c = New(Options{Root: e.root, Browser: e.browser})
	return e
}

func intStat(o Obj, key string) int {
	f, _ := number(asObj(o["stats"])[key])
	return int(f)
}

func TestChatJoinsAndViewerCountsReachThePanel(t *testing.T) {
	e := guardEnv(t, [][]byte{displayResponse()}, nil)
	e.mustStart()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && e.viewSnap() == nil {
		time.Sleep(50 * time.Millisecond)
	}
	snapshot := e.c.Snapshot()
	if _, err := json.Marshal(snapshot); err != nil {
		t.Fatalf("snapshot must be JSON-safe: %v", err)
	}
	view := asObj(snapshot["view"])
	if snapshot["phase"] != "live" || view["viewers"] != uint64(5) || view["total"] != uint64(9) {
		t.Fatalf("%v", view)
	}
	var kinds []string
	var texts []string
	for _, item := range view["feed"].([]any) {
		m := asObj(item)
		kinds = append(kinds, m["k"].(string)+":"+m["name"].(string))
		if m["k"] == "chat" {
			texts = append(texts, m["text"].(string))
		}
	}
	joined := strings.Join(kinds, ",")
	if !strings.Contains(joined, "chat:Maria") || !strings.Contains(joined, "join:Maria") {
		t.Fatalf("%v", kinds)
	}
	if len(texts) != 1 || texts[0] != "<b>oi</b>" {
		t.Fatalf("text must be kept raw (rendered as text by the UI): %v", texts)
	}
	e.c.End(ctx())
}

func TestLikesAndCommentsComeFromTheIMStreamNotRoomInfo(t *testing.T) {
	e := offlineEnv(t)
	e.c.view.stats = Obj{"likes": 0, "comments": 0}
	user := cat(v(1, 5), blob(3, []byte("Ana")))
	chat := cat(blob(2, user), blob(3, []byte("oi")))
	e.ingest(message("WebcastLikeMessage", cat(v(2, 8), v(3, 9)), 1), message("WebcastChatMessage", chat, 2),
		message("WebcastChatMessage", chat, 2)) // a duplicate id is ignored
	snap := e.viewSnap()
	if intStat(snap, "likes") != 9 || intStat(snap, "comments") != 1 {
		t.Fatalf("%v", snap["stats"])
	}
}

func TestMaxDurationEndsTheLiveAutomatically(t *testing.T) {
	e := guardEnv(t, nil, nil)
	mctx, cancel := context.WithCancel(ctx())
	defer cancel()
	go e.c.Monitor(mctx)
	_, err := e.c.Start(ctx(), StartRequest{Title: "Título", Scene: "Brio 4k", Confirmed: true, MaxMinutes: 1})
	if err != nil {
		t.Fatal(err)
	}
	if e.c.Phase() != Live || e.c.Snapshot()["remainingSec"] == nil {
		t.Fatal("the schedule must show")
	}
	e.c.st.Lock()
	soon := time.Now().Add(300 * time.Millisecond)
	e.c.endAt = &soon // fast-forward the schedule
	e.c.st.Unlock()
	if !e.waitFor(Closed, 4*time.Second) {
		t.Fatalf("%v: %s", e.c.Phase(), e.c.Detail())
	}
	if !strings.Contains(e.c.Detail(), "duração máxima") || e.obs.active || e.c.endAt != nil {
		t.Fatalf("%s", e.c.Detail())
	}
}

func TestScheduleCanBeChangedClearedAndValidated(t *testing.T) {
	e := guardEnv(t, nil, nil)
	e.mustStart()
	if e.c.Snapshot()["remainingSec"] != nil {
		t.Fatal("no schedule by default")
	}
	result, err := e.c.SetSchedule(ctx(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if rem := result["remainingSec"].(int64); rem <= 500 || rem > 600 {
		t.Fatalf("%d", rem)
	}
	if cleared, _ := e.c.SetSchedule(ctx(), nil); cleared["remainingSec"] != nil {
		t.Fatal("must clear")
	}
	for _, bad := range []any{0, -5, 721, true, "x"} {
		_, err := e.c.SetSchedule(ctx(), bad)
		expectPanelError(t, err, "bad schedule")
	}
	e.c.End(ctx())
	_, err = e.c.SetSchedule(ctx(), 5)
	expectPanelError(t, err, "schedule outside a LIVE")
}

func TestInvalidDurationBlocksStartBeforeAnythingHappens(t *testing.T) {
	e := guardEnv(t, nil, nil)
	_, err := e.c.Start(ctx(), StartRequest{Title: "Título", Scene: "Brio 4k", Confirmed: true, MaxMinutes: 0})
	expectPanelError(t, err, "zero minutes")
	if e.browser.has("create") {
		t.Fatal("nothing may be created")
	}
}

// ---- gifts and social events

func giftMessage(id, count uint64, ended bool, name string, coins, group, sender uint64) []byte {
	user := cat(v(1, sender), blob(3, []byte("Maria")), blob(38, []byte("maria")))
	gift := cat(blob(2, []byte("Sent "+name)), v(12, coins), blob(16, []byte(name)))
	payload := cat(v(2, 5655), v(5, count), blob(7, user))
	if ended {
		payload = append(payload, v(9, 1)...)
	}
	payload = cat(payload, v(11, group), blob(15, gift))
	return message("WebcastGiftMessage", payload, id)
}

func TestAComboCountsOnceWhenItEndsAndShowsInTheFeed(t *testing.T) {
	e := offlineEnv(t)
	e.ingest(giftMessage(1, 1, false, "Rose", 1, 777, 77)) // still streaming
	if early := e.viewSnap(); early != nil && intStat(early, "gifts") != 0 {
		t.Fatalf("nothing is counted yet: %v", early)
	}
	e.ingest(giftMessage(2, 1, true, "Rose", 1, 777, 77)) // same group, ended
	snap := e.viewSnap()
	if intStat(snap, "gifts") != 1 || intStat(snap, "coins") != 1 || intStat(snap, "gifters") != 1 {
		t.Fatalf("%v", snap["stats"])
	}
	var gifts []Obj
	for _, item := range snap["feed"].([]any) {
		if m := asObj(item); m["k"] == "gift" {
			gifts = append(gifts, m)
		}
	}
	if len(gifts) != 1 || gifts[0]["name"] != "Maria" || gifts[0]["gift"] != "Rose" || gifts[0]["count"] != uint64(1) {
		t.Fatalf("%v", gifts)
	}
	if _, err := json.Marshal(snap); err != nil {
		t.Fatal(err)
	}
}

func TestBiggerComboAndSeparateSenders(t *testing.T) {
	e := offlineEnv(t)
	e.ingest(giftMessage(1, 3, true, "Rose", 5, 1, 1), giftMessage(2, 2, true, "Rose", 5, 2, 2))
	stats := e.viewSnap()
	if intStat(stats, "gifts") != 5 || intStat(stats, "coins") != 25 || intStat(stats, "gifters") != 2 {
		t.Fatalf("%v", stats["stats"])
	}
}

func TestAComboThatNeverEndsIsStillCountedAfterTenSeconds(t *testing.T) {
	e := offlineEnv(t)
	e.ingest(giftMessage(1, 2, false, "Rose", 1, 777, 77))
	e.c.st.Lock()
	for key, entry := range e.c.view.combos {
		entry.at = entry.at.Add(-11 * time.Second)
		e.c.view.combos[key] = entry
	}
	e.c.st.Unlock()
	e.ingest(giftMessage(9, 1, false, "Rose", 1, 999, 5)) // any later gift triggers the sweep
	if got := intStat(e.viewSnap(), "gifts"); got != 2 {
		t.Fatalf("gifts=%d", got)
	}
}

func socialMessage(id uint64, key string, action, userID uint64, name string) []byte {
	user := cat(v(1, userID), blob(3, []byte(name)), blob(38, []byte("maria")))
	payload := cat(blob(1, blob(8, blob(1, []byte(key)))), blob(2, user), v(4, action))
	return message("WebcastSocialMessage", payload, id)
}

func fanMessage(id uint64, name, rank string) []byte {
	user := cat(v(1, 77), blob(3, []byte(name)))
	text := cat(blob(1, []byte("pm_mt_fan_live_joined_comment")), blob(4, cat(v(1, 11), blob(21, blob(1, user)))),
		blob(4, cat(v(1, 1), blob(11, []byte(rank)))))
	return message("WebcastRoomMessage", blob(1, blob(8, text)), id)
}

func TestFollowShareAndFanClubEventsReachTheFeedAndCounters(t *testing.T) {
	e := offlineEnv(t)
	e.ingest(socialMessage(1, "pm_main_follow_message_viewer_2", 1, 77, "Maria"),
		socialMessage(2, "pm_mt_guidance_host_more_share", 3, 77, "Maria"), fanMessage(3, "Maria", "1"))
	view := e.viewSnap()
	var kinds []string
	rank := ""
	for _, item := range view["feed"].([]any) {
		m := asObj(item)
		kinds = append(kinds, m["k"].(string))
		if m["k"] == "fan" {
			rank = m["rank"].(string)
		}
	}
	if strings.Join(sorted(kinds), ",") != "fan,follow,share" || rank != "1" ||
		intStat(view, "follows") != 1 || intStat(view, "shares") != 1 || intStat(view, "fans") != 1 {
		t.Fatalf("%v %v", kinds, view["stats"])
	}
}

func sorted(in []string) []string {
	out := append([]string{}, in...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func TestTheHostGuidancePromptIsNotCountedAsAShare(t *testing.T) {
	e := offlineEnv(t)
	e.ingest(socialMessage(1, "pm_mt_guidance_host_share", 3, 77, "Anfitrião"), socialMessage(2, "something_unknown", 3, 77, "Maria"))
	if view := e.viewSnap(); view != nil && len(view["feed"].([]any)) != 0 {
		t.Fatalf("%v", view["feed"])
	}
}

func TestRoomStatForFollowsWinsOverTheLocalCount(t *testing.T) {
	e := offlineEnv(t)
	e.c.view.stats = Obj{"follows": 5}
	e.ingest(socialMessage(1, "pm_main_follow_message_viewer_2", 1, 77, "Maria"))
	if got := intStat(e.viewSnap(), "follows"); got != 5 {
		t.Fatalf("follows=%d", got)
	}
}

// ---- monitor

func TestStoppingTheStreamInsideOBSEndsTheRoomWithinASecond(t *testing.T) {
	e := guardEnv(t, nil, nil)
	mctx, cancel := context.WithCancel(ctx())
	defer cancel()
	go e.c.Monitor(mctx)
	e.mustStart()
	time.Sleep(100 * time.Millisecond)
	started := time.Now()
	e.obs.setActive(false) // the user pressed "Parar transmissão" in OBS
	if !e.waitFor(Closed, 3*time.Second) {
		t.Fatalf("%v", e.c.Phase())
	}
	if time.Since(started) > 2500*time.Millisecond || e.browser.count("create") != 1 {
		t.Fatalf("%v", time.Since(started))
	}
}

func TestABriefOBSHiccupDoesNotEndTheLive(t *testing.T) {
	e := guardEnv(t, nil, nil)
	mctx, cancel := context.WithCancel(ctx())
	defer cancel()
	go e.c.Monitor(mctx)
	e.mustStart()
	e.obs.mu.Lock()
	e.obs.override = func(name string, _ Obj) (Obj, error, bool) {
		if name == "GetStreamStatus" {
			return nil, errors.New("transient"), true
		}
		return nil, nil, false
	}
	e.obs.mu.Unlock()
	time.Sleep(2600 * time.Millisecond) // more than 2 failed passes, but well under 8 s of errors
	e.obs.mu.Lock()
	e.obs.override = nil
	e.obs.mu.Unlock()
	time.Sleep(1500 * time.Millisecond)
	if e.c.Phase() != Live {
		t.Fatalf("%v: %s", e.c.Phase(), e.c.Detail())
	}
}

// ---- chat

type chatEnv struct {
	*env
	sent []Obj
}

func startChatLive(t *testing.T) *chatEnv {
	e := guardEnv(t, nil, nil)
	ce := &chatEnv{env: e}
	prev := e.browser.override
	e.browser.setOverride(func(method string, data any) (any, error, bool) {
		if method == "chat" {
			ce.sent = append(ce.sent, asObj(data))
			return Obj{"code": json.Number("0"), "prompts": nil}, nil, true
		}
		return prev(method, data)
	})
	e.mustStart()
	e.c.ChatMinInterval = 0
	return ce
}

func TestSendsOneCleanMessageWithTheRoomIDOnlyWhileLive(t *testing.T) {
	e := guardEnv(t, nil, nil)
	if _, err := e.c.SendChat(ctx(), ChatRequest{Text: "oi"}); !isPanelError(err) {
		t.Fatalf("not live yet: %v", err)
	}
	ce := startChatLive(t)
	result, err := ce.c.SendChat(ctx(), ChatRequest{Text: "  Olá,\n  seja   bem-vindo!  "})
	if err != nil || result["sent"] != "Olá, seja bem-vindo!" {
		t.Fatalf("%v %v", result, err)
	}
	if len(ce.sent) != 1 || ce.sent[0]["room_id"] != "123" || ce.sent[0]["content"] != "Olá, seja bem-vindo!" {
		t.Fatalf("%v", ce.sent)
	}
}

func TestRepliesQuoteTheRealNameFromTheFeedNotTextSuppliedByACaller(t *testing.T) {
	ce := startChatLive(t)
	ce.c.st.Lock()
	ce.c.view.seq = 4
	ce.c.view.push(Obj{"k": "chat", "name": "Ana @everyone\nSilva", "text": "oi"})
	ce.c.st.Unlock()
	result, err := ce.c.SendChat(ctx(), ChatRequest{Text: "obrigado!", ReplyTo: 5})
	if err != nil || result["sent"] != "@Ana everyone Silva obrigado!" {
		t.Fatalf("%v %v", result, err)
	}
	for _, bad := range []any{99, true, "5"} {
		if _, err := ce.c.SendChat(ctx(), ChatRequest{Text: "x", ReplyTo: bad}); !isPanelError(err) {
			t.Errorf("reply_to %v must be refused", bad)
		}
	}
}

func TestRefusesLinksEmptyTooLongAndHiddenCharacters(t *testing.T) {
	ce := startChatLive(t)
	for _, bad := range []any{"", "   ", strings.Repeat("x", 201), "veja https://exemplo.com", "acesse www.exemplo",
		"meu site loja.com.br", "fale em wa.me/55", nil, 123} {
		if _, err := ce.c.SendChat(ctx(), ChatRequest{Text: bad}); !isPanelError(err) {
			t.Errorf("%v must be refused", bad)
		}
	}
	if _, err := ce.c.SendChat(ctx(), ChatRequest{Text: "a​b"}); err != nil { // zero-width char becomes a plain space
		t.Fatal(err)
	}
	if len(ce.sent) != 1 || ce.sent[0]["content"] != "a b" {
		t.Fatalf("%v", ce.sent)
	}
}

func TestRateLimitsAndNoRepeats(t *testing.T) {
	ce := startChatLive(t)
	ce.c.SendChat(ctx(), ChatRequest{Text: "primeira"})
	if _, err := ce.c.SendChat(ctx(), ChatRequest{Text: "primeira"}); !isPanelError(err) {
		t.Fatal("a repeat must be refused")
	}
	ce.c.ChatMinInterval = 30 * time.Second
	if _, err := ce.c.SendChat(ctx(), ChatRequest{Text: "segunda"}); !isPanelError(err) {
		t.Fatal("too soon must be refused")
	}
	ce.c.ChatMinInterval = 0
	for i := 0; i < 7; i++ {
		if _, err := ce.c.SendChat(ctx(), ChatRequest{Text: "mensagem " + string(rune('a'+i))}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ce.c.SendChat(ctx(), ChatRequest{Text: "uma a mais"}); !isPanelError(err) {
		t.Fatal("the per-minute cap (8) must hold")
	}
	if len(ce.sent) != 8 {
		t.Fatalf("%d", len(ce.sent))
	}
}

func TestARejectionFromTikTokIsReportedAndNotCounted(t *testing.T) {
	ce := startChatLive(t)
	prev := ce.browser.override
	ce.browser.setOverride(func(method string, data any) (any, error, bool) {
		if method == "chat" {
			return Obj{"code": json.Number("20001"), "prompts": "Comentário bloqueado"}, nil, true
		}
		return prev(method, data)
	})
	_, err := ce.c.SendChat(ctx(), ChatRequest{Text: "oi"})
	if !isPanelError(err) || !strings.Contains(err.Error(), "bloqueado") || len(ce.c.chatLog) != 0 {
		t.Fatalf("%v", err)
	}
}

func TestAssistantMessagesNeedTheUsersStandingPermissionForThisLiveOnly(t *testing.T) {
	ce := startChatLive(t)
	ce.c.AutoMinInterval = 0
	for _, via := range []string{"thanks", "answer"} {
		if _, err := ce.c.SendChat(ctx(), ChatRequest{Text: "Obrigado!", Via: via}); !isPanelError(err) {
			t.Fatalf("%s must need the permission", via)
		}
	}
	if _, err := ce.c.SendChat(ctx(), ChatRequest{Text: "x", Via: "whatever"}); !isPanelError(err) {
		t.Fatal("unknown origin")
	}
	yes := true
	ce.c.SetAutopilot(&yes, nil)
	if _, err := ce.c.SendChat(ctx(), ChatRequest{Text: "Obrigado pelo carinho, Ana!", Via: "thanks", Context: Obj{"event": "gift"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ce.c.SendChat(ctx(), ChatRequest{Text: "Custa R$ 10", Via: "answer"}); !isPanelError(err) {
		t.Fatal("answers were not allowed")
	}
	autopilot := asObj(ce.c.Snapshot()["autopilot"])
	if autopilot["thanks"] != true || autopilot["answers"] != false || autopilot["sent"] != 1 {
		t.Fatalf("%v", autopilot)
	}
	log, _ := os.ReadFile(filepath.Join(ce.root, "runtime", "autopilot-log.jsonl"))
	if !strings.Contains(string(log), "Obrigado pelo carinho") {
		t.Fatalf("the audit log must record the message: %s", log)
	}
	ce.c.End(ctx())
	if ce.c.autopilot["thanks"] || ce.c.autopilot["answers"] {
		t.Fatal("the permission ends with the broadcast")
	}
}

func TestPermissionCannotBeTurnedOnOutsideALiveAndLimitsAreStricter(t *testing.T) {
	e := guardEnv(t, nil, nil)
	yes, no := true, false
	if _, err := e.c.SetAutopilot(&yes, nil); !isPanelError(err) {
		t.Fatal("permission outside a LIVE must be refused")
	}
	if _, err := e.c.SetAutopilot(&no, nil); err != nil {
		t.Fatalf("turning off is always fine: %v", err)
	}
	ce := startChatLive(t)
	ce.c.SetAutopilot(&yes, &yes)
	if _, err := ce.c.SendChat(ctx(), ChatRequest{Text: "Obrigado, Ana!", Via: "thanks"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ce.c.SendChat(ctx(), ChatRequest{Text: "Obrigado, Bia!", Via: "thanks"}); !isPanelError(err) {
		t.Fatal("AUTO_MIN_INTERVAL (8 s) still applies")
	}
	ce.c.AutoMinInterval = 0
	for i := 0; i < 3; i++ {
		if _, err := ce.c.SendChat(ctx(), ChatRequest{Text: "Valeu " + string(rune('a'+i)) + "!", Via: "thanks"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ce.c.SendChat(ctx(), ChatRequest{Text: "Mais um!", Via: "thanks"}); !isPanelError(err) {
		t.Fatal("4 per minute cap")
	}
}

func TestThreeRejectionsInARowSwitchTheAutomaticFeaturesOff(t *testing.T) {
	ce := startChatLive(t)
	ce.c.AutoMinInterval = 0
	yes := true
	ce.c.SetAutopilot(&yes, nil)
	prev := ce.browser.override
	ce.browser.setOverride(func(method string, data any) (any, error, bool) {
		if method == "chat" {
			return Obj{"code": json.Number("1"), "prompts": "não"}, nil, true
		}
		return prev(method, data)
	})
	for i := 0; i < 3; i++ {
		if _, err := ce.c.SendChat(ctx(), ChatRequest{Text: "oi " + string(rune('a'+i)), Via: "thanks"}); !isPanelError(err) {
			t.Fatal("expected a refusal")
		}
	}
	if ce.c.autopilot["thanks"] || ce.c.autopilot["answers"] {
		t.Fatal("the kill switch must turn the automatic features off")
	}
}

type syncingBrowser struct {
	*fakeBrowser
	synced bool
}

func (b *syncingBrowser) SyncWebcastSession(context.Context) (bool, error) {
	b.synced = true
	return true, nil
}

func TestAFreshWebLoginIsSyncedAndTheAccountIsCheckedAgain(t *testing.T) {
	fb := newFakeBrowser()
	sb := &syncingBrowser{fakeBrowser: fb}
	checks := 0
	fb.override = func(method string, _ any) (any, error, bool) {
		if method == "check" {
			checks++
			if !sb.synced {
				return Obj{"code": json.Number("20003")}, nil, true
			}
		}
		return nil, nil, false
	}
	obs := newFakeObs()
	c := New(Options{Root: t.TempDir(), Browser: sb, ObsFactory: func(context.Context) (Obs, error) { return obs, nil }})
	state, err := c.Connect(ctx())
	if err != nil || state["phase"] != "ready" || checks != 2 || fb.loginShown != 0 {
		t.Fatalf("phase=%v checks=%d login=%d err=%v", state["phase"], checks, fb.loginShown, err)
	}
}
