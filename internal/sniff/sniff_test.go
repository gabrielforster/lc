package sniff

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/gabrielforster/lc/internal/mcproto"
	"github.com/gabrielforster/lc/internal/netutil"
)

// feed writes b to one end of a real socket and returns a PeekConn on the other.
func feed(t *testing.T, b []byte) *netutil.PeekConn {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			return
		}
		c.Write(b)
		// The connection stays open, so a sniffer that over-reads blocks rather
		// than seeing a convenient EOF.
		time.AfterFunc(5*time.Second, func() { c.Close() })
	}()

	conn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	return netutil.NewPeekConn(conn)
}

func TestMinecraftSniffsHostname(t *testing.T) {
	h := &mcproto.Handshake{
		ProtocolVersion: 765,
		ServerAddress:   "play.mc.example.com",
		ServerPort:      25565,
		NextState:       mcproto.StateLogin,
	}
	c := feed(t, h.Encode())

	got, err := Minecraft{}.Key(c)
	if err != nil {
		t.Fatal(err)
	}
	if got != "play.mc.example.com" {
		t.Fatalf("key = %q", got)
	}

	// The bytes must still be there for the backend; consuming them would send
	// a truncated handshake downstream.
	replayed, err := mcproto.ReadHandshake(newBufio(c))
	if err != nil {
		t.Fatalf("handshake was consumed by sniffing: %v", err)
	}
	if replayed.ServerAddress != h.ServerAddress {
		t.Fatalf("replayed address = %q", replayed.ServerAddress)
	}
}

// Sniffing must not hang on a handshake smaller than any upper bound, which is
// every real handshake.
func TestMinecraftDoesNotBlockOnShortPacket(t *testing.T) {
	h := &mcproto.Handshake{ProtocolVersion: 765, ServerAddress: "a.example.com",
		ServerPort: 25565, NextState: mcproto.StateStatus}

	done := make(chan error, 1)
	go func() {
		_, err := Minecraft{}.Key(feed(t, h.Encode()))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("sniffer blocked waiting for more bytes than the handshake contains")
	}
}

func TestMinecraftRejectsOtherProtocols(t *testing.T) {
	for name, payload := range map[string][]byte{
		"http":      []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"),
		"tls":       {0x16, 0x03, 0x01, 0x00, 0x05},
		"junk":      {0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
		"oversized": mcproto.AppendVarInt(nil, 100000),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Minecraft{}.Key(feed(t, payload))
			if err == nil {
				t.Fatal("non-minecraft traffic produced a routing key")
			}
			if !errors.Is(err, ErrNoKey) && !isTimeout(err) {
				t.Logf("rejected with %v", err)
			}
		})
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
