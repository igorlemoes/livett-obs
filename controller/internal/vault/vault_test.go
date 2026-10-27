// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package vault

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type xorSealer struct{}

func (xorSealer) Seal(p []byte) ([]byte, error) {
	out := make([]byte, len(p))
	for i, b := range p {
		out[i] = b ^ 0x5a
	}
	return out, nil
}
func (x xorSealer) Open(s []byte) ([]byte, error) { return x.Seal(s) }

func cookies(value string) []map[string]any {
	return []map[string]any{{"name": "sessionid_ss", "value": value, "domain": ".tiktok.com", "path": "/"}}
}

func TestSavedSessionsAreSealedAndTheListCarriesNoSecret(t *testing.T) {
	root := t.TempDir()
	s := Open(root, xorSealer{})
	if err := s.Save(Account{ID: "111", Name: "Loja A", Handle: "lojaa", Fingerprint: "fp1"}, cookies("SECRET-VALUE-1")); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(Account{ID: "222", Name: "Loja B", Handle: "lojab", Fingerprint: "fp2"}, cookies("SECRET-VALUE-2")); err != nil {
		t.Fatal(err)
	}
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		raw, _ := os.ReadFile(path)
		if bytes.Contains(raw, []byte("SECRET-VALUE")) {
			t.Fatalf("%s holds a clear-text session", path)
		}
		return nil
	})
	list := s.List()
	if len(list) != 2 || list[0].ID != "111" || list[1].Handle != "lojab" {
		t.Fatalf("%+v", list)
	}
	got, err := s.Cookies("222")
	if err != nil || got[0]["value"] != "SECRET-VALUE-2" {
		t.Fatalf("%v %v", got, err)
	}
	if a, ok := s.ByFingerprint("fp1"); !ok || a.ID != "111" {
		t.Fatal("the fingerprint must identify the account")
	}
}

func TestSavingAgainKeepsIdentityAndClearsTheExpiredFlag(t *testing.T) {
	s := Open(t.TempDir(), xorSealer{})
	s.Save(Account{ID: "1", Name: "Nome", Handle: "h", Avatar: "https://x/a.jpg", Fingerprint: "a"}, cookies("v1"))
	s.MarkExpired("1")
	if a, _ := s.Find("1"); !a.Expired {
		t.Fatal("expired flag")
	}
	s.Save(Account{ID: "1", Fingerprint: "b"}, cookies("v2")) // refreshed session, no identity given
	a, _ := s.Find("1")
	if a.Expired || a.Name != "Nome" || a.Handle != "h" || a.Avatar == "" || a.Fingerprint != "b" {
		t.Fatalf("%+v", a)
	}
}

func TestRemoveDeletesTheSessionFile(t *testing.T) {
	root := t.TempDir()
	s := Open(root, xorSealer{})
	s.Save(Account{ID: "9"}, cookies("v"))
	if err := s.Remove("9"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cookies("9"); err == nil || len(s.List()) != 0 {
		t.Fatal("the account must be gone")
	}
	if _, err := os.Stat(filepath.Join(root, "runtime", "accounts", "9.session")); err == nil {
		t.Fatal("the session file must be deleted")
	}
}

func TestUnsafeIDsAreRefused(t *testing.T) {
	s := Open(t.TempDir(), xorSealer{})
	for _, id := range []string{"", "../x", `a\b`, "a/b", strings.Repeat("1", 41)} {
		if s.Save(Account{ID: id}, cookies("v")) == nil {
			t.Fatalf("id %q must be refused", id)
		}
		if _, err := s.Cookies(id); err == nil {
			t.Fatalf("id %q must be refused on read", id)
		}
	}
}

func TestDPAPIRoundTripAndTamperDetection(t *testing.T) {
	d := DPAPI{}
	sealed, err := d.Seal([]byte("segredo da conta"))
	if err != nil {
		t.Skip("DPAPI unavailable:", err)
	}
	if bytes.Contains(sealed, []byte("segredo")) {
		t.Fatal("sealed data must not contain the plain text")
	}
	plain, err := d.Open(sealed)
	if err != nil || string(plain) != "segredo da conta" {
		t.Fatalf("%q %v", plain, err)
	}
	sealed[len(sealed)/2] ^= 0xff
	if _, err := d.Open(sealed); err == nil {
		t.Fatal("a tampered blob must not open")
	}
}
