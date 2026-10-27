// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package session

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func TestOnlyTikTokDomainsAreTouched(t *testing.T) {
	for domain, want := range map[string]bool{".tiktok.com": true, "www.tiktok.com": true, ".tiktokv.com": true, "webcast.tiktokv.com": true,
		"tiktok.com.evil.example": false, "nottiktok.com": false, ".google.com": false, "": false} {
		if isTikTokDomain(domain) != want {
			t.Errorf("%q: want %v", domain, want)
		}
	}
}

func TestParseAccountInfoReadsTheKnownFieldsAndDropsUnsafeAvatars(t *testing.T) {
	var reply map[string]any
	json.Unmarshal([]byte(`{"data":{"user_id_str":"123","screen_name":"Loja","username":"loja","avatar_url":"https://p16.tiktokcdn.com/a.jpg"}}`), &reply)
	got := parseAccountInfo(reply)
	if got.ID != "123" || got.Name != "Loja" || got.Handle != "loja" || got.Avatar == "" {
		t.Fatalf("%+v", got)
	}
	json.Unmarshal([]byte(`{"data":{"user_id":"9","nickname":"N","avatar_url":"http://insecure/a.jpg"}}`), &reply)
	if got := parseAccountInfo(reply); got.ID != "9" || got.Name != "N" || got.Avatar != "" {
		t.Fatalf("%+v", got)
	}
	if got := parseAccountInfo(nil); got != (AccountInfo{}) {
		t.Fatal("no reply, no identity")
	}
}

func TestTheChallengeIsTheMD5OfTheVerifier(t *testing.T) {
	verifier, challenge := NewVerifier()
	sum := md5.Sum([]byte(verifier))
	if len(verifier) < 32 || challenge != hex.EncodeToString(sum[:]) {
		t.Fatalf("%q %q", verifier, challenge)
	}
	if other, _ := NewVerifier(); other == verifier {
		t.Fatal("verifiers must be random")
	}
}

func TestTicketScriptEncodesTheChallengeSafely(t *testing.T) {
	script := ticketScript("a'b&c")
	if want := "code_challenge=a%27b%26c"; !strings.Contains(script, want) {
		t.Fatalf("%s", script)
	}
}

func TestOnlyPlainHandlesReachTheProfileScript(t *testing.T) {
	for handle, ok := range map[string]bool{"familiagauchashop": true, "a.b_c9": true, "x'+alert(1)+'": false, "": false, "a/b": false} {
		if handlePattern.MatchString(handle) != ok {
			t.Errorf("%q: want %v", handle, ok)
		}
	}
}
