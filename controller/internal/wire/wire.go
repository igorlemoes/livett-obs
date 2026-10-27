// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// Package wire is a bounded subset of the LIVE IM wire format used by the LIVE service.
// No authentication or network access. Decoding is NOT proof of sender
// authenticity, and every text field is untrusted: render it as text only.
package wire

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"tiktoklive/internal/guard"
)

const MaxFrame = 2 * 1024 * 1024

// MaxDecoded is a variable only so tests can lower it.
var MaxDecoded = 4 * 1024 * 1024

// Error is returned for any malformed or out-of-bounds input.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func fail(msg string) error { return &Error{msg} }

// IsError reports whether err is a wire decoding error.
func IsError(err error) bool {
	var w *Error
	return errors.As(err, &w)
}

func varint(data []byte, at int) (uint64, int, error) {
	var value uint64
	for shift := uint(0); shift < 70; shift += 7 {
		if at >= len(data) {
			return 0, at, fail("Truncated varint")
		}
		b := data[at]
		at++
		if shift == 63 && b > 1 {
			return 0, at, fail("Varint exceeds uint64")
		}
		value |= uint64(b&127) << shift
		if b < 128 {
			return value, at, nil
		}
	}
	return 0, at, fail("Invalid varint")
}

// Field is one decoded field occurrence. Wire 0 uses Num; wires 1, 2 and 5 use Data.
type Field struct {
	Wire int
	Num  uint64
	Data []byte
}

// Fields maps a field number to its occurrences, in order.
type Fields map[uint64][]Field

func ParseFields(data []byte) (Fields, error) {
	if len(data) > MaxDecoded {
		return nil, fail("Message exceeds size limit")
	}
	result := Fields{}
	at := 0
	for at < len(data) {
		tag, next, err := varint(data, at)
		if err != nil {
			return nil, err
		}
		at = next
		number, wire := tag>>3, int(tag&7)
		if number == 0 || number >= 1<<29 {
			return nil, fail("Invalid field number")
		}
		switch wire {
		case 0:
			value, next, err := varint(data, at)
			if err != nil {
				return nil, err
			}
			at = next
			result[number] = append(result[number], Field{Wire: 0, Num: value})
		case 1, 2, 5:
			var size uint64
			switch wire {
			case 2:
				size, next, err = varint(data, at)
				if err != nil {
					return nil, err
				}
				at = next
			case 1:
				size = 8
			default:
				size = 4
			}
			if size > uint64(len(data)-at) {
				return nil, fail("Truncated field")
			}
			result[number] = append(result[number], Field{Wire: wire, Data: data[at : at+int(size)]})
			at += int(size)
		default:
			return nil, fail("Unsupported wire type")
		}
	}
	return result, nil
}

// last returns the last occurrence of a field, or nil when absent; a wire-type mismatch is an error.
func (f Fields) last(number uint64, wire int) (*Field, error) {
	values := f[number]
	if len(values) == 0 {
		return nil, nil
	}
	v := values[len(values)-1]
	if v.Wire != wire {
		return nil, fail("Unexpected field type")
	}
	return &v, nil
}

func (f Fields) Blob(number uint64) ([]byte, error) {
	v, err := f.last(number, 2)
	if v == nil {
		return nil, err
	}
	return v.Data, nil
}

func (f Fields) Text(number uint64) (string, error) {
	b, err := f.Blob(number)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(b) {
		return "", fail("Invalid UTF-8")
	}
	return string(b), nil
}

func (f Fields) Uint(number uint64) (uint64, error) {
	v, err := f.last(number, 0)
	if v == nil {
		return 0, err
	}
	return v.Num, nil
}

func (f Fields) Has(number uint64) bool { return len(f[number]) > 0 }

func (f Fields) repeatedBlobs(number uint64) ([][]byte, error) {
	var out [][]byte
	for _, v := range f[number] {
		if v.Wire != 2 {
			return nil, fail("Unexpected repeated field type")
		}
		out = append(out, v.Data)
	}
	return out, nil
}

// firstErr keeps the first error so decoders can read many fields and check once.
type firstErr struct{ err error }

