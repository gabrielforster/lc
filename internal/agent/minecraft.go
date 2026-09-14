package agent

import (
	"bufio"
	"fmt"
	"io"
	"net"

	"github.com/gabrielforster/lc/internal/mcproto"
)

// MinecraftTransform rewrites the handshake so the server sees the player's
// real address instead of the tunnel's.
//
// The ordering here is the awkward part. The forwarding payload belongs in the
// handshake's address field, but the UUID it must carry derives from the
// username, which arrives in the *next* packet. So the handshake is held,
// the login packet is read, and only then are both written on.
//
// Server-list pings (next state 1) send no login packet and need no forwarding,
// so they are replayed untouched. Holding them waiting for a login that never
// comes would hang every server list in the client.
func MinecraftTransform(client net.Conn, local net.Conn, clientAddr string) error {
	br := bufio.NewReader(client)

	h, err := mcproto.ReadHandshake(br)
	if err != nil {
		return fmt.Errorf("reading handshake: %w", err)
	}

	if h.NextState != mcproto.StateLogin {
		if _, err := local.Write(h.Encode()); err != nil {
			return err
		}
		return drain(br, local)
	}

	login, err := mcproto.ReadLoginStart(br)
	if err != nil {
		return fmt.Errorf("reading login start: %w", err)
	}

	ip, _, err := net.SplitHostPort(clientAddr)
	if err != nil {
		ip = clientAddr
	}
	h.RewriteForForwarding(ip, mcproto.OfflineUUID(login.Username))

	if _, err := local.Write(h.Encode()); err != nil {
		return err
	}
	// The login packet is replayed exactly as it arrived, so fields newer
	// versions added after the username pass through untouched.
	if _, err := local.Write(login.Raw); err != nil {
		return err
	}
	return drain(br, local)
}

// drain forwards whatever the reader buffered past the packets we parsed.
//
// Without this, any bytes the client pipelined behind the login packet are lost
// and the connection stalls.
func drain(br *bufio.Reader, local net.Conn) error {
	if n := br.Buffered(); n > 0 {
		buf := make([]byte, n)
		// ReadFull, not Read: a short read here would silently drop pipelined
		// bytes rather than fail.
		if _, err := io.ReadFull(br, buf); err != nil {
			return err
		}
		if _, err := local.Write(buf); err != nil {
			return err
		}
	}
	return nil
}
