package lctest

import (
	"testing"

	"github.com/gabrielforster/lc/internal/muxproto"
	"github.com/gabrielforster/lc/internal/store"
)

// Claiming happens over the live control stream, so it must work while tunnels
// are already running.
func TestClaimDomainOverLiveSession(t *testing.T) {
	h := Start(t, Options{
		AllowCustomDomains: true,
		Grants:             []store.Grant{{Kind: store.GrantPortAuto}},
		Tunnels: []muxproto.TunnelSpec{{
			Name: "echo", Kind: muxproto.KindTCP, LocalAddr: EchoService(t),
		}},
	})

	res, err := h.Agent.ClaimDomain("chosen.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("claim refused: %+v", res)
	}
	// The response must say the name still needs DNS pointed at the server,
	// since claiming only records intent here.
	if !res.NeedsDNS {
		t.Fatal("successful claim did not flag the DNS requirement")
	}

	// The claim must be durable, not just live session state.
	tok, err := h.DB.TokenBySecret(h.Token)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := h.DB.DomainOwner("chosen.example.com")
	if err != nil {
		t.Fatalf("claim was not persisted: %v", err)
	}
	if owner != tok.ID {
		t.Fatalf("domain owned by token %d, want %d", owner, tok.ID)
	}
}

func TestClaimRefusedWhenDisabled(t *testing.T) {
	h := Start(t, Options{
		AllowCustomDomains: false,
		Grants:             []store.Grant{{Kind: store.GrantPortAuto}},
		Tunnels: []muxproto.TunnelSpec{{
			Name: "echo", Kind: muxproto.KindTCP, LocalAddr: EchoService(t),
		}},
	})

	res, err := h.Agent.ClaimDomain("chosen.example.com")
	if err != nil {
		t.Fatal(err)
	}
	// The reason is typed so a caller can explain the failure specifically.
	if res.OK || res.Reason != muxproto.ClaimDisabled {
		t.Fatalf("got %+v, want disabled", res)
	}
}

// A claimed domain should be usable as a tunnel host on the next connection.
func TestClaimedDomainServesTraffic(t *testing.T) {
	h := Start(t, Options{
		AllowCustomDomains: true,
		Grants:             []store.Grant{{Kind: store.GrantPortAuto}},
		Tunnels: []muxproto.TunnelSpec{{
			Name: "echo", Kind: muxproto.KindTCP, LocalAddr: EchoService(t),
		}},
	})
	if res, err := h.Agent.ClaimDomain("later.example.com"); err != nil || !res.OK {
		t.Fatalf("claim: %+v %v", res, err)
	}

	// Registering that host now succeeds purely on the strength of the claim.
	tok, _ := h.DB.TokenBySecret(h.Token)
	spec := muxproto.TunnelSpec{Name: "web", Kind: muxproto.KindHTTP, Host: "later.example.com"}
	if _, err := h.Registry.Register(tok, nil, spec, "later-id"); err != nil {
		t.Fatalf("claimed domain rejected at registration: %v", err)
	}
}

// list and release ride the same control stream as claim, so the dispatch must
// keep replies matched to requests.
func TestDomainListAndRelease(t *testing.T) {
	h := Start(t, Options{
		AllowCustomDomains: true,
		Grants:             []store.Grant{{Kind: store.GrantPortAuto}},
		Tunnels: []muxproto.TunnelSpec{{
			Name: "echo", Kind: muxproto.KindTCP, LocalAddr: EchoService(t),
		}},
	})

	for _, d := range []string{"one.example.com", "two.example.com"} {
		if res, err := h.Agent.ClaimDomain(d); err != nil || !res.OK {
			t.Fatalf("claim %s: %+v %v", d, res, err)
		}
	}

	got, err := h.Agent.Domains()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "one.example.com" || got[1] != "two.example.com" {
		t.Fatalf("domains = %v", got)
	}

	if res, err := h.Agent.ReleaseDomain("one.example.com"); err != nil || !res.OK {
		t.Fatalf("release: %+v %v", res, err)
	}
	got, err = h.Agent.Domains()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "two.example.com" {
		t.Fatalf("after release, domains = %v", got)
	}

	// Releasing something we do not own must say so rather than succeed.
	res, err := h.Agent.ReleaseDomain("one.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || res.Reason != muxproto.ClaimNotFound {
		t.Fatalf("got %+v, want not_found", res)
	}
}
