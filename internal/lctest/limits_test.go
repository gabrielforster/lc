package lctest

import (
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/gabrielforster/lc/internal/muxproto"
	"github.com/gabrielforster/lc/internal/store"
)

// Once a port is public, bots find it. The cap is what keeps one tunnel's
// traffic from exhausting the home link for every other tunnel.
func TestConnectionCapIsEnforced(t *testing.T) {
	// A service that holds connections open lets the cap be observed.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Held open, never answered.
			t.Cleanup(func() { c.Close() })
		}
	}()

	h := Start(t, Options{
		MaxConnsPerTunnel: 2,
		Grants:            []store.Grant{{Kind: store.GrantPortAuto}},
		Tunnels: []muxproto.TunnelSpec{{
			Name: "capped", Kind: muxproto.KindTCP, LocalAddr: ln.Addr().String(),
		}},
	})
	addr := fmt.Sprintf("127.0.0.1:%d", h.PublicPort(t, "capped"))

	var held []net.Conn
	for i := range 2 {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("connection %d within the cap was refused: %v", i, err)
		}
		t.Cleanup(func() { c.Close() })
		held = append(held, c)
	}
	// Give the server time to register both before testing the third.
	time.Sleep(200 * time.Millisecond)

	over, err := net.Dial("tcp", addr)
	if err != nil {
		return // refused at accept is a valid way to enforce the cap
	}
	defer over.Close()

	// Otherwise the server accepts then immediately closes, which the client
	// sees as EOF rather than data.
	over.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := over.Read(make([]byte, 1))
	if err == nil && n > 0 {
		t.Fatal("connection beyond the cap was served")
	}

	// Releasing a slot must let a new connection through, or the cap would be
	// a permanent ceiling rather than a concurrency limit.
	held[0].Close()
	time.Sleep(200 * time.Millisecond)

	again, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial after a slot freed: %v", err)
	}
	defer again.Close()
	if _, err := again.Write([]byte("x")); err != nil {
		t.Fatalf("write after a slot freed: %v", err)
	}
}

// An unlimited tunnel must not be accidentally capped at zero.
func TestZeroCapMeansUnlimited(t *testing.T) {
	h := Start(t, Options{
		Grants: []store.Grant{{Kind: store.GrantPortAuto}},
		Tunnels: []muxproto.TunnelSpec{{
			Name: "echo", Kind: muxproto.KindTCP, LocalAddr: EchoService(t),
		}},
	})
	addr := fmt.Sprintf("127.0.0.1:%d", h.PublicPort(t, "echo"))

	for i := range 5 {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("connection %d refused with no cap configured: %v", i, err)
		}
		c.Write([]byte("ping"))
		c.(*net.TCPConn).CloseWrite()
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		got, err := io.ReadAll(c)
		if err != nil || string(got) != "ping" {
			t.Fatalf("connection %d: got %q, err %v", i, got, err)
		}
		c.Close()
	}
}
