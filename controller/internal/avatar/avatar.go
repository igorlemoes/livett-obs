// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// Package avatar is a loopback image proxy for viewer photos.
//
// The native overlay lives inside OBS's Qt, which ships without an HTTPS backend, so it asks the controller for
// photos over plain loopback HTTP. Only https URLs on TikTok's own image CDNs are fetched; size, time and redirects
// are bounded, and results are cached in memory. This is not an open proxy.
package avatar

import (
	"container/list"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	MaxURL    = 800
	MaxBytes  = 400 * 1024
	Timeout   = 6 * time.Second
	CacheSize = 300
)

var host = regexp.MustCompile(`^[a-z0-9-]+(\.[a-z0-9-]+)*\.tiktokcdn(-us|-eu)?\.com$`)

var ErrAvatar = errors.New("avatar unavailable")

// Doer is the HTTP client (a fake in tests).
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

type entry struct {
	url, kind string
	data      []byte
}

// Proxy fetches and caches photos.
type Proxy struct {
	Client Doer
	mu     sync.Mutex
	order  *list.List
	cache  map[string]*list.Element
}

func New() *Proxy {
	return &Proxy{
		// A redirect could leave the allowed hosts: never follow one.
		Client: &http.Client{Timeout: Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		order:  list.New(), cache: map[string]*list.Element{},
	}
}

// Validate accepts only https URLs on TikTok's image CDN hosts (no userinfo, no unusual port).
func Validate(raw string) error {
	if raw == "" || len(raw) > MaxURL {
		return ErrAvatar
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !host.MatchString(strings.ToLower(u.Hostname())) || u.User != nil ||
		(u.Port() != "" && u.Port() != "443") {
		return ErrAvatar
	}
	return nil
}

// Fetch returns the image bytes and content type for an allowed URL, cached.
func (p *Proxy) Fetch(raw string) ([]byte, string, error) {
	if err := Validate(raw); err != nil {
		return nil, "", err
	}
	p.mu.Lock()
	if el, ok := p.cache[raw]; ok {
		p.order.MoveToBack(el)
		e := el.Value.(*entry)
		p.mu.Unlock()
		return e.data, e.kind, nil
	}
	p.mu.Unlock()
	req, err := http.NewRequest("GET", raw, nil)
	if err != nil {
		return nil, "", ErrAvatar
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept", "image/*")
	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, "", ErrAvatar
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", ErrAvatar
	}
	kind, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(kind, "image/") {
		return nil, "", ErrAvatar
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxBytes+1))
	if err != nil || len(data) > MaxBytes || len(data) == 0 {
		return nil, "", ErrAvatar
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.cache[raw]; !ok {
		p.cache[raw] = p.order.PushBack(&entry{raw, kind, data})
	}
	for p.order.Len() > CacheSize {
		oldest := p.order.Front()
		p.order.Remove(oldest)
		delete(p.cache, oldest.Value.(*entry).url)
	}
	return data, kind, nil
}