func (e *firstErr) text(f Fields, n uint64) string {
	s, err := f.Text(n)
	e.set(err)
	return s
}
func (e *firstErr) uint(f Fields, n uint64) uint64 {
	v, err := f.Uint(n)
	e.set(err)
	return v
}
func (e *firstErr) blob(f Fields, n uint64) []byte {
	b, err := f.Blob(n)
	e.set(err)
	return b
}
func (e *firstErr) fields(data []byte) Fields {
	f, err := ParseFields(data)
	e.set(err)
	if f == nil {
		f = Fields{}
	}
	return f
}
func (e *firstErr) set(err error) {
	if e.err == nil {
		e.err = err
	}
}

type Message struct {
	Method    string
	Payload   []byte
	MessageID string
	History   bool
}

type Response struct {
	Messages      []Message
	Cursor        string
	InternalExt   string
	NeedAck       bool
	FetchType     uint64
	FetchInterval uint64
	PushServer    string
}

func DecodeResponse(data []byte) (*Response, error) {
	var e firstErr
	response := e.fields(data)
	if e.err != nil {
		return nil, e.err
	}
	raws, err := response.repeatedBlobs(1)
	if err != nil {
		return nil, err
	}
	var messages []Message
	for _, raw := range raws {
		m := e.fields(raw)
		if e.err != nil {
			return nil, e.err
		}
		messages = append(messages, Message{e.text(m, 1), e.blob(m, 2), strconv.FormatUint(e.uint(m, 3), 10), e.uint(m, 6) != 0})
	}
	r := &Response{Messages: messages, Cursor: e.text(response, 2), InternalExt: e.text(response, 5),
		NeedAck: e.uint(response, 9) != 0, FetchType: e.uint(response, 6), FetchInterval: e.uint(response, 3),
		PushServer: e.text(response, 10)}
	if e.err != nil {
		return nil, e.err
	}
	return r, nil
}

func encodeVarint(value uint64) []byte {
	var out []byte
	for value > 127 {
		out = append(out, byte(value&127)|128)
		value >>= 7
	}
	return append(out, byte(value))
}

func EncodeBlob(number uint64, value []byte) []byte {
	out := encodeVarint(number<<3 | 2)
	out = append(out, encodeVarint(uint64(len(value)))...)
	return append(out, value...)
}

// EncodeVarintField encodes a varint field (used by tests and the ack builder).
func EncodeVarintField(number, value uint64) []byte {
	return append(encodeVarint(number<<3), encodeVarint(value)...)
}

func gunzipBounded(payload []byte) ([]byte, error) {
	src := bytes.NewReader(payload)
	reader, err := gzip.NewReader(src)
	if err != nil {
		return nil, fail("Invalid gzip")
	}
	reader.Multistream(false)
	out, err := io.ReadAll(io.LimitReader(reader, int64(MaxDecoded)+1))
	if err != nil {
		return nil, fail("Invalid gzip")
	}
	if len(out) > MaxDecoded || src.Len() != 0 {
		return nil, fail("Compressed payload exceeds limit or is incomplete")
	}
	return out, nil
}

// DecodePush returns the payload type, the decoded response (kind "msg" only) and the ack frame to send back.
func DecodePush(data []byte) (string, *Response, []byte, error) {
	if len(data) > MaxFrame {
		return "", nil, nil, fail("Frame exceeds size limit")
	}
	var e firstErr
	frame := e.fields(data)
	kind := e.text(frame, 7)
	if e.err != nil {
		return "", nil, nil, e.err
	}
	if kind != "msg" {
		return kind, nil, nil, nil
	}
	headers := map[string]string{}
	rawHeaders, err := frame.repeatedBlobs(5)
	if err != nil {
		return "", nil, nil, err
	}
	for _, raw := range rawHeaders {
		h := e.fields(raw)
		headers[e.text(h, 1)] = e.text(h, 2)
	}
	payload := e.blob(frame, 8)
	if e.err != nil {
		return "", nil, nil, e.err
	}
	switch compression := headers["compress_type"]; compression {
	case "":
	case "gzip":
		if payload, err = gunzipBounded(payload); err != nil {
			return "", nil, nil, err
		}
	default:
		return "", nil, nil, fail("Unsupported compression")
	}
	response, err := DecodeResponse(payload)
	if err != nil {
		return "", nil, nil, err
	}
	var ack []byte
	if response.NeedAck {
		logid := e.uint(frame, 2)
		ack = append(encodeVarint(2<<3), encodeVarint(logid)...)
		ack = append(ack, EncodeBlob(7, []byte("ack"))...)
		ack = append(ack, EncodeBlob(8, []byte(response.InternalExt))...)
	}
	return kind, response, ack, e.err
}

