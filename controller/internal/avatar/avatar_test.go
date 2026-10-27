// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package avatar

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

type fakeClient struct {
	calls int
	body  []byte
	kind  string
	code  int
}

func (f *fakeClient) Do(*http.Request) (*http.Response, error) {
	f.calls++
	code := f.code
	if code == 0 {
		code = 200
	}
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {f.kind}}, Body: io.NopCloser(bytes.NewReader(f.body))}, nil
}

func TestOnlyHttpsTikTokImageCDNsAreAllowed(t *testing.T) {
	for _, u := range []string{"https://p16-common-sign.tiktokcdn.com/a.webp?x=1", "https://p19.tiktokcdn-us.com/b.jpeg"} {
		if err := Validate(u); err != nil {
			t.Errorf("%s must be allowed: %v", u, err)
		}
	}
	bad := []string{"http://p16.tiktokcdn.com/a.png", "https://evil.example/a.png", "https://tiktokcdn.com.evil.example/a.png",
		"https://user@p16.tiktokcdn.com/a.png", "https://p16.tiktokcdn.com:8443/a.png", "file:///c:/x", "",
		"https://127.0.0.1/a.png", "https://p16.tiktokcdn.com.evil.io/a.png", "https://x.tiktokcdn.com/" + strings.Repeat("a", 900)}
	for _, u := range bad {
		if err := Validate(u); err == nil {
			t.Errorf("%q must be refused", u)
		}
	}
}

func TestFetchCachesAndRejectsNonImagesAndOversize(t *testing.T) {
	p := New()
	client := &fakeClient{body: []byte("RIFFdata"), kind: "image/webp"}
	p.Client = client
	url := "https://p16.tiktokcdn.com/a.webp"
	data, kind, err := p.Fetch(url)
	if err != nil || string(data) != "RIFFdata" || kind != "image/webp" {
		t.Fatalf("%q %q %v", data, kind, err)
	}
	p.Fetch(url)
	if client.calls != 1 {
		t.Fatalf("the second call must come from the cache (%d fetches)", client.calls)
	}
	p.Client = &fakeClient{body: []byte("<html>"), kind: "text/html"}
	if _, _, err := p.Fetch("https://p16.tiktokcdn.com/b"); err == nil {
		t.Fatal("a non-image must be refused")
	}
	p.Client = &fakeClient{body: bytes.Repeat([]byte("x"), MaxBytes+5), kind: "image/png"}
	if _, _, err := p.Fetch("https://p16.tiktokcdn.com/c"); err == nil {
		t.Fatal("an oversize image must be refused")
	}
	p.Client = &fakeClient{body: []byte("redirect"), kind: "image/png", code: 302}
	if _, _, err := p.Fetch("https://p16.tiktokcdn.com/d"); err == nil {
		t.Fatal("a redirect must be refused, never followed")
	}
}

func TestCacheIsBounded(t *testing.T) {
	p := New()
	p.Client = &fakeClient{body: []byte("x"), kind: "image/png"}
	for i := 0; i < CacheSize+20; i++ {
		p.Fetch("https://p16.tiktokcdn.com/" + strings.Repeat("a", i%50+1) + string(rune('a'+i%26)) + strings.Repeat("b", i/26))
	}
	if p.order.Len() > CacheSize {
		t.Fatalf("cache grew to %d", p.order.Len())
	}
}
