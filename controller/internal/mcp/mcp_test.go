// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakePanel struct {
	posts []struct {
		path string
		data Obj
	}
	state func() Obj
}

func (f *fakePanel) State(context.Context) (Obj, error) {
	if f.state != nil {
		return f.state(), nil
	}
	feed := []any{}
	for i := 0; i < 30; i++ {
		feed = append(feed, Obj{"k": "chat", "name": "x", "text": "oi"})
	}
	return Obj{"phase": "ready", "detail": "ok", "obs": Obj{"scene": "A", "profile": "P", "active": false},
		"tiktok": Obj{"code": 0, "enabled": true}, "view": Obj{"viewers": 3, "feed": feed}}, nil
}

func (f *fakePanel) Post(ctx context.Context, path string, data Obj, _ time.Duration) (Obj, error) {
	f.posts = append(f.posts, struct {
		path string
		data Obj
	}{path, data})
	return f.State(ctx)
}

func (f *fakePanel) EnsureRunning(context.Context) (bool, error) { return false, nil }

func (f *fakePanel) postsTo(path string) []Obj {
	var out []Obj
	for _, p := range f.posts {
		if p.path == path {
			out = append(out, p.data)
		}
	}
	return out
}

type fakeObs struct {
	calls  []string
	args   []Obj
	scenes []string
}

func (f *fakeObs) Close() {}
func (f *fakeObs) has(name string) bool {
	for _, c := range f.calls {
		if c == name {
			return true
		}
	}
	return false
}
func (f *fakeObs) Call(_ context.Context, name string, data map[string]any) (map[string]any, error) {
	f.calls = append(f.calls, name)
	f.args = append(f.args, data)
	switch name {
	case "GetSceneList":
		list := []any{}
		for _, s := range f.scenes {
			list = append(list, Obj{"sceneName": s})
		}
		return Obj{"currentProgramSceneName": "A", "scenes": list}, nil
	case "GetInputList":
		return Obj{"inputs": []any{Obj{"inputName": "Video"}}}, nil
	case "GetCurrentProgramScene":
		return Obj{"currentProgramSceneName": "A"}, nil
	case "GetSourceScreenshot":
		return Obj{"imageData": "data:image/jpeg;base64,/9j/AAAA"}, nil
	}
	return Obj{}, nil
}

type rig struct {
	t       *testing.T
	panel   *fakePanel
	obs     *fakeObs
	server  *Server
	media   string
	outside string
	policy  Policy
}

func newRig(t *testing.T) *rig {
	dir := t.TempDir()
	media := filepath.Join(dir, "media")
	os.MkdirAll(media, 0o755)
	os.WriteFile(filepath.Join(media, "a.mp4"), []byte("x"), 0o644)
	outside := filepath.Join(dir, "outside.mp4")
	os.WriteFile(outside, []byte("x"), 0o644)
	r := &rig{t: t, panel: &fakePanel{}, obs: &fakeObs{scenes: []string{"A", "B"}}, media: media, outside: outside,
		policy: Policy{MaxDurationMinutes: 60, AllowedMediaDirs: []string{media}}}
	r.server = NewServer(&Tools{Panel: r.panel, OpenObs: func(context.Context) (ObsAPI, error) { return r.obs, nil }, Policy: r.policy})
	return r
}

