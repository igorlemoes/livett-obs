// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// Package vault keeps the TikTok sessions of the accounts the user signed in with, so switching accounts does not
// need a new sign-in. The list shown in the panel (name, handle, photo) is plain
// JSON; each account's cookies are a separate file sealed with the Windows user's key (DPAPI): another Windows user,
// or a copy of the folder on another machine, cannot read them.
package vault

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Sealer encrypts and decrypts the session files. The real one is DPAPI; tests use a reversible fake.
type Sealer interface {
	Seal(plain []byte) ([]byte, error)
	Open(sealed []byte) ([]byte, error)
}

// Account is what the panel may show. It never carries a cookie or token.
type Account struct {
	ID          string `json:"id"` // TikTok user id; a local fingerprint when the id could not be read
	Name        string `json:"name"`
	Handle      string `json:"handle"`
	Avatar      string `json:"avatar"`
	Fingerprint string `json:"fingerprint"` // truncated hash of the session cookie: tells which account a browser holds
	Expired     bool   `json:"expired"`
	AddedAt     int64  `json:"addedAt"`
	UsedAt      int64  `json:"usedAt"`
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

// ValidID reports whether id is safe to use as a file name.
func ValidID(id string) bool { return idPattern.MatchString(id) }

type Store struct {
	dir    string
	sealer Sealer
	mu     sync.Mutex
}

func Open(root string, sealer Sealer) *Store {
	return &Store{dir: filepath.Join(root, "runtime", "accounts"), sealer: sealer}
}

func (s *Store) indexPath() string { return filepath.Join(s.dir, "index.json") }
func (s *Store) sessionPath(id string) string {
	return filepath.Join(s.dir, id+".session")
}

func (s *Store) readIndex() []Account {
	raw, err := os.ReadFile(s.indexPath())
	if err != nil {
		return nil
	}
	var list []Account
	if json.Unmarshal(raw, &list) != nil {
		return nil
	}
	return list
}

func (s *Store) writeIndex(list []Account) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].AddedAt < list[j].AddedAt })
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.indexPath(), raw)
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// List returns the saved accounts, oldest first.
func (s *Store) List() []Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.readIndex()
	sort.SliceStable(list, func(i, j int) bool { return list[i].AddedAt < list[j].AddedAt })
	return list
}

// Find returns the account with the given id.
func (s *Store) Find(id string) (Account, bool) {
	for _, a := range s.List() {
		if a.ID == id {
			return a, true
		}
	}
	return Account{}, false
}

// ByFingerprint finds the account whose saved session matches a browser's current session cookie.
func (s *Store) ByFingerprint(fp string) (Account, bool) {
	if fp == "" {
		return Account{}, false
	}
	for _, a := range s.List() {
		if a.Fingerprint == fp {
			return a, true
		}
	}
	return Account{}, false
}

// Save adds or updates an account and replaces its sealed session. Cookies are opaque JSON-able values.
func (s *Store) Save(acc Account, cookies []map[string]any) error {
	if !ValidID(acc.ID) {
		return errors.New("invalid account id")
	}
	if len(cookies) == 0 {
		return errors.New("no session to save")
	}
	plain, err := json.Marshal(cookies)
	if err != nil {
		return err
	}
	sealed, err := s.sealer.Seal(plain)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	if err := writeAtomic(s.sessionPath(acc.ID), sealed); err != nil {
		return err
	}
	now := time.Now().Unix()
	list := s.readIndex()
	found := false
	for i := range list {
		if list[i].ID == acc.ID {
			if acc.Name == "" {
				acc.Name = list[i].Name
			}
			if acc.Handle == "" {
				acc.Handle = list[i].Handle
			}
			if acc.Avatar == "" {
				acc.Avatar = list[i].Avatar
			}
			acc.AddedAt = list[i].AddedAt
			acc.UsedAt = now
			acc.Expired = false
			list[i] = acc
			found = true
			break
		}
	}
	if !found {
		acc.AddedAt, acc.UsedAt, acc.Expired = now, now, false
		list = append(list, acc)
	}
	return s.writeIndex(list)
}

// Cookies returns an account's saved session.
func (s *Store) Cookies(id string) ([]map[string]any, error) {
	if !ValidID(id) {
		return nil, errors.New("invalid account id")
	}
	sealed, err := os.ReadFile(s.sessionPath(id))
	if err != nil {
		return nil, errors.New("sessão salva não encontrada")
	}
	plain, err := s.sealer.Open(sealed)
	if err != nil {
		return nil, errors.New("não foi possível abrir a sessão salva (outro usuário do Windows?)")
	}
	var cookies []map[string]any
	if err := json.Unmarshal(plain, &cookies); err != nil {
		return nil, errors.New("sessão salva ilegível")
	}
	return cookies, nil
}

// MarkExpired flags an account whose saved session TikTok no longer accepts (the user must sign in again).
func (s *Store) MarkExpired(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.readIndex()
	for i := range list {
		if list[i].ID == id && !list[i].Expired {
			list[i].Expired = true
			s.writeIndex(list)
			return
		}
	}
}

// Remove forgets an account and deletes its sealed session.
func (s *Store) Remove(id string) error {
	if !ValidID(id) {
		return errors.New("invalid account id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	os.Remove(s.sessionPath(id))
	list := s.readIndex()
	out := list[:0]
	for _, a := range list {
		if a.ID != id {
			out = append(out, a)
		}
	}
	return s.writeIndex(out)
}
