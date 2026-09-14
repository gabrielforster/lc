// Package mctest provides a fake Minecraft client and server speaking just
// enough of the protocol to prove the tunnel end to end, with no JVM involved.
package mctest

import (
	"bufio"
	"net"
	"testing"
	"time"

	"github.com/gabrielforster/lc/internal/mcproto"
)

// Join is what a fake server observed from one connection.
type Join struct {
	Handshake *mcproto.Handshake
	Username  string
	// ForwardedIP and ForwardedUUID are parsed out of the handshake's address
	// field, which is where BungeeCord forwarding hides them.
	ForwardedIP   string
	ForwardedUUID string
	// Host is the address field with any forwarding payload stripped.
	Host string
}

// Server is a fake Minecraft server that records what it was sent.
type Server struct {
	Addr  string
	joins chan Join
}

// NewServer starts a fake server on loopback.
func NewServer(t *testing.T) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	s := &Server{Addr: ln.Addr().String(), joins: make(chan Join, 8)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.handle(c)
		}
	}()
	return s
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)

	h, err := mcproto.ReadHandshake(br)
	if err != nil {
		return
	}
	j := Join{Handshake: h, Host: h.Hostname()}

	// Forwarding data rides in the address field as host\0ip\0uuid\0properties.
	if parts := splitNull(h.ServerAddress); len(parts) >= 3 {
		j.ForwardedIP, j.ForwardedUUID = parts[1], parts[2]
	}

	if h.NextState == mcproto.StateLogin {
		login, err := mcproto.ReadLoginStart(br)
		if err != nil {
			return
		}
		j.Username = login.Username
	}

	select {
	case s.joins <- j:
	default:
	}
}

// NextJoin returns the next observed connection, failing the test on timeout.
func (s *Server) NextJoin(t *testing.T) Join {
	t.Helper()
	select {
	case j := <-s.joins:
		return j
	case <-time.After(10 * time.Second):
		t.Fatal("fake minecraft server saw no connection")
		return Join{}
	}
}

// Client is a fake vanilla client: it sends a handshake, and a login packet
// when joining.
type Client struct{ conn net.Conn }

// Dial connects to addr and performs the opening exchange for hostname.
//
// The hostname is what the player typed, which is both the routing key and,
// after the agent's rewrite, the field carrying the forwarding payload.
func Dial(t *testing.T, addr, hostname string, state int32) *Client {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	h := &mcproto.Handshake{
		ProtocolVersion: 765,
		ServerAddress:   hostname,
		ServerPort:      25565,
		NextState:       state,
	}
	if _, err := conn.Write(h.Encode()); err != nil {
		t.Fatal(err)
	}
	return &Client{conn: conn}
}

// Login sends the login-start packet for username.
func (c *Client) Login(t *testing.T, username string) {
	t.Helper()
	body := mcproto.AppendVarInt(nil, 0x00)
	body = mcproto.AppendString(body, username)

	packet := mcproto.AppendVarInt(nil, int32(len(body)))
	packet = append(packet, body...)

	if _, err := c.conn.Write(packet); err != nil {
		t.Fatal(err)
	}
}

func splitNull(s string) []string {
	var out []string
	start := 0
	for i := range len(s) {
		if s[i] == 0 {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// Joins exposes the channel directly, for tests asserting that nothing arrived.
func (s *Server) Joins() <-chan Join { return s.joins }
