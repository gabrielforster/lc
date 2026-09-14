package netutil

import (
	"io"
	"net"
	"testing"
	"time"
)

// TestJoinHalfClose is the regression test for the classic proxy leak: when one
// side finishes sending, the peer must see EOF while still being able to send.
func TestJoinHalfClose(t *testing.T) {
	clientSide, proxyA := tcpPair(t)
	proxyB, serverSide := tcpPair(t)

	go Join(proxyA, proxyB)

	// Client sends a request and half-closes; the server must observe EOF.
	if _, err := clientSide.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if err := clientSide.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}

	serverSide.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(serverSide)
	if err != nil {
		t.Fatalf("server read: %v", err)
	}
	if string(got) != "ping" {
		t.Fatalf("server got %q, want %q", got, "ping")
	}

	// The reverse direction must still be open after that half-close.
	if _, err := serverSide.Write([]byte("pong")); err != nil {
		t.Fatalf("server write after client half-close: %v", err)
	}
	serverSide.(*net.TCPConn).CloseWrite()

	clientSide.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err = io.ReadAll(clientSide)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	if string(got) != "pong" {
		t.Fatalf("client got %q, want %q", got, "pong")
	}
}

func TestPeekConnDoesNotConsume(t *testing.T) {
	client, server := tcpPair(t)
	go func() {
		client.Write([]byte("HELLO WORLD"))
		client.(*net.TCPConn).CloseWrite()
	}()

	pc := NewPeekConn(server)
	head, err := pc.Peek(5)
	if err != nil {
		t.Fatal(err)
	}
	if string(head) != "HELLO" {
		t.Fatalf("peek got %q", head)
	}

	// The peeked bytes must still be delivered to the next reader.
	pc.SetReadDeadline(time.Now().Add(5 * time.Second))
	all, err := io.ReadAll(pc)
	if err != nil {
		t.Fatal(err)
	}
	if string(all) != "HELLO WORLD" {
		t.Fatalf("read after peek got %q, want full payload", all)
	}
}

// tcpPair returns two ends of a real loopback TCP connection, so tests exercise
// actual CloseWrite semantics rather than net.Pipe's synchronous approximation.
func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()

	dialed, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	got := <-ch
	if got.err != nil {
		t.Fatal(got.err)
	}
	t.Cleanup(func() { dialed.Close(); got.c.Close() })
	return dialed, got.c
}
