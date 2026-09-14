// Package server is the VPS-side daemon: it accepts agent control sessions and
// runs the public frontends that route inbound connections into them.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/gabrielforster/lc/internal/muxproto"
	"github.com/gabrielforster/lc/internal/netutil"
	"github.com/gabrielforster/lc/internal/registry"
	"github.com/gabrielforster/lc/internal/store"
	"github.com/hashicorp/yamux"
)

// Server owns the control listener and every public frontend.
type Server struct {
	reg   *registry.Registry
	log   *slog.Logger
	ports *portFrontends
}

func New(reg *registry.Registry, log *slog.Logger) *Server {
	s := &Server{reg: reg, log: log}
	s.ports = &portFrontends{srv: s, live: map[int]*portListener{}}
	return s
}

// yamuxConfig tunes the session shared by every tunnel of one agent.
func yamuxConfig(log *slog.Logger) *yamux.Config {
	c := yamux.DefaultConfig()
	// NAT tables drop idle mappings silently, so the session needs its own
	// traffic to stay alive. yamux's built-in keepalive does this, which is why
	// there is no hand-rolled heartbeat in the control protocol.
	c.EnableKeepAlive = true
	c.KeepAliveInterval = 15 * time.Second
	c.LogOutput = nil
	c.Logger = slogAdapter(log)
	return c
}

// ServeControl accepts agent sessions until ctx is cancelled.
func (s *Server) ServeControl(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handleAgent(ctx, conn)
	}
}

// handleAgent runs one agent's session for its lifetime.
func (s *Server) handleAgent(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	sess, err := yamux.Server(conn, yamuxConfig(s.log))
	if err != nil {
		s.log.Warn("yamux handshake failed", "remote", conn.RemoteAddr(), "err", err)
		return
	}
	defer sess.Close()

	// The agent opens the control stream first; everything else on this session
	// is the server opening streams towards the agent.
	ctrl, err := sess.AcceptStream()
	if err != nil {
		s.log.Warn("no control stream", "remote", conn.RemoteAddr(), "err", err)
		return
	}
	defer ctrl.Close()

	r := muxproto.NewReader(ctrl)
	var hello muxproto.Hello
	if err := r.Read(&hello); err != nil {
		muxproto.Write(ctrl, &muxproto.Error{Code: muxproto.CodeBadReq, Msg: err.Error()})
		return
	}

	tok, err := s.reg.Authenticate(hello.Token)
	if err != nil {
		s.log.Warn("auth failed", "remote", conn.RemoteAddr())
		muxproto.Write(ctrl, &muxproto.Error{Code: muxproto.CodeAuth, Msg: "unknown or disabled token"})
		return
	}

	tunnels, err := s.registerAll(tok, sess, hello.Tunnels)
	// Registered tunnels are released even on partial failure, so a rejected
	// registration cannot leave half its tunnels live.
	defer func() {
		for _, t := range tunnels {
			s.ports.stop(t)
			s.reg.Release(t)
			s.log.Info("tunnel closed", "name", t.Name, "host", t.Host, "port", t.PublicPort)
		}
	}()
	if err != nil {
		muxproto.Write(ctrl, toProtoError(err))
		return
	}

	results := make([]muxproto.TunnelResult, 0, len(tunnels))
	for _, t := range tunnels {
		results = append(results, muxproto.TunnelResult{
			Name: t.Name, ID: t.ID, PublicAddr: s.publicAddr(t),
		})
		s.log.Info("tunnel open", "name", t.Name, "kind", t.Kind,
			"public", s.publicAddr(t), "token", tok.ID)
	}
	if err := muxproto.Write(ctrl, &muxproto.HelloOK{Tunnels: results}); err != nil {
		return
	}

	// The control stream stays open for runtime requests; reading it also
	// detects the agent going away.
	s.serveControlStream(ctx, ctrl, r, tok)
}

