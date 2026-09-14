package netutil

import (
	"net"
	"sync"
	"time"
)

// IdleConn closes a connection that has moved no bytes in either direction for
// a set period.
//
// A peer that vanishes -- a closed laptop, a dropped NAT mapping, a crashed
// client -- never sends a FIN, so nothing ever errors and the connection sits
// there holding a public socket, a yamux stream and a socket to the local
// service. Since nothing fails, this is invisible until the resources run out
// or the per-tunnel connection cap fills with connections nobody is using.
//
// The timeout cannot tell a dead peer from a legitimately quiet one, so it must
// be generous enough for the protocols being carried: an idle SSH session sends
// nothing for hours.
type IdleConn struct {
	net.Conn
	timeout time.Duration

	mu sync.Mutex
	// last is when activity was last recorded. The deadline is only pushed
	// forward when it has aged meaningfully, since a syscall per read would
	// cost more than the timeout saves on a busy connection.
	last time.Time
}

// WithIdleTimeout wraps c so it is closed after d without traffic. A zero or
// negative d returns c unchanged, so "disabled" costs nothing.
func WithIdleTimeout(c net.Conn, d time.Duration) net.Conn {
	if d <= 0 {
		return c
	}
	ic := &IdleConn{Conn: c, timeout: d}
	ic.touch(true)
	return ic
}

func (c *IdleConn) Read(p []byte) (int, error) {
	// Touching before the read arms the deadline for the wait itself, which is
	// where an abandoned connection actually sits.
	c.touch(false)
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.touch(false)
	}
	return n, err
}

func (c *IdleConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 {
		// Traffic in either direction means the connection is alive, so a
		// write extends the read deadline too. Without this, a connection that
		// only sends would be torn down mid-transfer.
		c.touch(true)
	}
	return n, err
}

// touch pushes the read deadline out. force bypasses the rate limiting, for
// events that must be recorded immediately.
func (c *IdleConn) touch(force bool) {
	now := time.Now()

	c.mu.Lock()
	// Refreshing on every read would mean a syscall per read. A tenth of the
	// timeout keeps the granularity far finer than the timeout itself.
	if !force && now.Sub(c.last) < c.timeout/10 {
		c.mu.Unlock()
		return
	}
	c.last = now
	c.mu.Unlock()

	// Deadlines apply to reads already blocked, so this reaches the goroutine
	// currently waiting on this connection.
	c.Conn.SetReadDeadline(now.Add(c.timeout))
}

// CloseWrite forwards the half-close, so wrapping a connection for idle
// tracking does not disable Join's half-close handling.
func (c *IdleConn) CloseWrite() error {
	if cw, ok := c.Conn.(CloseWriter); ok {
		return cw.CloseWrite()
	}
	return nil
}
