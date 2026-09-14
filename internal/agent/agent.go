// Package agent is the home-side daemon. It dials out to the server, holds the
// session open, and serves the streams the server pushes down it.
//
// Everything crossing NAT is opened from in here; nothing dials in.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	"github.com/gabrielforster/lc/internal/muxproto"
	"github.com/gabrielforster/lc/internal/netutil"
	"github.com/hashicorp/yamux"
)

// Config describes what this agent exposes and where it connects.
type Config struct {
	// ServerAddr is the control address of the server, host:port.
	ServerAddr string
	Token      string
	Tunnels    []muxproto.TunnelSpec
	// DialTLS, when set, is used instead of a plain TCP dial.
	DialTLS func(ctx context.Context, addr string) (net.Conn, error)
}

// Transform adapts the first bytes of a proxied connection before they reach
// the local service. It is how per-protocol behaviour -- notably the Minecraft
// real-IP rewrite -- stays out of the generic stream plumbing.
//
// It runs after the local connection is dialled and before the two are joined.
type Transform func(client net.Conn, local net.Conn, clientAddr string) error

// Agent holds one session's worth of state.
type Agent struct {
	cfg        Config
	log        *slog.Logger
	transforms map[muxproto.Kind]Transform

	mu      sync.RWMutex
	tunnels map[string]muxproto.TunnelSpec // by server-assigned id
	ctrl    net.Conn
	// ctrlReader must be the same reader that consumed HelloOK, or replies
	// buffered behind it are lost.
	ctrlReader *muxproto.Reader

	// reqMu serialises control requests, which share one stream.
	reqMu sync.Mutex
}

func New(cfg Config, log *slog.Logger) *Agent {
	return &Agent{
		cfg:        cfg,
		log:        log,
		transforms: map[muxproto.Kind]Transform{},
		tunnels:    map[string]muxproto.TunnelSpec{},
	}
}

// SetTransform registers the behaviour for a tunnel kind. Kinds without one are
// piped through untouched.
func (a *Agent) SetTransform(k muxproto.Kind, t Transform) { a.transforms[k] = t }

