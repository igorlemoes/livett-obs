// SPDX-License-Identifier: GPL-2.0-or-later
// Copyright (C) 2026 Igor Lemões

package ws

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func wsURL(s *httptest.Server) string { return "ws" + strings.TrimPrefix(s.URL, "http") + "/x" }

func echoServer(t *testing.T, behave func(*Conn)) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(w, r)
		if err != nil {
			return
		}
		defer c.Close()
		behave(c)
	}))
	t.Cleanup(s.Close)
	return s
}

func dial(t *testing.T, s *httptest.Server) *Conn {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := Dial(ctx, wsURL(s))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestEchoesTextAndLargeBinaryMessages(t *testing.T) {
	s := echoServer(t, func(c *Conn) {
		for {
			op, msg, err := c.Read()
			if err != nil {
				return
			}
			c.Write(op, msg)
		}
	})
	c := dial(t, s)
	for _, size := range []int{0, 5, 125, 126, 70000, 300000} {
		payload := bytes.Repeat([]byte("a"), size)
		if err := c.Write(OpBinary, payload); err != nil {
			t.Fatal(err)
		}
		op, got, err := c.Read()
		if err != nil || op != OpBinary || !bytes.Equal(got, payload) {
			t.Fatalf("size %d: op=%d len=%d err=%v", size, op, len(got), err)
		}
	}
}

func TestFragmentedMessageIsReassembledAndPingIsAnswered(t *testing.T) {
	pong := make(chan string, 1)
	s := echoServer(t, func(c *Conn) {
		c.nc.Write([]byte{0x01, 0x03, 'h', 'e', 'l'}) // text, not final
		c.writeFrame(opPing, []byte("p"))             // a control frame may interleave
		c.nc.Write([]byte{0x80, 0x02, 'l', 'o'})      // continuation, final
		fin, op, payload, err := c.readFrame()
		if err == nil && fin && op == opPong {
			pong <- string(payload)
		}
	})
	c := dial(t, s)
	op, msg, err := c.Read()
	if err != nil || op != OpText || string(msg) != "hello" {
		t.Fatalf("%d %q %v", op, msg, err)
	}
	select {
	case p := <-pong:
		if p != "p" {
			t.Fatalf("pong %q", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ping was not answered")
	}
}

func TestCloseFrameEndsTheConnection(t *testing.T) {
	s := echoServer(t, func(c *Conn) { c.writeFrame(opClose, nil); time.Sleep(100 * time.Millisecond) })
	c := dial(t, s)
	if _, _, err := c.Read(); err != ErrClosed {
		t.Fatalf("want ErrClosed, got %v", err)
	}
}

func TestOnlyLoopbackWsEndpointsAreAllowed(t *testing.T) {
	for _, u := range []string{"ws://example.com:80/x", "ws://192.168.0.5:4455/", "wss://127.0.0.1/x", "http://127.0.0.1/x", "ws://"} {
		if _, err := Dial(context.Background(), u); err == nil {
			t.Errorf("%s must be refused", u)
		}
	}
}

func TestHandshakeRefusalIsAnError(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 403) }))
	defer s.Close()
	if _, err := Dial(context.Background(), wsURL(s)); err == nil {
		t.Fatal("a 403 must fail the handshake")
	}
}

func TestOversizedMessageIsRejected(t *testing.T) {
	old := MaxMessage
	_ = old
	s := echoServer(t, func(c *Conn) {
		c.nc.Write([]byte{0x82, 127, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	})
	c := dial(t, s)
	if _, _, err := c.Read(); err == nil {
		t.Fatal("a frame header announcing an absurd size must be rejected")
	}
}