// registerAll registers every requested tunnel, returning those that succeeded
// so far alongside the first error.
func (s *Server) registerAll(tok store.Token, sess *yamux.Session, specs []muxproto.TunnelSpec) ([]*registry.Tunnel, error) {
	var out []*registry.Tunnel
	seen := map[string]bool{}
	for _, spec := range specs {
		if spec.Name == "" || seen[spec.Name] {
			return out, fmt.Errorf("%w: tunnel names must be present and unique", errBadRequest)
		}
		seen[spec.Name] = true

		t, err := s.reg.Register(tok, sess, spec, newID())
		if err != nil {
			return out, err
		}
		// A tcp tunnel needs its own public listener; hostname-routed kinds
		// share the standing frontends instead.
		if t.Kind == muxproto.KindTCP {
			if err := s.ports.start(t); err != nil {
				return out, err
			}
		}
		out = append(out, t)
	}
	return out, nil
}

// serveControlStream handles runtime messages until the agent disconnects.
func (s *Server) serveControlStream(ctx context.Context, ctrl net.Conn, r *muxproto.Reader, tok store.Token) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var claim muxproto.ClaimDomain
			if err := r.Read(&claim); err != nil {
				return // agent gone, or a frame we do not handle
			}
			res := s.reg.Claim(tok, claim.Domain)
			s.log.Info("domain claim", "token", tok.ID, "domain", claim.Domain,
				"ok", res.OK, "reason", res.Reason)
			if err := muxproto.Write(ctrl, &res); err != nil {
				return
			}
		}
	}()
	select {
	case <-ctx.Done():
	case <-done:
	}
}

// Dial opens a stream to the agent serving t and announces the client address.
// The returned conn is the agent end of that public connection.
func Dial(t *registry.Tunnel, clientAddr string) (net.Conn, error) {
	if t.Session == nil {
		return nil, errors.New("server: tunnel has no live session")
	}
	stream, err := t.Session.OpenStream()
	if err != nil {
		return nil, err
	}
	init := muxproto.StreamInit{TunnelID: t.ID, ClientAddr: clientAddr}
	if err := muxproto.Write(stream, &init); err != nil {
		stream.Close()
		return nil, err
	}
	return stream, nil
}

// pipe joins a public connection to a freshly opened agent stream.
func (s *Server) pipe(public net.Conn, t *registry.Tunnel) {
	stream, err := Dial(t, public.RemoteAddr().String())
	if err != nil {
		s.log.Warn("open stream failed", "tunnel", t.Name, "err", err)
		public.Close()
		return
	}
	// The yamux stream is wrapped so Join treats its Close as the half-close it
	// actually is, rather than a full teardown.
	netutil.Join(public, netutil.YamuxHalfCloser{Stream: stream.(*yamux.Stream)})
}

func (s *Server) publicAddr(t *registry.Tunnel) string {
	if t.Host != "" {
		return t.Host
	}
	return net.JoinHostPort(s.reg.Config().PublicHost, fmt.Sprint(t.PublicPort))
}

var errBadRequest = errors.New("bad request")

func toProtoError(err error) *muxproto.Error {
	switch {
	case errors.Is(err, registry.ErrForbidden):
		return &muxproto.Error{Code: muxproto.CodeForbidden, Msg: err.Error()}
	case errors.Is(err, registry.ErrHostBusy):
		return &muxproto.Error{Code: muxproto.CodeConflict, Msg: err.Error()}
	case errors.Is(err, errBadRequest):
		return &muxproto.Error{Code: muxproto.CodeBadReq, Msg: err.Error()}
	}
	return &muxproto.Error{Code: muxproto.CodeInternal, Msg: err.Error()}
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// portFrontends owns one public listener per live tcp tunnel.
type portFrontends struct {
	srv *Server
	mu  sync.Mutex
	// live is keyed by public port.
	live map[int]*portListener
}

type portListener struct {
	ln       net.Listener
	tunnelID string
}

func (p *portFrontends) start(t *registry.Tunnel) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.live[t.PublicPort]; ok {
		return fmt.Errorf("%w: port %d already listening", registry.ErrHostBusy, t.PublicPort)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", t.PublicPort))
	if err != nil {
		return err
	}
	p.live[t.PublicPort] = &portListener{ln: ln, tunnelID: t.ID}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go p.srv.pipe(conn, t)
		}
	}()
	return nil
}

func (p *portFrontends) stop(t *registry.Tunnel) {
	if t.PublicPort == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if l, ok := p.live[t.PublicPort]; ok && l.tunnelID == t.ID {
		l.ln.Close()
		delete(p.live, t.PublicPort)
	}
}