// Run keeps a session up until ctx is cancelled, reconnecting with backoff.
//
// The home connection will drop -- that is a given, not an exceptional case --
// so reconnecting is part of normal operation rather than error handling. The
// durable claims in the server's store are what let a reconnect come back with
// the same hostname and port.
func (a *Agent) Run(ctx context.Context) error {
	const (
		minBackoff = time.Second
		maxBackoff = time.Minute
	)
	backoff := minBackoff

	for ctx.Err() == nil {
		err := a.session(ctx)
		if ctx.Err() != nil {
			return nil
		}

		// An authentication or authorization failure will not fix itself by
		// retrying, so it stops the agent instead of spinning.
		var pe *muxproto.Error
		if errors.As(err, &pe) && (pe.Code == muxproto.CodeAuth || pe.Code == muxproto.CodeForbidden) {
			return fmt.Errorf("agent: server rejected registration: %w", pe)
		}
		if err != nil {
			a.log.Warn("session ended", "err", err, "retry_in", backoff)
		}

		// Jitter keeps many agents from reconnecting in lockstep after a
		// server restart.
		jittered := backoff + rand.N(backoff/2+1)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(jittered):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
	return nil
}

// session runs one connection from dial to teardown.
func (a *Agent) session(ctx context.Context) error {
	conn, err := a.dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = 15 * time.Second
	cfg.LogOutput = nil
	cfg.Logger = slogAdapter(a.log)

	sess, err := yamux.Client(conn, cfg)
	if err != nil {
		return err
	}
	defer sess.Close()

	ctrl, err := sess.OpenStream()
	if err != nil {
		return err
	}
	defer ctrl.Close()

	if err := muxproto.Write(ctrl, &muxproto.Hello{Token: a.cfg.Token, Tunnels: a.cfg.Tunnels}); err != nil {
		return err
	}
	r := muxproto.NewReader(ctrl)
	var ok muxproto.HelloOK
	if err := r.Read(&ok); err != nil {
		return err
	}

	a.mu.Lock()
	a.ctrl = ctrl
	a.ctrlReader = r
	a.tunnels = map[string]muxproto.TunnelSpec{}
	for _, res := range ok.Tunnels {
		for _, spec := range a.cfg.Tunnels {
			if spec.Name == res.Name {
				a.tunnels[res.ID] = spec
			}
		}
		a.log.Info("tunnel ready", "name", res.Name, "public", res.PublicAddr)
	}
	a.mu.Unlock()

	go func() {
		<-ctx.Done()
		sess.Close()
	}()

	defer func() {
		a.mu.Lock()
		a.ctrl, a.ctrlReader = nil, nil
		a.mu.Unlock()
	}()

	// From here the server drives: every inbound public connection arrives as a
	// new stream on this session.
	for {
		stream, err := sess.AcceptStream()
		if err != nil {
			return err
		}
		go a.serveStream(stream)
	}
}

func (a *Agent) dial(ctx context.Context) (net.Conn, error) {
	if a.cfg.DialTLS != nil {
		return a.cfg.DialTLS(ctx, a.cfg.ServerAddr)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", a.cfg.ServerAddr)
}

// serveStream handles one proxied connection.
func (a *Agent) serveStream(stream *yamux.Stream) {
	// The init header is framed on the stream itself, so its reader must also
	// be the one that hands the remaining bytes to the transform -- otherwise
	// anything it buffered past the header is lost.
	r := muxproto.NewReader(stream)
	var init muxproto.StreamInit
	if err := r.Read(&init); err != nil {
		a.log.Warn("bad stream init", "err", err)
		stream.Close()
		return
	}

	a.mu.RLock()
	spec, ok := a.tunnels[init.TunnelID]
	a.mu.RUnlock()
	if !ok {
		a.log.Warn("stream for unknown tunnel", "id", init.TunnelID)
		stream.Close()
		return
	}

	local, err := net.DialTimeout("tcp", spec.LocalAddr, 10*time.Second)
	if err != nil {
		a.log.Warn("local dial failed", "tunnel", spec.Name, "addr", spec.LocalAddr, "err", err)
		stream.Close()
		return
	}

	// client reads the post-header bytes through the muxproto reader's buffer,
	// and writes straight to the stream.
	client := &streamConn{Stream: stream, r: r}

	if tf := a.transforms[spec.Kind]; tf != nil {
		if err := tf(client, local, init.ClientAddr); err != nil {
			a.log.Warn("transform failed", "tunnel", spec.Name, "err", err)
			stream.Close()
			local.Close()
			return
		}
	}

	netutil.Join(client, local)
}

// ClaimDomain asks the server for a custom hostname on the live session.
func (a *Agent) ClaimDomain(domain string) (muxproto.ClaimResult, error) {
	var res muxproto.ClaimResult
	err := a.request(&muxproto.ClaimDomain{Domain: domain}, &res)
	return res, err
}

// ReleaseDomain gives up a claimed hostname.
func (a *Agent) ReleaseDomain(domain string) (muxproto.ClaimResult, error) {
	var res muxproto.ClaimResult
	err := a.request(&muxproto.ReleaseDomain{Domain: domain}, &res)
	return res, err
}

// Domains lists the hostnames this agent's token owns.
func (a *Agent) Domains() ([]string, error) {
	var res muxproto.DomainList
	err := a.request(&muxproto.ListDomains{}, &res)
	return res.Domains, err
}

// request sends one control message and decodes its reply.
//
// The lock is held across the exchange because the control stream carries one
// request at a time; concurrent callers would otherwise read each other's
// replies.
func (a *Agent) request(req, out any) error {
	a.reqMu.Lock()
	defer a.reqMu.Unlock()

	a.mu.RLock()
	ctrl, r := a.ctrl, a.ctrlReader
	a.mu.RUnlock()
	if ctrl == nil {
		return errors.New("agent: not connected")
	}
	if err := muxproto.Write(ctrl, req); err != nil {
		return err
	}
	return r.Read(out)
}

// WaitReady blocks until the session is registered, or ctx expires. One-shot
// commands need this because Run connects asynchronously.
func (a *Agent) WaitReady(ctx context.Context) error {
	for {
		a.mu.RLock()
		ready := a.ctrl != nil
		a.mu.RUnlock()
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}
