package server

import (
	"context"
	"net"
	"time"

	"github.com/gabrielforster/lc/internal/netutil"
	"github.com/gabrielforster/lc/internal/sniff"
)

// ServeSniffed runs a public listener that routes by peeking at each
// connection's first bytes, so one port serves every tunnel of that protocol.
//
// This is how a single :25565 can host every Minecraft tunnel: the hostname the
// player typed is in the handshake, and peeking leaves it intact for the server
// on the other end.
func (s *Server) ServeSniffed(ctx context.Context, ln net.Listener, sn sniff.Sniffer) error {
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
		go s.routeSniffed(conn, sn)
	}
}

func (s *Server) routeSniffed(conn net.Conn, sn sniff.Sniffer) {
	// A client that connects and says nothing must not hold a slot forever.
	conn.SetReadDeadline(time.Now().Add(15 * time.Second))

	peek := netutil.NewPeekConn(conn)
	host, err := sn.Key(peek)
	if err != nil {
		s.log.Debug("no routing key", "sniffer", sn.Name(), "remote", conn.RemoteAddr(), "err", err)
		conn.Close()
		return
	}

	tun, ok := s.reg.LookupHost(host)
	if !ok {
		s.log.Debug("no tunnel for host", "sniffer", sn.Name(), "host", host)
		conn.Close()
		return
	}

	// Routing is done; the rest of the connection belongs to the backend.
	conn.SetReadDeadline(time.Time{})
	s.log.Debug("routed", "sniffer", sn.Name(), "host", host, "tunnel", tun.Name)
	s.pipe(peek, tun)
}
