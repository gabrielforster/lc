// Package netutil holds the low-level connection plumbing shared by the server
// and the agent: non-consuming reads for protocol sniffing, and a bidirectional
// copy that propagates half-closes correctly.
package netutil

import (
	"bufio"
	"net"
)

// peekBufSize bounds how far a sniffer can look ahead. A Minecraft handshake is
// a few hundred bytes at most; HTTP request headers are the larger case.
const peekBufSize = 8192

// PeekConn wraps a net.Conn so the first bytes can be inspected for routing
// without consuming them. Reads go through the same buffered reader that Peek
// fills, so whatever a sniffer looked at is still delivered to the next reader.
// This is what keeps the "truncated handshake" bug from being possible: there is
// no separate replay path that could be forgotten.
type PeekConn struct {
	net.Conn
	r *bufio.Reader
}

// NewPeekConn wraps c. The returned conn must be used in place of c from here on.
func NewPeekConn(c net.Conn) *PeekConn {
	return &PeekConn{Conn: c, r: bufio.NewReaderSize(c, peekBufSize)}
}

// Peek returns the next n bytes without advancing the reader. The bytes stop
// being valid at the next read, so callers that need to keep them must copy.
func (c *PeekConn) Peek(n int) ([]byte, error) { return c.r.Peek(n) }

func (c *PeekConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// CloseWrite forwards to the underlying conn when it supports half-close, so
// wrapping a conn for sniffing does not silently disable Join's half-close
// handling.
func (c *PeekConn) CloseWrite() error {
	if cw, ok := c.Conn.(CloseWriter); ok {
		return cw.CloseWrite()
	}
	return nil
}