// Classify is a limited classification only; the caller must first establish a trusted session.
// Perception/Bottom/Recall stay unclassified until their semantics are mapped. Returns nil for "no signal".
func Classify(m Message, sessionID string) (*guard.Signal, error) {
	if m.History || m.MessageID == "0" {
		return nil, nil
	}
	method := strings.TrimPrefix(m.Method, "Webcast")
	if method != "ControlMessage" && method != "NoticeMessage" {
		return nil, nil
	}
	var e firstErr
	payload := e.fields(m.Payload)
	common := e.fields(e.blob(payload, 1))
	roomID := strconv.FormatUint(e.uint(common, 3), 10)
	if e.err != nil {
		return nil, e.err
	}
	if roomID == "0" {
		return nil, nil
	}
	var kind guard.SignalKind
	if method == "ControlMessage" {
		if action := e.uint(payload, 2); action == 3 || action == 4 {
			kind = guard.RemoteEnd
		}
	} else {
		for _, n := range []uint64{2, 5, 11} {
			if len(e.blob(payload, n)) > 0 {
				kind = guard.Warning
				break
			}
		}
	}
	if e.err != nil {
		return nil, e.err
	}
	if kind == "" {
		return nil, nil
	}
	return &guard.Signal{RoomID: roomID, SessionID: sessionID, Kind: kind, EvidenceID: "im:" + m.MessageID}, nil
}

// --- Display decoders. Only layouts observed in real traces; each is bounded and returns an *Error on malformed data.

var avatarHost = regexp.MustCompile(`^[a-z0-9-]+(\.[a-z0-9-]+)*\.tiktokcdn(-us|-eu)?\.com$`)

// truncate cuts to n characters (not bytes), counting runes.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// avatar returns the first avatar URL on a TikTok CDN host (https only); anything else is ignored.
func avatar(data []byte) string {
	f, err := ParseFields(data)
	if err != nil {
		return ""
	}
	raws, err := f.repeatedBlobs(1)
	if err != nil {
		return ""
	}
	for _, raw := range raws {
		if !utf8.Valid(raw) {
			return ""
		}
		u := string(raw)
		parts, err := url.Parse(u)
		if err != nil {
			return ""
		}
		if parts.Scheme == "https" && avatarHost.MatchString(parts.Hostname()) && utf8.RuneCountInString(u) <= 600 {
			return u
		}
	}
	return ""
}

type User struct {
	ID, Name, Handle, Avatar string
}

func decodeUser(data []byte) (User, error) {
	var e firstErr
	f := e.fields(data)
	u := User{ID: strconv.FormatUint(e.uint(f, 1), 10), Name: truncate(e.text(f, 3), 80), Handle: truncate(e.text(f, 38), 60),
		Avatar: avatar(e.blob(f, 9))}
	return u, e.err
}

type Chat struct {
	User User
	Text string
}

func DecodeChat(payload []byte) (*Chat, error) {
	var e firstErr
	m := e.fields(payload)
	user, err := decodeUser(e.blob(m, 2))
	e.set(err)
	text := truncate(e.text(m, 3), 300)
	if e.err != nil {
		return nil, e.err
	}
	return &Chat{User: user, Text: text}, nil
}

func DecodeMember(payload []byte) (*User, error) {
	var e firstErr
	m := e.fields(payload)
	user, err := decodeUser(e.blob(m, 2))
	e.set(err)
	if e.err != nil {
		return nil, e.err
	}
	return &user, nil
}

type RoomUsers struct{ Viewers, Total uint64 }

// DecodeRoomUsers reads RoomUserSeq: field 3 = current viewers, field 7 = total users seen (0 when absent).
func DecodeRoomUsers(payload []byte) (*RoomUsers, error) {
	var e firstErr
	m := e.fields(payload)
	r := &RoomUsers{e.uint(m, 3), e.uint(m, 7)}
	if e.err != nil {
		return nil, e.err
	}
	return r, nil
}

type Like struct{ Count, Total uint64 }

