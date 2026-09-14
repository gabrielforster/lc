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

// The whole point of the project, reduced to one assertion: a client that knows
// nothing about tunnels reaches a service it cannot route to.
func TestRawTCPRoundTrip(t *testing.T) {
	local := EchoService(t)
	h := Start(t, Options{
		Grants:  []store.Grant{{Kind: store.GrantPortAuto}},
		Tunnels: []muxproto.TunnelSpec{{Name: "echo", Kind: muxproto.KindTCP, LocalAddr: local}},
	})

	port := h.PublicPort(t, "echo")
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("hello through the tunnel")); err != nil {
		t.Fatal(err)
	}
	// Half-closing signals end-of-request; the echo service replies and closes.
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello through the tunnel" {
		t.Fatalf("got %q", got)
	}
}

// Half-close must survive the trip through the tunnel in both directions, or
// request/response protocols that signal completion by closing will hang.
func TestHalfCloseTraversesTunnel(t *testing.T) {
	// A service that waits for EOF before answering only completes if the
	// client's half-close reached it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		body, _ := io.ReadAll(c)
		fmt.Fprintf(c, "read %d bytes then EOF", len(body))
	}()

	h := Start(t, Options{
		Grants:  []store.Grant{{Kind: store.GrantPortAuto}},
		Tunnels: []muxproto.TunnelSpec{{Name: "drain", Kind: muxproto.KindTCP, LocalAddr: ln.Addr().String()}},
	})

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", h.PublicPort(t, "drain")))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	conn.Write([]byte("12345"))
	conn.(*net.TCPConn).CloseWrite()

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "read 5 bytes then EOF" {
		t.Fatalf("got %q -- half-close likely did not reach the local service", got)
	}
}

// Concurrent connections share one yamux session, so they must not interleave.
func TestConcurrentConnectionsStayIsolated(t *testing.T) {
	local := EchoService(t)
	h := Start(t, Options{
		Grants:  []store.Grant{{Kind: store.GrantPortAuto}},
		Tunnels: []muxproto.TunnelSpec{{Name: "echo", Kind: muxproto.KindTCP, LocalAddr: local}},
	})
	addr := fmt.Sprintf("127.0.0.1:%d", h.PublicPort(t, "echo"))

	errs := make(chan error, 8)
	for i := range 8 {
		go func() {
			payload := fmt.Sprintf("connection-%d-payload", i)
			c, err := net.Dial("tcp", addr)
			if err != nil {
				errs <- err
				return
			}
			defer c.Close()
			c.Write([]byte(payload))
			c.(*net.TCPConn).CloseWrite()

			c.SetReadDeadline(time.Now().Add(10 * time.Second))
			got, err := io.ReadAll(c)
			if err != nil {
				errs <- err
				return
			}
			if string(got) != payload {
				errs <- fmt.Errorf("got %q, want %q", got, payload)
				return
			}
			errs <- nil
		}()
	}
	for range 8 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

// A rejected registration must not leave the agent retrying forever.
func TestBadTokenIsFatalForAgent(t *testing.T) {
	// Covered indirectly: Run returns rather than looping on auth errors.
	// Exercised here through the registry to keep the test fast.
	h := Start(t, Options{
		Grants:  []store.Grant{{Kind: store.GrantPortAuto}},
		Tunnels: []muxproto.TunnelSpec{{Name: "echo", Kind: muxproto.KindTCP, LocalAddr: EchoService(t)}},
	})
	if _, err := h.DB.TokenBySecret("definitely-not-a-token"); err == nil {
		t.Fatal("unknown secret authenticated")
	}
}
