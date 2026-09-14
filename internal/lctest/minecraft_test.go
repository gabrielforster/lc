package lctest

import (
	"testing"

	"github.com/gabrielforster/lc/internal/agent"
	"github.com/gabrielforster/lc/internal/mcproto"
	"github.com/gabrielforster/lc/internal/mctest"
	"github.com/gabrielforster/lc/internal/muxproto"
	"github.com/gabrielforster/lc/internal/store"
)

func mcHarness(t *testing.T, backend string) *Harness {
	t.Helper()
	return Start(t, Options{
		Minecraft: true,
		Grants:    []store.Grant{{Kind: store.GrantWildcard, Value: ".mc.example.com"}},
		Tunnels: []muxproto.TunnelSpec{{
			Name: "survival", Kind: muxproto.KindMinecraft,
			Host: "play.mc.example.com", LocalAddr: backend,
		}},
		Transforms: map[muxproto.Kind]agent.Transform{
			muxproto.KindMinecraft: agent.MinecraftTransform,
		},
	})
}

// The full path: a client that typed a hostname reaches a server behind NAT,
// and that server sees the player's real address rather than the tunnel's.
func TestMinecraftJoinForwardsRealIP(t *testing.T) {
	backend := mctest.NewServer(t)
	h := mcHarness(t, backend.Addr)

	client := mctest.Dial(t, h.MCAddr, "play.mc.example.com", mcproto.StateLogin)
	client.Login(t, "Notch")

	join := backend.NextJoin(t)
	if join.Username != "Notch" {
		t.Fatalf("username = %q", join.Username)
	}
	// The host field must survive the rewrite, since the server matches on it.
	if join.Host != "play.mc.example.com" {
		t.Fatalf("host = %q", join.Host)
	}
	// Without forwarding this would be the tunnel, which is what breaks bans
	// and every IP-aware plugin.
	if join.ForwardedIP == "" {
		t.Fatal("no forwarded IP in the handshake")
	}
	// The UUID must be the one a vanilla offline-mode server would compute, or
	// player data and whitelist entries point at a different player.
	if want := mcproto.OfflineUUID("Notch"); join.ForwardedUUID != want {
		t.Fatalf("forwarded uuid = %q, want %q", join.ForwardedUUID, want)
	}
}

// Server-list pings send no login packet. Holding one waiting for a login that
// never arrives would hang the client's server list.
func TestMinecraftStatusPingPassesThrough(t *testing.T) {
	backend := mctest.NewServer(t)
	h := mcHarness(t, backend.Addr)

	mctest.Dial(t, h.MCAddr, "play.mc.example.com", mcproto.StateStatus)

	join := backend.NextJoin(t)
	if join.Handshake.NextState != mcproto.StateStatus {
		t.Fatalf("next state = %d", join.Handshake.NextState)
	}
	if join.Host != "play.mc.example.com" {
		t.Fatalf("host = %q", join.Host)
	}
	// A status ping carries no forwarding payload, so the address field should
	// be the plain hostname.
	if join.ForwardedIP != "" {
		t.Fatalf("status ping was rewritten: ip = %q", join.ForwardedIP)
	}
}

// One public port serves every Minecraft tunnel, routed by what the player
// typed -- that is the whole point of sniffing the handshake.
func TestMinecraftRoutesTwoTunnelsOnOnePort(t *testing.T) {
	survival := mctest.NewServer(t)
	creative := mctest.NewServer(t)

	h := Start(t, Options{
		Minecraft: true,
		Grants:    []store.Grant{{Kind: store.GrantWildcard, Value: ".mc.example.com"}},
		Tunnels: []muxproto.TunnelSpec{
			{Name: "survival", Kind: muxproto.KindMinecraft,
				Host: "survival.mc.example.com", LocalAddr: survival.Addr},
			{Name: "creative", Kind: muxproto.KindMinecraft,
				Host: "creative.mc.example.com", LocalAddr: creative.Addr},
		},
		Transforms: map[muxproto.Kind]agent.Transform{
			muxproto.KindMinecraft: agent.MinecraftTransform,
		},
	})

	mctest.Dial(t, h.MCAddr, "survival.mc.example.com", mcproto.StateLogin).Login(t, "PlayerA")
	mctest.Dial(t, h.MCAddr, "creative.mc.example.com", mcproto.StateLogin).Login(t, "PlayerB")

	if got := survival.NextJoin(t); got.Username != "PlayerA" {
		t.Fatalf("survival server saw %q", got.Username)
	}
	if got := creative.NextJoin(t); got.Username != "PlayerB" {
		t.Fatalf("creative server saw %q", got.Username)
	}
}

// A hostname nobody serves must be dropped, not routed somewhere arbitrary.
func TestMinecraftUnknownHostIsDropped(t *testing.T) {
	backend := mctest.NewServer(t)
	h := mcHarness(t, backend.Addr)

	mctest.Dial(t, h.MCAddr, "nobody.mc.example.com", mcproto.StateLogin).Login(t, "Stranger")

	// A served hostname is connected afterwards, so this test fails loudly if
	// the path is broken rather than passing because nothing works at all.
	mctest.Dial(t, h.MCAddr, "play.mc.example.com", mcproto.StateLogin).Login(t, "Regular")

	join := backend.NextJoin(t)
	if join.Username != "Regular" {
		t.Fatalf("backend saw %q -- a connection for an unserved hostname was routed to it", join.Username)
	}
}