func parse(t *testing.T, raw string) Obj {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var m Obj
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

// call runs tools/call with the arguments given as a JSON object literal.
func (r *rig) call(name, args string) Obj {
	r.t.Helper()
	if args == "" {
		args = "{}"
	}
	reply := r.server.Handle(context.Background(), parse(r.t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`))
	return reply["result"].(Obj)
}

func isError(result Obj) bool { return result["isError"] == true }

func (r *rig) body(result Obj) Obj {
	r.t.Helper()
	content := result["content"].([]any)
	return parse(r.t, content[0].(Obj)["text"].(string))
}

func TestProtocolBasics(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	init := r.server.Handle(ctx, parse(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`))
	if init["result"].(Obj)["protocolVersion"] != "2025-03-26" {
		t.Fatalf("%v", init)
	}
	if r.server.Handle(ctx, parse(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)) != nil {
		t.Fatal("notifications get no answer")
	}
	listed := r.server.Handle(ctx, parse(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	names := map[string]bool{}
	for _, tl := range listed["result"].(Obj)["tools"].([]any) {
		names[tl.(Obj)["name"].(string)] = true
	}
	for _, want := range []string{"start_live", "end_live", "set_scene", "create_video_scene", "send_chat", "autopilot", "obs_screenshot"} {
		if !names[want] {
			t.Errorf("tool %s is missing", want)
		}
	}
	unknown := r.server.Handle(ctx, parse(t, `{"jsonrpc":"2.0","id":3,"method":"nope"}`))
	if unknown["error"].(Obj)["code"] != -32601 {
		t.Fatalf("%v", unknown)
	}
}

func TestPublishingNeedsExplicitConfirmationTitleSceneAndBoundedDuration(t *testing.T) {
	r := newRig(t)
	if !isError(r.call("start_live", `{"title":"T","scene":"A","duration_minutes":10}`)) {
		t.Fatal("an unconfirmed publication must be refused")
	}
	if len(r.panel.posts) != 0 {
		t.Fatal("nothing may be posted")
	}
	for _, bad := range []string{`{"title":"T","scene":"A","duration_minutes":0}`, `{"title":"T","scene":"A","duration_minutes":61}`,
		`{"title":"","scene":"A","duration_minutes":5}`, `{"title":"T","scene":"","duration_minutes":5}`} {
		withConfirm := strings.Replace(bad, "{", `{"confirm_publish":true,`, 1)
		if !isError(r.call("start_live", withConfirm)) {
			t.Errorf("%s must be refused", bad)
		}
	}
	if len(r.panel.posts) != 0 {
		t.Fatal("nothing may be posted")
	}
	dry := r.call("start_live", `{"title":"T","scene":"A","duration_minutes":5,"dry_run":true}`)
	if isError(dry) || len(r.panel.posts) != 0 {
		t.Fatal("a dry run must show the plan and post nothing")
	}
}

func TestConfirmedStartSetsSceneThenStartsWithMaxMinutes(t *testing.T) {
	r := newRig(t)
	result := r.call("start_live", `{"title":"T","scene":"B","duration_minutes":15,"confirm_publish":true,"product_details":"Pizza R$ 30"}`)
	if isError(result) {
		t.Fatalf("%v", result)
	}
	if !r.obs.has("SetCurrentProgramScene") {
		t.Fatal("the scene must be set first")
	}
	starts := r.panel.postsTo("/api/start")
	if len(starts) != 1 || starts[0]["scene"] != "B" || starts[0]["confirmed"] != true || starts[0]["maxMinutes"] != float64(15) ||
		starts[0]["productDetails"] != "Pizza R$ 30" {
		t.Fatalf("%v", starts)
	}
}

func TestMediaPathsAreConfinedToAllowedFolders(t *testing.T) {
	r := newRig(t)
	good := filepath.Join(r.media, "a.mp4")
	if got, err := CheckMediaPath(good, r.policy); err != nil || !strings.EqualFold(got, good) {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{r.outside, filepath.Join(r.media, "missing.mp4"), filepath.Join(r.media, "..", "outside.mp4")} {
		if _, err := CheckMediaPath(bad, r.policy); err == nil {
			t.Errorf("%s must be refused", bad)
		}
	}
	exe := filepath.Join(r.media, "x.exe")
	os.WriteFile(exe, []byte("x"), 0o644)
	if _, err := CheckMediaPath(exe, r.policy); err == nil {
		t.Fatal("only video formats are allowed")
	}
}

func TestVideoSceneNeverOverwritesAndUnknownSceneIsRejected(t *testing.T) {
	r := newRig(t)
	file, _ := json.Marshal(filepath.Join(r.media, "a.mp4"))
	if isError(r.call("create_video_scene", `{"scene":"Novo","file":`+string(file)+`}`)) || !r.obs.has("CreateScene") {
		t.Fatal("a new scene must be created")
	}
	r.obs.calls = nil
	if !isError(r.call("create_video_scene", `{"scene":"A","file":`+string(file)+`}`)) || r.obs.has("CreateScene") {
		t.Fatal("an existing scene must never be overwritten")
	}
	if !isError(r.call("set_scene", `{"scene":"ZZ"}`)) {
		t.Fatal("an unknown scene must be refused")
	}
}

func TestChatTailIsBoundedAndFlagsUntrustedText(t *testing.T) {
	r := newRig(t)
	body := r.body(r.call("chat_tail", `{"limit":5}`))
	if len(body["mensagens"].([]any)) != 5 || !strings.Contains(body["aviso"].(string), "não confiável") {
		t.Fatalf("%v", body)
	}
}

func TestUnknownArgumentsAndAboutMeConfirmation(t *testing.T) {
	r := newRig(t)
	if !isError(r.call("live_status", `{"extra":1}`)) {
		t.Fatal("unknown arguments must be refused")
	}
	if !isError(r.call("set_about_me", `{"text":"x"}`)) || len(r.panel.postsTo("/api/about")) != 0 {
		t.Fatal("changing Sobre mim needs confirm=true")
	}
}

func TestSendChatNeedsExplicitUserApprovalAndValidArguments(t *testing.T) {
	r := newRig(t)
	for _, bad := range []string{`{"text":"Olá"}`, `{"text":"Olá","approved_by_user":false}`,
		`{"text":"` + strings.Repeat("x", 201) + `","approved_by_user":true}`, `{"text":"ok","reply_to_n":"3","approved_by_user":true}`} {
		if !isError(r.call("send_chat", bad)) {
			t.Errorf("%.40s must be refused", bad)
		}
	}
	if len(r.panel.postsTo("/api/chat")) != 0 {
		t.Fatal("nothing may be sent")
	}
	if isError(r.call("send_chat", `{"text":"Obrigado!","reply_to_n":4,"approved_by_user":true}`)) {
		t.Fatal("an approved message must go")
	}
	sent := r.panel.postsTo("/api/chat")
	if len(sent) != 1 || sent[0]["text"] != "Obrigado!" || sent[0]["replyToN"] != int64(4) {
		t.Fatalf("%v", sent)
	}
}

func TestChatTailExposesIDsHidesPhotosAndSupportsSince(t *testing.T) {
	r := newRig(t)
	r.panel.state = func() Obj {
		feed := []any{}
		for i := 1; i <= 7; i++ {
			feed = append(feed, Obj{"n": json.Number(string(rune('0' + i))), "k": "chat", "name": "x", "text": "t", "avatar": "https://p.tiktokcdn.com/a"})
		}
		return Obj{"phase": "live", "view": Obj{"viewers": 2, "feed": feed}}
	}
	body := r.body(r.call("chat_tail", `{"limit":20,"since_n":5}`))
	msgs := body["mensagens"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("%v", msgs)
	}
	for _, m := range msgs {
		if _, has := m.(Obj)["avatar"]; has {
			t.Fatal("photos must not be exposed")
		}
	}
}

func TestAutopilotNeedsApprovalAndAssistantMessagesNameTheirKindAndTarget(t *testing.T) {
	r := newRig(t)
	if !isError(r.call("autopilot", `{"thanks":true}`)) || len(r.panel.postsTo("/api/autopilot")) != 0 {
		t.Fatal("turning it on needs the user's approval")
	}
	if isError(r.call("autopilot", `{"thanks":true,"answers":true,"approved_by_user":true}`)) {
		t.Fatal("approved")
	}
	posts := r.panel.postsTo("/api/autopilot")
	if len(posts) != 1 || posts[0]["thanks"] != true || posts[0]["answers"] != true {
		t.Fatalf("%v", posts)
	}
	if isError(r.call("autopilot", `{"thanks":false}`)) {
		t.Fatal("turning off needs no approval")
	}
	if !isError(r.call("send_chat", `{"text":"Obrigado!","autopilot_kind":"thanks"}`)) {
		t.Fatal("it needs reply_to_n")
	}
	if !isError(r.call("send_chat", `{"text":"x","reply_to_n":1,"autopilot_kind":"other"}`)) {
		t.Fatal("unknown kind")
	}
	if isError(r.call("send_chat", `{"text":"Obrigado, Ana!","reply_to_n":3,"autopilot_kind":"thanks","context":"presente"}`)) {
		t.Fatal("a standing-permission message must go to the controller")
	}
	chat := r.panel.postsTo("/api/chat")
	last := chat[len(chat)-1]
	if last["via"] != "thanks" || last["replyToN"] != int64(3) || last["text"] != "Obrigado, Ana!" {
		t.Fatalf("%v", last)
	}
}

func TestLiveContextListsOnlyPublishedFactsAndScreenshotReturnsAnImage(t *testing.T) {
	r := newRig(t)
	r.panel.state = func() Obj {
		return Obj{"phase": "live", "liveTitle": "Oferta", "aboutMe": Obj{"text": "Frete grátis acima de R$100"}, "obs": Obj{"scene": "A"},
			"view": Obj{"intro": "Bem-vindos", "viewers": 4}, "autopilot": Obj{"thanks": true, "answers": false}}
	}
	body := r.body(r.call("live_context", ""))
	if body["titulo_da_live"] != "Oferta" || body["introducao_na_sala"] != "Bem-vindos" || body["cena_no_ar"] != "A" ||
		!strings.Contains(body["regra"].(string), "Se não der para confirmar") {
		t.Fatalf("%v", body)
	}
	result := r.call("obs_screenshot", `{"width":640}`)
	content := result["content"].([]any)
	if len(content) != 2 || content[0].(Obj)["type"] != "image" || content[0].(Obj)["mimeType"] != "image/jpeg" || content[1].(Obj)["type"] != "text" {
		t.Fatalf("%v", content)
	}
	var shot Obj
	for i, c := range r.obs.calls {
		if c == "GetSourceScreenshot" {
			shot = r.obs.args[i]
		}
	}
	if shot["sourceName"] != "A" || shot["imageWidth"] != int64(640) {
		t.Fatalf("%v", shot)
	}
	if !isError(r.call("obs_screenshot", `{"scene":"NaoExiste"}`)) {
		t.Fatal("an unknown scene must be refused")
	}
}

func TestServeAnswersLinesAndSurvivesGarbage(t *testing.T) {
	r := newRig(t)
	in := strings.NewReader("not json\n" + `{"jsonrpc":"2.0","id":7,"method":"ping"}` + "\n\n" + `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	var out bytes.Buffer
	if err := r.server.Serve(context.Background(), in, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "-32700") || !strings.Contains(lines[1], `"id":7`) {
		t.Fatalf("%q", out.String())
	}
}

func TestPolicyDefaultsAndFileOverride(t *testing.T) {
	root := t.TempDir()
	if p := LoadPolicy(root); p.MaxDurationMinutes != 180 || p.AllowedScenes != nil {
		t.Fatalf("%+v", p)
	}
	os.MkdirAll(filepath.Join(root, "runtime"), 0o755)
	os.WriteFile(filepath.Join(root, "runtime", "mcp-policy.json"), []byte(`{"max_duration_minutes":30,"allowed_scenes":["A"]}`), 0o644)
	p := LoadPolicy(root)
	if p.MaxDurationMinutes != 30 || p.AllowedScenes == nil || (*p.AllowedScenes)[0] != "A" {
		t.Fatalf("%+v", p)
	}
	r := newRig(t)
	r.server.Tools.Policy = p
	if !isError(r.call("set_scene", `{"scene":"B"}`)) {
		t.Fatal("a scene outside the policy must be refused")
	}
}
