package lctest

import (
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/gabrielforster/lc/internal/muxproto"
	"github.com/gabrielforster/lc/internal/store"
)

// A peer that goes quiet must be reclaimed, and the slot it held must go back
// to the tunnel -- otherwise the connection cap slowly fills with connections
// nobody is using.
func TestIdleConnectionIsReclaimed(t *testing.T) {
	h := Start(t, Options{
		IdleTimeout: 300 * time.Millisecond,
		Grants:      []store.Grant{{Kind: store.GrantPortAuto}},
		Tunnels: []muxproto.TunnelSpec{{
			Name: "echo", Kind: muxproto.KindTCP, LocalAddr: EchoService(t),
		}},
	})

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", h.PublicPort(t, "echo")))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Say nothing, as a vanished peer would.
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadAll(conn); err != nil {
		t.Fatalf("idle connection was not closed: %v", err)
	}
}

// The timeout must never interrupt a connection that is being used, however
// long it stays open.
func TestActiveConnectionOutlivesIdleTimeout(t *testing.T) {
	h := Start(t, Options{
		IdleTimeout: 300 * time.Millisecond,
		Grants:      []store.Grant{{Kind: store.GrantPortAuto}},
		Tunnels: []muxproto.TunnelSpec{{
			Name: "echo", Kind: muxproto.KindTCP, LocalAddr: EchoService(t),
		}},
	})

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", h.PublicPort(t, "echo")))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Several multiples of the timeout, with traffic throughout.
	buf := make([]byte, 4)
	for i := range 8 {
		if _, err := conn.Write([]byte("ping")); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Fatalf("read %d on an active connection: %v", i, err)
		}
		if string(buf) != "ping" {
			t.Fatalf("read %d got %q", i, buf)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Reclaiming an idle connection must free its slot against the cap.
func TestIdleReclaimFreesCapSlot(t *testing.T) {
	h := Start(t, Options{
		IdleTimeout:       300 * time.Millisecond,
		MaxConnsPerTunnel: 1,
		Grants:            []store.Grant{{Kind: store.GrantPortAuto}},
		Tunnels: []muxproto.TunnelSpec{{
			Name: "echo", Kind: muxproto.KindTCP, LocalAddr: EchoService(t),
		}},
	})
	addr := fmt.Sprintf("127.0.0.1:%d", h.PublicPort(t, "echo"))

	// Occupy the only slot, then abandon it.
	first, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	time.Sleep(100 * time.Millisecond)

	// Once the idle connection is reclaimed, the slot must be usable again.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("cap slot was never freed by the idle timeout")
		}
		second, err := net.Dial("tcp", addr)
		if err == nil {
			second.Write([]byte("ok"))
			second.SetReadDeadline(time.Now().Add(2 * time.Second))
			got := make([]byte, 2)
			if _, err := io.ReadFull(second, got); err == nil && string(got) == "ok" {
				second.Close()
				return
			}
			second.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// blackHoleService accepts connections and then neither sends nor closes,
// standing in for a local service that holds a connection open -- a database,
// an idle shell, an HTTP server with keep-alive.
func blackHoleService(t *testing.T) string {
	t.Helper()
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
			t.Cleanup(func() { c.Close() })
		}
	}()
	return ln.Addr().String()
}

// Reclaiming must not depend on the local service noticing anything.
//
// A half-close only tells the local service the client stopped sending; a
// service that holds the connection open never reacts, so if teardown waits for
// it the connection is never actually reclaimed.
func TestIdleReclaimWhenLocalServiceHoldsOn(t *testing.T) {
	h := Start(t, Options{
		IdleTimeout: 300 * time.Millisecond,
		Grants:      []store.Grant{{Kind: store.GrantPortAuto}},
		Tunnels: []muxproto.TunnelSpec{{
			Name: "hold", Kind: muxproto.KindTCP, LocalAddr: blackHoleService(t),
		}},
	})

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", h.PublicPort(t, "hold")))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadAll(conn); err != nil {
		t.Fatalf("idle connection was not reclaimed: %v", err)
	}
}

// The server must reclaim on its own, without help from the agent's timeout.
//
// Otherwise a connection is only half-closed: the public side is still held,
// and its slot against the connection cap is never released, which is exactly
// the leak the timeout exists to prevent.
func TestServerReclaimsWithoutAgentTimeout(t *testing.T) {
	h := Start(t, Options{
		IdleTimeout:        300 * time.Millisecond,
		NoAgentIdleTimeout: true,
		MaxConnsPerTunnel:  1,
		Grants:             []store.Grant{{Kind: store.GrantPortAuto}},
		Tunnels: []muxproto.TunnelSpec{{
			Name: "hold", Kind: muxproto.KindTCP, LocalAddr: blackHoleService(t),
		}},
	})
	addr := fmt.Sprintf("127.0.0.1:%d", h.PublicPort(t, "hold"))

	first, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	// The cap is 1, so the slot must come back before another connection can
	// be served. That only happens once the first is fully torn down.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("cap slot never freed: the idle connection was not fully reclaimed")
		}
		second, err := net.Dial("tcp", addr)
		if err == nil {
			// The read deadline must be shorter than the idle timeout, or this
			// connection is itself reclaimed first and success is
			// indistinguishable from refusal.
			second.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			_, err := second.Read(make([]byte, 1))
			second.Close()
			// A served connection reaches the black hole, which sends nothing,
			// so the read times out. A refused one was closed by the server and
			// gives EOF immediately.
			if err != nil && !errors.Is(err, io.EOF) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}
