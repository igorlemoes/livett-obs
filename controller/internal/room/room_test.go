// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package room

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

const goodResponse = `{"status_code":0,"data":{"id_str":"123","stream_id_str":"789",
 "living_room_attrs":{"room_id_str":"456"},"stream_url":{
 "push_urls":["rtmpq://example.invalid/live/secret?token=synthetic"],
 "rtmp_push_url":"rtmp://example.invalid/live/fallback?token=other"}}}`

func parse(t *testing.T, raw string) map[string]any {
	t.Helper()
	m, err := ParseJSON([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestDistinctIDsAndExactFinishBody(t *testing.T) {
	binding, err := BindCreatedRoom(parse(t, goodResponse))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanEnd(*binding)
	if err != nil {
		t.Fatal(err)
	}
	if string(plan.Body) != "status=4&room_id=456&stream_id=789" || plan.VerificationRoomID != "123" {
		t.Fatalf("%q %q", plan.Body, plan.VerificationRoomID)
	}
	q, _ := url.ParseQuery(string(plan.Body))
	if q.Get("status") != "4" || q.Get("room_id") != "456" || q.Get("stream_id") != "789" {
		t.Fatal("body fields")
	}
	if plan.BodyMD5 != "b72c9fca60630df12f4b673ea469adf0" { // digest of the known request body
		t.Fatalf("md5 %q", plan.BodyMD5)
	}
}

func TestURLsPreserveQueryAndSecretsAreNotPrinted(t *testing.T) {
	room := parse(t, goodResponse)["data"].(map[string]any)
	targets, err := ExtractPushTargets(room)
	if err != nil || len(targets) != 2 {
		t.Fatalf("%v %v", targets, err)
	}
	if targets[0].Protocol != "rtmpq" || targets[1].Protocol != "rtmp" {
		t.Fatal("protocols")
	}
	if targets[0].Key() != "secret?token=synthetic" || targets[1].Server() != "rtmp://example.invalid/live" {
		t.Fatalf("%q %q", targets[0].Key(), targets[1].Server())
	}
	printed := fmt.Sprintf("%v %+v", targets, targets)
	if strings.Contains(printed, "secret") || strings.Contains(printed, "fallback") {
		t.Fatalf("secrets leaked through formatting: %s", printed)
	}
}

func TestErrorAndMissingIDsAreRejected(t *testing.T) {
	for _, status := range []string{"20003", "10018", "null", "false", "0.0", `"0"`} {
		r := parse(t, goodResponse)
		raw := strings.Replace(goodResponse, `"status_code":0`, `"status_code":`+status, 1)
		r = parse(t, raw)
		if _, err := BindCreatedRoom(r); err == nil {
			t.Errorf("status %s must be rejected", status)
		}
	}
	for _, v := range []string{"null", "123", `"0"`, `"1e9"`, `"１２３"`, `""`, `"12a"`} {
		raw := strings.Replace(goodResponse, `"room_id_str":"456"`, `"room_id_str":`+v, 1)
		if _, err := BindCreatedRoom(parse(t, raw)); err == nil {
			t.Errorf("ping room id %s must be rejected", v)
		}
	}
}

func TestUnsafeOrUnsupportedURLsAreRejected(t *testing.T) {
	for _, u := range []string{"https://example.invalid/key", "rtmp://user:pass@example.invalid/key", "rtmp://example.invalid/live/",
		"rtmp://example.invalid/key\n", "rtmp://example.invalid/key#fragment", "rtmp://example.invalid/key?x=a/b", "rtmp:///key"} {
		room := map[string]any{"stream_url": map[string]any{"push_urls": []any{u}}}
		if _, err := ExtractPushTargets(room); err == nil {
			t.Errorf("%q must be rejected", u)
		}
	}
}

func TestAbsentDestinationsAreNotInvented(t *testing.T) {
	if _, err := ExtractPushTargets(map[string]any{"stream_url": map[string]any{}}); err == nil {
		t.Fatal("no destination must be an error")
	}
}

func TestCreateFieldsAreFixedConstantsAndAccountDefaultsOnly(t *testing.T) {
	defaults := parse(t, `{"cover_uri":"tos-x/abc","screenshot_cover_status":1,"is_group_live_session":false}`)
	fields, err := CreateFields(" Olá ", defaults)
	if err != nil {
		t.Fatal(err)
	}
	if fields["title"] != "Olá" || fields["live_studio"] != "1" || fields["gen_replay"] != "true" ||
		fields["hashtag_id"] != "0" || fields["game_tag_id"] != "0" {
		t.Fatalf("%v", fields)
	}
	if fields["cover_uri"] != "tos-x/abc" || fields["screenshot_cover_status"] != "1" || fields["is_group_live_session"] != "false" {
		t.Fatalf("%v", fields)
	}
	for _, unverified := range []string{"chat_auth", "gift_auth", "age_restricted", "visible_scope_type", "ad_track_id",
		"open_commercial_content_toggle", "multi_stream_source"} {
		if _, ok := fields[unverified]; ok {
			t.Errorf("%s is unverified and must not be sent", unverified)
		}
	}
}

func TestMissingDefaultsAreOmittedAndBadTitleRejected(t *testing.T) {
	fields, err := CreateFields("T", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["cover_uri"]; ok {
		t.Fatal("cover_uri must be omitted")
	}
	if _, ok := fields["screenshot_cover_status"]; ok {
		t.Fatal("screenshot_cover_status must be omitted")
	}
	for _, bad := range []string{"", "   ", strings.Repeat("x", 81)} {
		if _, err := CreateFields(bad, nil); err == nil {
			t.Errorf("title %q must be rejected", bad)
		}
	}
}
