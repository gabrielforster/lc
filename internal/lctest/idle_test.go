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
