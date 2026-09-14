package netutil

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// The point of the timeout: a peer that vanishes without sending FIN must not
// hold the connection open forever.
func TestIdleConnClosesSilentConnection(t *testing.T) {
	client, server := tcpPair(t)
	_ = client // deliberately silent, standing in for a vanished peer

	idle := WithIdleTimeout(server, 150*time.Millisecond)

	start := time.Now()
	_, err := idle.Read(make([]byte, 16))
	if err == nil {
		t.Fatal("read on an abandoned connection succeeded")
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("got %v, want a timeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %v to give up", elapsed)
	}
}

// A connection carrying traffic must never be torn down, however long it lives.
func TestIdleConnSurvivesOngoingTraffic(t *testing.T) {
	client, server := tcpPair(t)
	idle := WithIdleTimeout(server, 200*time.Millisecond)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Well inside the timeout, for several multiples of it.
		for range 10 {
			client.Write([]byte("x"))
			time.Sleep(50 * time.Millisecond)
		}
	}()

	buf := make([]byte, 1)
	for i := range 10 {
		if _, err := idle.Read(buf); err != nil {
			t.Fatalf("read %d on an active connection failed: %v", i, err)
		}
	}
	<-done
}

// Writing counts as activity: a connection that only sends must not be killed
// mid-transfer because nothing came back.
func TestIdleConnWriteCountsAsActivity(t *testing.T) {
	client, server := tcpPair(t)
	go io.Copy(io.Discard, client)

	idle := WithIdleTimeout(server, 200*time.Millisecond)
	for i := range 8 {
		if _, err := idle.Write([]byte("payload")); err != nil {
			t.Fatalf("write %d failed: %v", i, err)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// After sustained writing the connection must still be readable, meaning
	// the deadline moved along with the writes.
	go func() { client.Write([]byte("reply")) }()
	if _, err := idle.Read(make([]byte, 5)); err != nil {
		t.Fatalf("read after sustained writes: %v", err)
	}
}

// Disabling must cost nothing, not wrap in a no-op layer.
func TestIdleTimeoutDisabledReturnsOriginal(t *testing.T) {
	_, server := tcpPair(t)
	if got := WithIdleTimeout(server, 0); got != server {
		t.Fatal("zero timeout wrapped the connection anyway")
	}
	if got := WithIdleTimeout(server, -time.Second); got != server {
		t.Fatal("negative timeout wrapped the connection anyway")
	}
}

// Half-close has to survive the wrapper, or Join stops propagating EOF.
func TestIdleConnForwardsHalfClose(t *testing.T) {
	client, server := tcpPair(t)
	idle := WithIdleTimeout(server, time.Minute)

	cw, ok := idle.(CloseWriter)
	if !ok {
		t.Fatal("wrapped connection no longer offers CloseWrite")
	}
	idle.Write([]byte("request"))
	if err := cw.CloseWrite(); err != nil {
		t.Fatal(err)
	}

	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(client)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "request" {
		t.Fatalf("peer got %q and no EOF", got)
	}
}
