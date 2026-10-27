// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// Package room is offline room-response validation and shutdown request planning. No HTTP client, room creation or automatic publication. Push URLs are secrets:
// the types below deliberately have no String methods that would print them, and callers must not log them.
package room

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
)

// ParseJSON decodes with json.Number so identifiers and status codes never go through a float64.
func ParseJSON(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

func invalid(msg string) error { return errors.New(msg) }

// id requires a string of ASCII digits with a positive value: a rounded JavaScript float is never accepted.
func id(value any) (string, error) {
	s, ok := value.(string)
	if !ok || s == "" || strings.Trim(s, "0") == "" {
		return "", invalid("Missing or invalid string identifier")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return "", invalid("Missing or invalid string identifier")
		}
	}
	return s, nil
}

type Binding struct {
	RoomID, PingRoomID, StreamID string
}

type PushTarget struct {
	Protocol string
	server   string
	key      string
	Source   string
}

// String and GoString redact the secret parts, so %v, %+v and %#v never print them (fmt reads unexported fields).
func (p PushTarget) String() string {
	return "PushTarget{" + p.Protocol + ", " + p.Source + ", <redacted>}"
}
func (p PushTarget) GoString() string { return p.String() }

// Server and Key are the secret parts; they are accessors so nothing prints them by accident.
func (p PushTarget) Server() string { return p.server }
func (p PushTarget) Key() string    { return p.key }

type EndPlan struct {
	Endpoint           string
	Body               []byte
	BodyMD5            string
	VerificationRoomID string
}

// BindCreatedRoom validates a room/create response and extracts the three distinct identifiers.
func BindCreatedRoom(response map[string]any) (*Binding, error) {
	status, ok := response["status_code"].(json.Number)
	if !ok || status.String() != "0" {
		return nil, invalid("Room creation was not successful")
	}
	data, ok := response["data"].(map[string]any)
	if !ok {
		return nil, invalid("Missing room data")
	}
	attrs, ok := data["living_room_attrs"].(map[string]any)
	if !ok {
		return nil, invalid("Missing ping room attributes")
	}
	roomID, err := id(data["id_str"])
	if err != nil {
		return nil, err
	}
	pingID, err := id(attrs["room_id_str"])
	if err != nil {
		return nil, err
	}
	streamID, err := id(data["stream_id_str"])
	if err != nil {
		return nil, err
	}
	return &Binding{RoomID: roomID, PingRoomID: pingID, StreamID: streamID}, nil
}

// ExtractPushTargets returns the usable push destinations of a created room, in preference order.
func ExtractPushTargets(room map[string]any) ([]PushTarget, error) {
	stream, ok := room["stream_url"].(map[string]any)
	if !ok {
		return nil, invalid("Missing stream URL data")
	}
	type candidate struct {
		source string
		value  any
	}
	var candidates []candidate
	if urls, ok := stream["push_urls"].([]any); ok && len(urls) > 0 {
		candidates = append(candidates, candidate{"push_urls[0]", urls[0]})
	}
	if v, ok := stream["rtmp_push_url"]; ok && v != nil && v != "" {
		candidates = append(candidates, candidate{"rtmp_push_url", v})
	}
	var targets []PushTarget
	for _, c := range candidates {
		value, ok := c.value.(string)
		if !ok {
			return nil, invalid("Invalid push URL")
		}
		for _, r := range value {
			if r < 33 {
				return nil, invalid("Invalid push URL")
			}
		}
		parsed, err := url.Parse(value)
		valid := err == nil && (parsed.Scheme == "rtmp" || parsed.Scheme == "rtmps" || parsed.Scheme == "rtmpq") &&
			parsed.Hostname() != "" && parsed.User == nil && parsed.Fragment == "" && !strings.Contains(value, "#")
		if !valid || parsed.Path == "" || strings.HasSuffix(parsed.Path, "/") {
			return nil, invalid("Unsupported or incomplete push URL")
		}
		// Matches the observed last-slash split, preserving the query in the key. No pri rewrite: the necessity
		// for an independent RTMP stream remains unproven.
		cut := strings.LastIndex(value, "/")
		server, key := value[:cut], value[cut+1:]
		if key == "" || strings.Contains(parsed.RawQuery, "/") {
			return nil, invalid("Ambiguous push URL key")
		}
		targets = append(targets, PushTarget{Protocol: parsed.Scheme, server: server, key: key, Source: c.source})
	}
	if len(targets) == 0 {
		return nil, invalid("No push destination supplied")
	}
	return targets, nil
}

// PlanEnd builds the exact room/ping/anchor body that ends a broadcast (status 4). The order of the form fields is
// part of the contract (it is what x-ss-stub signs).
func PlanEnd(b Binding) (*EndPlan, error) {
	roomID, err := id(b.RoomID)
	if err != nil {
		return nil, err
	}
	pingID, err := id(b.PingRoomID)
	if err != nil {
		return nil, err
	}
	streamID, err := id(b.StreamID)
	if err != nil {
		return nil, err
	}
	body := []byte("status=4&room_id=" + pingID + "&stream_id=" + streamID)
	sum := md5.Sum(body)
	return &EndPlan{Endpoint: "https://webcast.tiktokv.com/webcast/room/ping/anchor/", Body: body,
		BodyMD5: hex.EncodeToString(sum[:]), VerificationRoomID: roomID}, nil
}

// CreateFields are the form fields for POST /webcast/room/create/, with fixed values only.
// Only values with a verified source are sent: constants that need no user choice, and account
// defaults read from create_info. Fields whose encoding is unverified are left out so the server applies the
// account's own settings. The map keys are the wire names; iteration order is irrelevant for a form.
func CreateFields(title string, defaults map[string]any) (map[string]string, error) {
	trimmed := strings.TrimSpace(title)
	if n := len([]rune(trimmed)); n < 1 || n > 80 {
		return nil, invalid("A title of 1 to 80 characters is required")
	}
	fields := map[string]string{
		"title": trimmed, "live_studio": "1", "gen_replay": "true",
		"close_room_when_close_stream": "false",
		"hashtag_id":                   "0", "game_tag_id": "0", // no topic chosen
		"multi_stream_scene": "0",     // single (non-dual) orientation
		"rtc_net_enabled":    "false", // default 0
	}
	if cover, ok := defaults["cover_uri"].(string); ok && cover != "" && len(cover) <= 256 {
		fields["cover_uri"] = cover
	}
	if n, ok := defaults["screenshot_cover_status"].(json.Number); ok {
		if v, err := strconv.ParseInt(n.String(), 10, 64); err == nil {
			fields["screenshot_cover_status"] = strconv.FormatInt(v, 10)
		}
	}
	if group, ok := defaults["is_group_live_session"].(bool); ok {
		if group {
			fields["is_group_live_session"] = "true"
		} else {
			fields["is_group_live_session"] = "false"
		}
	}
	return fields, nil
}
