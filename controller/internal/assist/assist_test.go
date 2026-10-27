// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package assist

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

type fakeLogin struct {
	calls []string
	fail  bool
}

func (f *fakeLogin) note(s string) error {
	f.calls = append(f.calls, s)
	if f.fail {
		return io.ErrUnexpectedEOF
	}
	return nil
}
func (f *fakeLogin) LoginShot(context.Context) ([]byte, error) {
	if f.fail {
		return nil, io.ErrUnexpectedEOF
	}
	return []byte("\xff\xd8\xffJPEG"), nil
}
func (f *fakeLogin) LoginTap(_ context.Context, x, y float64) error { return f.note("tap") }
func (f *fakeLogin) LoginText(_ context.Context, t string) error    { return f.note("text") }
func (f *fakeLogin) LoginKey(_ context.Context, k string) error     { return f.note("key:" + k) }
func (f *fakeLogin) LoginGo(_ context.Context, w string) error      { return f.note("go:" + w) }

func rig(t *testing.T, minutes float64) (*Server, *fakeLogin, string) {
	login := &fakeLogin{}
	s := NewServer(login, minutes)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, login, ts.URL + "/" + s.Token
}

func post(url, body string) int {
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		return -1
	}
	resp.Body.Close()
	return resp.StatusCode
}

func status(url string) int {
	resp, err := http.Get(url)
	if err != nil {
		return -1
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestPageShotAndActionsWorkOnlyWithTheSecret(t *testing.T) {
	s, login, base := rig(t, 5)
	resp, _ := http.Get(base + "/")
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Contains(page, []byte("Login do TikTok")) {
		t.Fatalf("%d", resp.StatusCode)
	}
	resp, _ = http.Get(base + "/shot")
	shot, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.HasPrefix(shot, []byte("\xff\xd8\xff")) {
		t.Fatalf("%d", resp.StatusCode)
	}
	for name, code := range map[string]int{
		"tap": post(base+"/tap", `{"fx":0.5,"fy":0.5}`), "text": post(base+"/text", `{"text":"123456"}`),
		"key": post(base+"/key", `{"key":"Enter"}`), "go": post(base+"/go", `{"to":"qr"}`)} {
		if code != 200 {
			t.Errorf("%s: %d", name, code)
		}
	}
	if len(login.calls) != 4 {
		t.Fatalf("%v", login.calls)
	}
	wrong := strings.Replace(base, s.Token, "wrong", 1)
	if status(wrong+"/") != 404 {
		t.Fatal("a wrong secret must look like nothing is there")
	}
	if post(wrong+"/tap", `{"fx":0.5,"fy":0.5}`) != 404 {
		t.Fatal("a wrong secret must not drive the browser")
	}
}

func TestInputsAreBoundedAndNavigationIsFixed(t *testing.T) {
	_, login, base := rig(t, 5)
	for name, c := range map[string]struct{ path, body string }{
		"tap outside": {"/tap", `{"fx":2,"fy":0.5}`}, "tap text": {"/tap", `{"fx":"a","fy":0.5}`}, "empty text": {"/text", `{"text":""}`},
		"long text": {"/text", `{"text":"` + strings.Repeat("x", 65) + `"}`}, "bad key": {"/key", `{"key":"F12"}`},
		"bad url": {"/go", `{"to":"https://evil.example"}`}, "not json": {"/text", `nope`}} {
		if code := post(base+c.path, c.body); code != 400 {
			t.Errorf("%s: %d", name, code)
		}
	}
	if len(login.calls) != 0 {
		t.Fatalf("a rejected input must never reach the browser: %v", login.calls)
	}
}

func TestItStopsAndExpires(t *testing.T) {
	s, _, base := rig(t, 5)
	if post(base+"/stop", `{}`) != 200 {
		t.Fatal("stop")
	}
	select {
	case <-s.Done():
	case <-time.After(time.Second):
		t.Fatal("Done must be closed after stop")
	}
	if status(base+"/shot") != 410 {
		t.Fatal("after stopping the view must be gone")
	}
	_, _, base2 := rig(t, 0.00001) // expires at once
	time.Sleep(20 * time.Millisecond)
	if status(base2+"/") != 410 {
		t.Fatal("after the deadline the view must be gone")
	}
}

func TestBrowserFailureIsAnErrorNotACrash(t *testing.T) {
	_, login, base := rig(t, 5)
	login.fail = true
	if status(base+"/shot") != 503 {
		t.Fatal("no picture must be reported")
	}
	if post(base+"/key", `{"key":"Enter"}`) != 503 {
		t.Fatal("a browser failure must be reported")
	}
}

func TestRunServesOnLoopbackAndRemovesTheLinkFile(t *testing.T) {
	file := t.TempDir() + "/link.txt"
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan string, 1)
	finished := make(chan error, 1)
	go func() { finished <- Run(ctx, &fakeLogin{}, 5, 0, "", file, func(link string) { got <- link }) }()
	var link string
	select {
	case link = <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("not ready")
	}
	if !strings.HasPrefix(link, "http://127.0.0.1:") {
		t.Fatalf("%s", link)
	}
	if status(link) != 200 {
		t.Fatal("the link must serve the page")
	}
	if raw, err := os.ReadFile(file); err != nil || string(raw) != link {
		t.Fatalf("the link file must hold the link: %v", err)
	}
	cancel()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("the link file must be deleted at the end")
	}
}