// DecodeLike reads LikeMessage: field 2 = likes in this burst, field 3 = running total.
func DecodeLike(payload []byte) (*Like, error) {
	var e firstErr
	m := e.fields(payload)
	l := &Like{e.uint(m, 2), e.uint(m, 3)}
	if e.err != nil {
		return nil, e.err
	}
	return l, nil
}

type Gift struct {
	User   User
	GiftID string
	Group  string
	Count  uint64
	Ended  bool
	Name   string
	Coins  uint64
}

// DecodeGift reads GiftMessage: f2 gift id, f5 repeat count, f7 sender, f9 repeat_end (a combo streams several
// messages and the last one carries 1), f11 group id, f15 gift struct (f12 coin price, f16 name).
func DecodeGift(payload []byte) (*Gift, error) {
	var e firstErr
	m := e.fields(payload)
	gift := e.fields(e.blob(m, 15))
	user, err := decodeUser(e.blob(m, 7))
	e.set(err)
	count := e.uint(m, 5)
	if count < 1 {
		count = 1
	}
	g := &Gift{User: user, GiftID: strconv.FormatUint(e.uint(m, 2), 10), Group: strconv.FormatUint(e.uint(m, 11), 10),
		Count: count, Ended: e.uint(m, 9) != 0, Name: truncate(e.text(gift, 16), 60), Coins: e.uint(gift, 12)}
	if e.err != nil {
		return nil, e.err
	}
	return g, nil
}

type display struct {
	Key     string
	Users   []User
	Strings []string
}

// decodeDisplay reads the localised display text of a message: i18n key, mentioned users and plain-string pieces.
// Malformed data yields an empty display, never an error .
func decodeDisplay(commonPayload []byte) display {
	var e firstErr
	common := e.fields(commonPayload)
	text := e.fields(e.blob(common, 8))
	key := e.text(text, 1)
	pieces, err := text.repeatedBlobs(4)
	e.set(err)
	var users []User
	var strs []string
	for _, raw := range pieces {
		piece := e.fields(raw)
		if e.uint(piece, 1) == 11 {
			inner := e.fields(e.blob(piece, 21))
			u, err := decodeUser(e.blob(inner, 1))
			e.set(err)
			users = append(users, u)
		} else if piece.Has(11) {
			strs = append(strs, truncate(e.text(piece, 11), 40))
		}
	}
	if e.err != nil {
		return display{}
	}
	return display{key, users, strs}
}

type Social struct {
	Action    uint64
	Key       string
	User      *User
	Followers uint64
}

// DecodeSocial reads SocialMessage: f4 action (1 follow, 3 share), f2 acting user; the i18n key in the common
// header tells apart a real event from the host-guidance prompt (which carries no user).
func DecodeSocial(payload []byte) (*Social, error) {
	var e firstErr
	m := e.fields(payload)
	d := decodeDisplay(e.blob(m, 1))
	var user *User
	if m.Has(2) {
		u, err := decodeUser(e.blob(m, 2))
		e.set(err)
		if u.ID != "0" {
			user = &u
		}
	}
	s := &Social{Action: e.uint(m, 4), Key: d.Key, User: user, Followers: e.uint(m, 6)}
	if e.err != nil {
		return nil, e.err
	}
	return s, nil
}

type FanJoined struct {
	User User
	Rank string
}

// DecodeFanJoined reads RoomMessage "pm_mt_fan_live_joined_comment": the Nth fan joined the Fan Club.
// Returns (nil, nil) for any other room message.
func DecodeFanJoined(payload []byte) (*FanJoined, error) {
	var e firstErr
	m := e.fields(payload)
	d := decodeDisplay(e.blob(m, 1))
	if e.err != nil {
		return nil, e.err
	}
	if d.Key != "pm_mt_fan_live_joined_comment" || len(d.Users) == 0 {
		return nil, nil
	}
	rank := ""
	if len(d.Strings) > 0 {
		rank = d.Strings[0]
	}
	return &FanJoined{User: d.Users[0], Rank: rank}, nil
}

// DecodeIntro reads LiveIntroMessage: field 4 is the host's introduction text.
func DecodeIntro(payload []byte) (string, error) {
	var e firstErr
	m := e.fields(payload)
	text := truncate(e.text(m, 4), 240)
	return text, e.err
}
