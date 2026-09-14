// Package sniff extracts a routing key from the first bytes of a connection,
// without consuming them.
//
// This is the seam that keeps protocol awareness out of the server core: the
// L4 router knows only "ask the sniffer for a hostname", so adding TLS SNI
// routing later means adding an implementation here, not touching the router.
package sniff

import (
	"bufio"
	"errors"

	"github.com/gabrielforster/lc/internal/mcproto"
	"github.com/gabrielforster/lc/internal/netutil"
)

// ErrNoKey means the connection carried no usable routing key.
var ErrNoKey = errors.New("sniff: no routing key in the first bytes")

// Sniffer reads a routing key from a connection's opening bytes.
//
// Implementations must not consume: the bytes they inspect still have to reach
// the backend, or the service on the far end sees a truncated protocol.
type Sniffer interface {
	// Name identifies the sniffer in logs.
	Name() string
	// Key returns the routing key, or ErrNoKey.
	Key(c *netutil.PeekConn) (string, error)
}

// Minecraft routes by the hostname in the Java handshake packet.
//
// The first packet a client sends carries the address the player typed, which
// is what lets one public port serve every Minecraft tunnel.
type Minecraft struct{}

func (Minecraft) Name() string { return "minecraft" }

func (Minecraft) Key(c *netutil.PeekConn) (string, error) {
	// maxHandshake bounds how much of the connection a routing decision may
	// read, so a client that sends a huge length prefix cannot make us buffer
	// it.
	const maxHandshake = 1024

	// Peek blocks until it has the full count, so the packet length is read
	// first and only exactly that many bytes are then peeked. Asking for a
	// fixed large count instead would hang on every real handshake, which is
	// smaller than any sensible upper bound.
	length, prefix, err := peekVarInt(c)
	if err != nil {
		return "", err
	}
	total := prefix + int(length)
	if length <= 0 || total > maxHandshake {
		return "", ErrNoKey
	}

	peeked, err := c.Peek(total)
	if err != nil {
		return "", err
	}

	h, err := mcproto.ReadHandshake(bufio.NewReader(newByteReader(peeked)))
	if err != nil {
		return "", ErrNoKey
	}
	host := h.Hostname()
	if host == "" {
		return "", ErrNoKey
	}
	return host, nil
}

// peekVarInt reads the leading VarInt without consuming it, growing the peek
// one byte at a time because a VarInt's length is only known as it is decoded.
// It returns the value and how many bytes it occupied.
func peekVarInt(c *netutil.PeekConn) (int32, int, error) {
	for n := 1; n <= 5; n++ {
		buf, err := c.Peek(n)
		if err != nil {
			return 0, 0, err
		}
		if buf[n-1]&0x80 != 0 {
			continue // continuation bit set, another byte follows
		}
		v, err := mcproto.ReadVarInt(newByteReader(buf))
		if err != nil {
			return 0, 0, err
		}
		return v, n, nil
	}
	return 0, 0, ErrNoKey
}
