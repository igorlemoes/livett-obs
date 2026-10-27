// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

// Package ws is a minimal RFC 6455 WebSocket implementation: a client for the loopback OBS and Chrome (CDP)
// endpoints and a tiny server-side Accept used by tests. Plain ws:// only, and only to loopback hosts.
package ws

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	OpText   = 1
	OpBinary = 2
	opClose  = 8
	opPing   = 9
	opPong   = 10

	// MaxMessage bounds one reassembled message (Chrome can return multi-megabyte CDP responses).
	MaxMessage = 64 << 20
)

const guid = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

var ErrClosed = errors.New("websocket closed")

type Conn struct {
	nc     net.Conn
	br     *bufio.Reader
	client bool // clients mask outgoing frames
	wmu    sync.Mutex
	closed bool
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Dial connects to ws://host:port/path on a loopback host.
func Dial(ctx context.Context, rawurl string) (*Conn, error) {
	u, err := url.Parse(rawurl)
	if err != nil || u.Scheme != "ws" || u.Host == "" {
		return nil, fmt.Errorf("unsupported websocket url")
	}
	if !isLoopback(u.Hostname()) {
		return nil, fmt.Errorf("only loopback websocket endpoints are supported")
	}
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		nc.SetDeadline(deadline)
	}
	keyBytes := make([]byte, 16)
	rand.Read(keyBytes)
	key := base64.StdEncoding.EncodeToString(keyBytes)
	path := u.RequestURI()
	// Origin is deliberately absent: Chrome's --remote-allow-origins only restricts browser origins.
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
		path, u.Host, key)
	if _, err := io.WriteString(nc, req); err != nil {
		nc.Close()
		return nil, err
	}
	br := bufio.NewReader(nc)
	resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
	if err != nil {
		nc.Close()
		return nil, err
	}
	if resp.StatusCode != 101 || resp.Header.Get("Sec-WebSocket-Accept") != accept(key) {
		nc.Close()
		return nil, fmt.Errorf("websocket handshake refused (%s)", resp.Status)
	}
	nc.SetDeadline(time.Time{})
	return &Conn{nc: nc, br: br, client: true}, nil
}

func accept(key string) string {
	sum := sha1.Sum([]byte(key + guid))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// Accept upgrades an incoming request (server side; used by the tests' fake OBS/Chrome).
func Accept(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	key := r.Header.Get("Sec-WebSocket-Key")
	hj, ok := w.(http.Hijacker)
	if !ok || key == "" || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "websocket required", http.StatusBadRequest)
		return nil, errors.New("not a websocket request")
	}
	nc, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept(key))
	if err := rw.Flush(); err != nil {
		nc.Close()
		return nil, err
	}
	return &Conn{nc: nc, br: rw.Reader}, nil
}

func (c *Conn) SetReadDeadline(t time.Time) error { return c.nc.SetReadDeadline(t) }

// Write sends one message (OpText or OpBinary).
func (c *Conn) Write(op int, payload []byte) error {
	return c.writeFrame(byte(op), payload)
}

func (c *Conn) writeFrame(op byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return ErrClosed
	}
	header := []byte{0x80 | op}
	maskBit := byte(0)
	if c.client {
		maskBit = 0x80
	}
	switch n := len(payload); {
	case n < 126:
		header = append(header, maskBit|byte(n))
	case n < 1<<16:
		header = append(header, maskBit|126, byte(n>>8), byte(n))
	default:
		header = append(header, maskBit|127)
		header = binary.BigEndian.AppendUint64(header, uint64(n))
	}
	body := payload
	if c.client {
		mask := make([]byte, 4)
		rand.Read(mask)
		header = append(header, mask...)
		body = make([]byte, len(payload))
		for i, b := range payload {
			body[i] = b ^ mask[i%4]
		}
	}
	if _, err := c.nc.Write(append(header, body...)); err != nil {
		return err
	}
	return nil
}

// Read returns the next complete text/binary message. Pings are answered, pongs ignored, and a close frame
// ends the connection with ErrClosed.
func (c *Conn) Read() (int, []byte, error) {
	var message []byte
	messageOp := 0
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch op {
		case opPing:
			c.writeFrame(opPong, payload)
			continue
		case opPong:
			continue
		case opClose:
			c.writeFrame(opClose, payload)
			c.Close()
			return 0, nil, ErrClosed
		case 0: // continuation
			if messageOp == 0 {
				return 0, nil, errors.New("unexpected continuation frame")
			}
		case OpText, OpBinary:
			if messageOp != 0 {
				return 0, nil, errors.New("interleaved data frames")
			}
			messageOp = int(op)
		default:
			return 0, nil, errors.New("unsupported websocket opcode")
		}
		if len(message)+len(payload) > MaxMessage {
			return 0, nil, errors.New("websocket message exceeds size limit")
		}
		message = append(message, payload...)
		if fin {
			return messageOp, message, nil
		}
	}
}

func (c *Conn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var head [2]byte
	if _, err = io.ReadFull(c.br, head[:]); err != nil {
		return
	}
	fin, op = head[0]&0x80 != 0, head[0]&0x0f
	masked := head[1]&0x80 != 0
	size := uint64(head[1] & 0x7f)
	switch size {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(c.br, b[:]); err != nil {
			return
		}
		size = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(c.br, b[:]); err != nil {
			return
		}
		size = binary.BigEndian.Uint64(b[:])
	}
	if size > MaxMessage {
		err = errors.New("websocket frame exceeds size limit")
		return
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(c.br, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, size)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return
}

// Close sends a close frame (best effort) and drops the connection.
func (c *Conn) Close() error {
	c.wmu.Lock()
	if c.closed {
		c.wmu.Unlock()
		return nil
	}
	c.closed = true
	c.wmu.Unlock()
	return c.nc.Close() // the peer learns of it from the TCP close
}
