// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package wire

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"testing"

	"tiktoklive/internal/guard"
)

// Fixed binary fixtures.
// Response -> Message -> ControlMessage -> Common(room_id=123), action=4.
const controlResponseHex = "0a210a1557656263617374436f6e74726f6c4d65737361676512060a02187b10041863" + "1201632a01784801"

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func gz(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

func TestFixedResponseDecodesControlAndExactID(t *testing.T) {
	response, err := DecodeResponse(mustHex(t, controlResponseHex))
	if err != nil {
		t.Fatal(err)
	}
	if response.Cursor != "c" || response.InternalExt != "x" || !response.NeedAck {
		t.Fatalf("%+v", response)
	}
	signal, err := Classify(response.Messages[0], "test")
	if err != nil || signal == nil {
		t.Fatalf("%v %v", signal, err)
	}
	if signal.RoomID != "123" || signal.EvidenceID != "im:99" || signal.Kind != guard.RemoteEnd {
		t.Fatalf("%+v", signal)
	}
}

func TestPushAckKeepsLogidAndInternalExt(t *testing.T) {
	control := mustHex(t, controlResponseHex)
	frame := append(mustHex(t, "10073a036d736742"), byte(len(control)))
	frame = append(frame, control...)
	kind, response, ack, err := DecodePush(frame)
	if err != nil || kind != "msg" || response.Cursor != "c" {
		t.Fatalf("%v %v %v", kind, response, err)
	}
	if !bytes.Equal(ack, mustHex(t, "10073a0361636b420178")) {
		t.Fatalf("ack %x", ack)
	}
}

func TestGzipPayloadDecodesIdentically(t *testing.T) {
	control := mustHex(t, controlResponseHex)
	packed := gz(t, control)
	frame := append(mustHex(t, "2a150a0d636f6d70726573735f747970651204677a6970"), mustHex(t, "10073a036d736742")...)
	frame = append(frame, byte(len(packed)))
	frame = append(frame, packed...)
	_, got, _, err := DecodePush(frame)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := DecodeResponse(control)
	if got.Cursor != want.Cursor || len(got.Messages) != len(want.Messages) || got.Messages[0].MessageID != want.Messages[0].MessageID {
		t.Fatalf("%+v vs %+v", got, want)
	}
}

func TestHistoryAndUnmappedPerceptionDoNotTriggerShutdown(t *testing.T) {
	response, _ := DecodeResponse(mustHex(t, controlResponseHex))
	m := response.Messages[0]
	if s, _ := Classify(Message{m.Method, m.Payload, "99", true}, "x"); s != nil {
		t.Fatal("history must not trigger")
	}
	if s, _ := Classify(Message{"WebcastPerceptionMessage", nil, "100", false}, "x"); s != nil {
		t.Fatal("perception is unmapped")
	}
}

func TestMissingRoomAndNonterminalControlDoNotTrigger(t *testing.T) {
	for _, p := range []string{"1004", "0a02187b1002"} {
		s, err := Classify(Message{"ControlMessage", mustHex(t, p), "1", false}, "x")
		if s != nil || err != nil {
			t.Fatalf("%s: %v %v", p, s, err)
		}
	}
}

func TestNoticeRequiresContentAndCurrentRoomID(t *testing.T) {
	m := Message{"WebcastNoticeMessage", mustHex(t, "0a02187b12047761726e"), "3", false}
	if s, _ := Classify(m, "x"); s == nil || s.Kind != guard.Warning {
		t.Fatalf("%v", s)
	}
	m.Payload = mustHex(t, "0a02187b")
	if s, _ := Classify(m, "x"); s != nil {
		t.Fatal("a notice with no content must not trigger")
	}
}

func TestMalformedWireIsRejected(t *testing.T) {
	for _, raw := range [][]byte{{0x80}, {0x00}, append([]byte{0x0a, 0x10}, "abc"...), {0x0b}, append([]byte{0x08}, bytes.Repeat([]byte{0xff}, 10)...)} {
		if _, err := DecodeResponse(raw); !IsError(err) {
			t.Errorf("%x: want wire error, got %v", raw, err)
		}
	}
}

func TestHeartbeatDoesNotParseAsWarning(t *testing.T) {
	kind, response, ack, err := DecodePush(mustHex(t, "3a026862"))
	if kind != "hb" || response != nil || ack != nil || err != nil {
		t.Fatalf("%v %v %v %v", kind, response, ack, err)
	}
}

func TestGzipExpansionHasAHardLimit(t *testing.T) {
	old := MaxDecoded
	MaxDecoded = 128
	defer func() { MaxDecoded = old }()
	packed := gz(t, bytes.Repeat([]byte("a"), 1000))
	frame := append(mustHex(t, "2a150a0d636f6d70726573735f747970651204677a6970"), mustHex(t, "3a036d736742")...)
	frame = append(frame, byte(len(packed)))
	frame = append(frame, packed...)
	if _, _, _, err := DecodePush(frame); !IsError(err) {
		t.Fatalf("want wire error, got %v", err)
	}
}

func TestLargeRoomIDsKeepIntegerPrecision(t *testing.T) {
	room := append(append([]byte{0x18, 0x81}, bytes.Repeat([]byte{0x80}, 6)...), 0x10)
	payload := append(append([]byte{0x0a, 0x09}, room...), 0x10, 0x04)
	s, err := Classify(Message{"ControlMessage", payload, "1", false}, "x")
	if err != nil || s == nil || s.RoomID != "9007199254740993" {
		t.Fatalf("%v %v", s, err)
	}
}

// --- display decoders

func user() []byte {
	out := EncodeVarintField(1, 7000000000000000001)
	out = append(out, EncodeBlob(3, []byte("Ana Teste"))...)
	return append(out, EncodeBlob(38, []byte("anateste"))...)
}

func TestChatMemberAndViewerCounts(t *testing.T) {
	chat, err := DecodeChat(append(EncodeBlob(2, user()), EncodeBlob(3, []byte("Oi beleza"))...))
	if err != nil || chat.User.Name != "Ana Teste" || chat.User.Handle != "anateste" || chat.Text != "Oi beleza" ||
		chat.User.ID != "7000000000000000001" {
		t.Fatalf("%+v %v", chat, err)
	}
	if m, err := DecodeMember(EncodeBlob(2, user())); err != nil || m.Name != "Ana Teste" {
		t.Fatalf("%v %v", m, err)
	}
	counts := append(EncodeVarintField(3, 7), EncodeVarintField(7, 12)...)
	if r, err := DecodeRoomUsers(counts); err != nil || r.Viewers != 7 || r.Total != 12 {
		t.Fatalf("%v %v", r, err)
	}
	if r, _ := DecodeRoomUsers(nil); r.Viewers != 0 || r.Total != 0 {
		t.Fatalf("%v", r)
	}
}

func TestMalformedDisplayPayloadsRaiseWireError(t *testing.T) {
	if _, err := DecodeChat([]byte{0x12, 0xFF}); !IsError(err) {
		t.Fatalf("%v", err)
	}
	if _, err := DecodeChat(EncodeBlob(3, []byte{0xFF, 0xFE})); !IsError(err) {
		t.Fatalf("%v", err)
	}
}

func userWith(urls ...string) []byte {
	var avatarBlob []byte
	for _, u := range urls {
		avatarBlob = append(avatarBlob, EncodeBlob(1, []byte(u))...)
	}
	out := EncodeVarintField(1, 5)
	out = append(out, EncodeBlob(3, []byte("Ana"))...)
	return append(out, EncodeBlob(9, avatarBlob)...)
}

func TestOnlyHttpsTikTokCDNAvatarsAreAccepted(t *testing.T) {
	good := "https://p16-common-sign.tiktokcdn.com/obj/x~tplv-cropcenter:100:100.webp?x-expires=1&x-signature=abc"
	us := "https://p19-common-sign.tiktokcdn-us.com/a.webp"
	cases := []struct {
		urls []string
		want string
	}{
		{[]string{good}, good},
		{[]string{"https://evil.example/a.png", good}, good},
		{[]string{"http://p16.tiktokcdn.com/a.png"}, ""},
		{[]string{"https://tiktokcdn.com.evil.example/a.png"}, ""},
		{[]string{us}, us},
		{nil, ""},
	}
	for _, c := range cases {
		m, err := DecodeMember(EncodeBlob(2, userWith(c.urls...)))
		if err != nil || m.Avatar != c.want {
			t.Errorf("%v: got %q (%v), want %q", c.urls, m.Avatar, err, c.want)
		}
	}
}

func TestIntroText(t *testing.T) {
	if s, err := DecodeIntro(EncodeBlob(4, []byte("Bora vamos testar"))); err != nil || s != "Bora vamos testar" {
		t.Fatalf("%q %v", s, err)
	}
	if s, err := DecodeIntro(nil); err != nil || s != "" {
		t.Fatalf("%q %v", s, err)
	}
}

func TestGiftSocialAndFanJoined(t *testing.T) {
	gift := append(EncodeVarintField(12, 1), EncodeBlob(16, []byte("Rose"))...)
	payload := EncodeVarintField(2, 5655)
	payload = append(payload, EncodeVarintField(5, 0)...) // a zero repeat count counts as one
	payload = append(payload, EncodeBlob(7, user())...)
	payload = append(payload, EncodeVarintField(9, 1)...)
	payload = append(payload, EncodeVarintField(11, 77)...)
	payload = append(payload, EncodeBlob(15, gift)...)
	g, err := DecodeGift(payload)
	if err != nil || g.Name != "Rose" || g.Coins != 1 || g.Count != 1 || !g.Ended || g.Group != "77" || g.User.Name != "Ana Teste" {
		t.Fatalf("%+v %v", g, err)
	}

	// Common header (field 1) -> display text (field 8) -> key (f1) + a user piece (f1==11, f21.f1) + a string piece (f11).
	userPiece := append(EncodeVarintField(1, 11), EncodeBlob(21, EncodeBlob(1, user()))...)
	text := EncodeBlob(1, []byte("pm_mt_fan_live_joined_comment"))
	text = append(text, EncodeBlob(4, userPiece)...)
	text = append(text, EncodeBlob(4, EncodeBlob(11, []byte("3")))...)
	common := EncodeBlob(8, text)
	fan, err := DecodeFanJoined(EncodeBlob(1, common))
	if err != nil || fan == nil || fan.User.Name != "Ana Teste" || fan.Rank != "3" {
		t.Fatalf("%+v %v", fan, err)
	}
	if other, err := DecodeFanJoined(EncodeBlob(1, EncodeBlob(8, EncodeBlob(1, []byte("other"))))); other != nil || err != nil {
		t.Fatalf("%v %v", other, err)
	}

	social := append(EncodeBlob(1, common), EncodeBlob(2, user())...)
	social = append(social, EncodeVarintField(4, 1)...)
	social = append(social, EncodeVarintField(6, 9)...)
	s, err := DecodeSocial(social)
	if err != nil || s.Action != 1 || s.User == nil || s.User.Name != "Ana Teste" || s.Followers != 9 || s.Key != "pm_mt_fan_live_joined_comment" {
		t.Fatalf("%+v %v", s, err)
	}
	// A social message whose user id is 0 is the host-guidance prompt: no user.
	zero := append(EncodeBlob(2, EncodeVarintField(1, 0)), EncodeVarintField(4, 3)...)
	if s, err := DecodeSocial(zero); err != nil || s.User != nil {
		t.Fatalf("%+v %v", s, err)
	}
}
