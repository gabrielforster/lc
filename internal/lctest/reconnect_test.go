package lctest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/gabrielforster/lc/internal/agent"
	"github.com/gabrielforster/lc/internal/muxproto"
	"github.com/gabrielforster/lc/internal/registry"
	"github.com/gabrielforster/lc/internal/server"
	"github.com/gabrielforster/lc/internal/store"
)

// The home connection dropping is normal, not exceptional. The agent must come
// back on its own, and come back with the same public port -- which is what the
// durable reservation in the store is for.
func TestAgentReconnectsAndKeepsItsPort(t *testing.T) {
	db, err := store.Open(t.TempDir() + "/lc.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	tok, secret, err := db.CreateToken("home")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddGrant(tok.ID, store.GrantPortAuto, ""); err != nil {
		t.Fatal(err)
	}

	reg := registry.New(db, registry.Config{PortMin: 34500, PortMax: 34600, PublicHost: "127.0.0.1"})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(reg, log)

	// A fixed control address lets the listener be torn down and rebuilt,
	// standing in for the server restarting or the link dropping.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	controlAddr := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srvCtx, stopServer := context.WithCancel(ctx)
	go srv.ServeControl(srvCtx, ln)

	local := EchoService(t)
	ag := agent.New(agent.Config{
		ServerAddr: controlAddr,
		Token:      secret,
		Tunnels: []muxproto.TunnelSpec{{
			Name: "echo", Kind: muxproto.KindTCP, LocalAddr: local,
		}},
	}, log)
	go ag.Run(ctx)

	first := waitForPort(t, db, tok.ID, "echo")
	assertEcho(t, first, "before restart")

	// Drop everything the agent was connected to.
	stopServer()
	ln.Close()

	// Bring the server back on the same address.
	ln2, err := net.Listen("tcp", controlAddr)
	if err != nil {
		t.Fatalf("rebinding control listener: %v", err)
	}
	defer ln2.Close()
	go srv.ServeControl(ctx, ln2)

	second := waitForLivePort(t, reg, db, tok.ID, "echo")
	if second != first {
		t.Fatalf("port changed across reconnect: %d then %d", first, second)
	}
	assertEcho(t, second, "after restart")
}

func waitForPort(t *testing.T, db *store.DB, tokenID int64, name string) int {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		ports, err := db.Ports(tokenID)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range ports {
			if p.TunnelName == name {
				return p.Port
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("tunnel %q never got a port", name)
	return 0
}

// waitForLivePort waits for the tunnel to be reachable again, not merely
// recorded, since the reservation outlives the session by design.
func waitForLivePort(t *testing.T, reg *registry.Registry, db *store.DB, tokenID int64, name string) int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		port := waitForPort(t, db, tokenID, name)
		if _, ok := reg.LookupPort(port); ok {
			return port
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("tunnel %q never came back", name)
	return 0
}

func assertEcho(t *testing.T, port int, payload string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		t.Fatalf("%s: dial: %v", payload, err)
	}
	defer conn.Close()

	conn.Write([]byte(payload))
	conn.(*net.TCPConn).CloseWrite()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("%s: read: %v", payload, err)
	}
	if string(got) != payload {
		t.Fatalf("%s: got %q", payload, got)
	}
}
