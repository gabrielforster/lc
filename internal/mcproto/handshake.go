package mcproto

import (
	"bufio"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Packet ids in the handshaking and login states.
const (
	idHandshake  = 0x00
	idLoginStart = 0x00
)

// NextState values from the handshake.
const (
	// StateStatus is a server-list ping, which sends no login packet.
	StateStatus int32 = 1
	// StateLogin is a player joining.
	StateLogin int32 = 2
)

// Handshake is the first packet a Java client sends.
type Handshake struct {
	ProtocolVersion int32
	// ServerAddress is the hostname the player typed. It is the routing key for
	// hostname-based Minecraft tunnels, and the field BungeeCord forwarding
	// overloads to carry the real client IP.
	ServerAddress string
	ServerPort    uint16
	NextState     int32
}

// ErrNotHandshake means the first packet was not a handshake, so the connection
// is not a Minecraft client.
var ErrNotHandshake = errors.New("mcproto: first packet is not a handshake")

// ReadHandshake decodes the handshake from r.
func ReadHandshake(r *bufio.Reader) (*Handshake, error) {
	length, err := ReadVarInt(r)
	if err != nil {
		return nil, err
	}
	if length <= 0 || length > maxStringLen {
		return nil, fmt.Errorf("%w: implausible packet length %d", ErrNotHandshake, length)
	}

	// Bounding the reader to the declared length keeps a malformed packet from
	// consuming bytes belonging to the next one.
	lr := &bufio.Reader{}
	*lr = *bufio.NewReader(io.LimitReader(r, int64(length)))

	id, err := ReadVarInt(lr)
	if err != nil {
		return nil, err
	}
	if id != idHandshake {
		return nil, fmt.Errorf("%w: packet id 0x%02x", ErrNotHandshake, id)
	}

	h := &Handshake{}
	if h.ProtocolVersion, err = ReadVarInt(lr); err != nil {
		return nil, err
	}
	if h.ServerAddress, err = ReadString(lr); err != nil {
		return nil, err
	}
	hi, err := lr.ReadByte()
	if err != nil {
		return nil, err
	}
	lo, err := lr.ReadByte()
	if err != nil {
		return nil, err
	}
	h.ServerPort = uint16(hi)<<8 | uint16(lo)
	if h.NextState, err = ReadVarInt(lr); err != nil {
		return nil, err
	}
	return h, nil
}

// Encode renders the handshake back onto the wire, length prefix included.
func (h *Handshake) Encode() []byte {
	body := make([]byte, 0, 64+len(h.ServerAddress))
	body = AppendVarInt(body, idHandshake)
	body = AppendVarInt(body, h.ProtocolVersion)
	body = AppendString(body, h.ServerAddress)
	body = append(body, byte(h.ServerPort>>8), byte(h.ServerPort))
	body = AppendVarInt(body, h.NextState)

	out := make([]byte, 0, len(body)+5)
	out = AppendVarInt(out, int32(len(body)))
	return append(out, body...)
}

// Hostname returns the address the player typed, with any forwarding payload
// and trailing FML marker stripped.
//
// Clients modded with Forge append "\0FML\0" to the address, so a raw
// comparison against a configured hostname would miss them.
func (h *Handshake) Hostname() string {
	addr := h.ServerAddress
	if i := strings.IndexByte(addr, 0); i >= 0 {
		addr = addr[:i]
	}
	return strings.ToLower(strings.TrimSuffix(addr, "."))
}

// LoginStart is the packet that follows a handshake with NextState 2.
type LoginStart struct {
	Username string
	// Raw is the packet exactly as received, replayed verbatim so this parser
	// does not have to track the field changes newer versions made after the
	// username.
	Raw []byte
}

// ReadLoginStart decodes the login packet, keeping the original bytes.
func ReadLoginStart(r *bufio.Reader) (*LoginStart, error) {
	length, err := ReadVarInt(r)
	if err != nil {
		return nil, err
	}
	if length <= 0 || length > maxStringLen {
		return nil, fmt.Errorf("mcproto: implausible login packet length %d", length)
	}

	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}

	raw := AppendVarInt(nil, length)
	raw = append(raw, body...)

	br := bufio.NewReader(strings.NewReader(string(body)))
	id, err := ReadVarInt(br)
	if err != nil {
		return nil, err
	}
	if id != idLoginStart {
		return nil, fmt.Errorf("mcproto: expected login start, got packet id 0x%02x", id)
	}
	name, err := ReadString(br)
	if err != nil {
		return nil, err
	}
	return &LoginStart{Username: name, Raw: raw}, nil
}

// OfflineUUID derives the UUID an offline-mode server assigns to a username.
//
// It is the version-3 UUID of "OfflinePlayer:<name>", which is what vanilla
// does when online-mode is false. Matching that exactly is what keeps
// whitelists, player data and bans pointing at the same player across restarts.
func OfflineUUID(username string) string {
	sum := md5.Sum([]byte("OfflinePlayer:" + username))
	sum[6] = (sum[6] & 0x0f) | 0x30 // version 3
	sum[8] = (sum[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x", sum)
}

// RewriteForForwarding overloads the handshake's address field with the
// BungeeCord forwarding payload: host\0clientIP\0uuid\0properties.
//
// Without it every player appears to the server as the tunnel itself, which
// breaks bans and any IP-aware plugin. The properties field is an empty JSON
// array because this tunnel does not authenticate with Mojang and so has no
// signed profile properties to pass on.
//
// The server must run with online-mode=false to accept this, which means it
// performs no session verification of its own. A firewall keeping the server
// reachable only through the tunnel is therefore load-bearing.
func (h *Handshake) RewriteForForwarding(clientIP, uuid string) {
	const emptyProperties = "[]"
	h.ServerAddress = strings.Join([]string{h.Hostname(), clientIP, uuid, emptyProperties}, "\x00")
}
