// Package lctest wires a server and an agent together in one process so tests
// exercise the real control protocol and real sockets, not mocks.
package lctest

import (
	"context"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/gabrielforster/lc/internal/agent"
	"github.com/gabrielforster/lc/internal/muxproto"
	"github.com/gabrielforster/lc/internal/registry"
	"github.com/gabrielforster/lc/internal/server"
	"github.com/gabrielforster/lc/internal/sniff"
	"github.com/gabrielforster/lc/internal/store"
)

// Harness is a running server plus a connected agent.
type Harness struct {
	Server   *server.Server
	Agent    *agent.Agent
	Registry *registry.Registry
	DB       *store.DB
	Token    string
	// ControlAddr is where the agent connects.
	ControlAddr string
	// HTTPAddr is the public HTTP frontend, set when Options.HTTP is true.
	HTTPAddr string
	// HTTPSAddr is the public TLS frontend, set when Options.HTTPS is true.
	HTTPSAddr string
	// Certs is the self-signed source backing HTTPSAddr; its RootCAs must be
	// trusted by any test client.
	Certs *server.SelfSigned
	// MCAddr is the public Minecraft frontend, set when Options.Minecraft is true.
	MCAddr string
}

// Options tunes the harness for a test.
type Options struct {
	Tunnels            []muxproto.TunnelSpec
	Grants             []store.Grant
	AllowCustomDomains bool
	PortMin, PortMax   int
	Transforms         map[muxproto.Kind]agent.Transform
	// HTTP starts the public HTTP frontend.
	HTTP bool
	// HTTPS starts the public TLS frontend with a self-signed cert source.
	HTTPS bool
	// Minecraft starts a sniffing L4 frontend routing by handshake hostname.
	Minecraft bool
	// MaxConnsPerTunnel caps concurrent public connections per tunnel.
	MaxConnsPerTunnel int
	// IdleTimeout closes proxied connections after this long without traffic.
	IdleTimeout time.Duration
}

// Start brings up a server and an agent and waits until the tunnels are live.
func Start(t *testing.T, opts Options) *Harness {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "lc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	tok, secret, err := db.CreateToken("test")
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range opts.Grants {
		if err := db.AddGrant(tok.ID, g.Kind, g.Value); err != nil {
			t.Fatal(err)
		}
	}

	if opts.PortMin == 0 {
		// A high range keeps test ports clear of anything privileged.
		opts.PortMin, opts.PortMax = 34000, 34200
	}
	reg := registry.New(db, registry.Config{
		PortMin:            opts.PortMin,
		PortMax:            opts.PortMax,
		AllowCustomDomains: opts.AllowCustomDomains,
		PublicHost:         "127.0.0.1",
		MaxConnsPerTunnel:  opts.MaxConnsPerTunnel,
		IdleTimeout:        opts.IdleTimeout,
	})

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(reg, log)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.ServeControl(ctx, ln)

	var httpAddr string
	if opts.HTTP {
		hln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		httpAddr = hln.Addr().String()
		go srv.ServeHTTPListener(ctx, hln)
	}

	var (
		httpsAddr string
		certs     *server.SelfSigned
	)
	if opts.HTTPS {
		certs, err = server.NewSelfSigned()
		if err != nil {
			t.Fatal(err)
		}
		sln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		httpsAddr = sln.Addr().String()
		go srv.ServeHTTPSListener(ctx, sln, certs)
	}

	var mcAddr string
	if opts.Minecraft {
		mln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		mcAddr = mln.Addr().String()
		go srv.ServeSniffed(ctx, mln, sniff.Minecraft{})
	}

	ag := agent.New(agent.Config{
		ServerAddr:  ln.Addr().String(),
		Token:       secret,
		Tunnels:     opts.Tunnels,
		IdleTimeout: opts.IdleTimeout,
	}, log)
	for kind, tf := range opts.Transforms {
		ag.SetTransform(kind, tf)
	}
	go ag.Run(ctx)

	h := &Harness{
		Server: srv, Agent: ag, Registry: reg, DB: db,
		Token: secret, ControlAddr: ln.Addr().String(), HTTPAddr: httpAddr, HTTPSAddr: httpsAddr, Certs: certs, MCAddr: mcAddr,
	}
	h.waitReady(t, opts.Tunnels)
	return h
}

// waitReady blocks until every requested tunnel is registered, so tests do not
// race the agent's connect.
func (h *Harness) waitReady(t *testing.T, specs []muxproto.TunnelSpec) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for _, spec := range specs {
		for {
			if time.Now().After(deadline) {
				t.Fatalf("tunnel %q never became ready", spec.Name)
			}
			var ok bool
			if spec.Host != "" {
				_, ok = h.Registry.LookupHost(spec.Host)
			} else {
				ok = h.tcpReady(spec.Name)
			}
			if ok {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func (h *Harness) tcpReady(name string) bool {
	tok, err := h.DB.TokenBySecret(h.Token)
	if err != nil {
		return false
	}
	ports, err := h.DB.Ports(tok.ID)
	if err != nil {
		return false
	}
	for _, p := range ports {
		if p.TunnelName == name {
			_, ok := h.Registry.LookupPort(p.Port)
			return ok
		}
	}
	return false
}

// PublicPort returns the port assigned to a tcp tunnel.
func (h *Harness) PublicPort(t *testing.T, name string) int {
	t.Helper()
	tok, err := h.DB.TokenBySecret(h.Token)
	if err != nil {
		t.Fatal(err)
	}
	ports, err := h.DB.Ports(tok.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ports {
		if p.TunnelName == name {
			return p.Port
		}
	}
	t.Fatalf("no port assigned to tunnel %q", name)
	return 0
}

// EchoService starts a local TCP service that echoes what it receives, standing
// in for whatever the agent would really forward to.
func EchoService(t *testing.T) string {
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
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}
