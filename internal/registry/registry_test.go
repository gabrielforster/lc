package registry

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/gabrielforster/lc/internal/muxproto"
	"github.com/gabrielforster/lc/internal/store"
)

func newReg(t *testing.T, cfg Config) (*Registry, *store.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "lc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if cfg.PortMin == 0 {
		cfg.PortMin, cfg.PortMax = 20000, 20010
	}
	return New(db, cfg), db
}

func TestHostGrantsGateRegistration(t *testing.T) {
	r, db := newReg(t, Config{})
	tok, _, _ := db.CreateToken("a")
	db.AddGrant(tok.ID, store.GrantWildcard, ".mc.example.com")

	spec := muxproto.TunnelSpec{Name: "mc", Kind: muxproto.KindMinecraft, Host: "one.mc.example.com"}
	if _, err := r.Register(tok, nil, spec, "id1"); err != nil {
		t.Fatalf("granted host refused: %v", err)
	}

	// Outside the zone, and the zone apex itself, are both out of scope.
	for _, host := range []string{"elsewhere.example.com", "mc.example.com"} {
		spec := muxproto.TunnelSpec{Name: "x", Kind: muxproto.KindHTTP, Host: host}
		if _, err := r.Register(tok, nil, spec, "id-"+host); !errors.Is(err, ErrForbidden) {
			t.Fatalf("host %q: got %v, want ErrForbidden", host, err)
		}
	}
}

func TestCustomDomainsOffMeansNoUngrantedHost(t *testing.T) {
	r, db := newReg(t, Config{AllowCustomDomains: false})
	tok, _, _ := db.CreateToken("a")

	res := r.Claim(tok, "free.example.com")
	if res.OK || res.Reason != muxproto.ClaimDisabled {
		t.Fatalf("got %+v, want disabled", res)
	}
}

func TestClaimReportsWhy(t *testing.T) {
	r, db := newReg(t, Config{AllowCustomDomains: true, ReservedHosts: []string{"lc.example.com"}})
	a, _, _ := db.CreateToken("a")
	b, _, _ := db.CreateToken("b")

	if res := r.Claim(a, "mine.example.com"); !res.OK || !res.NeedsDNS {
		t.Fatalf("first claim: %+v", res)
	}
	// Re-claiming our own name is what a reconnect does.
	if res := r.Claim(a, "mine.example.com"); !res.OK {
		t.Fatalf("owner re-claim: %+v", res)
	}
	if res := r.Claim(b, "mine.example.com"); res.OK || res.Reason != muxproto.ClaimTaken {
		t.Fatalf("taken: got %+v", res)
	}
	if res := r.Claim(b, "lc.example.com"); res.OK || res.Reason != muxproto.ClaimReserved {
		t.Fatalf("reserved: got %+v", res)
	}
	if res := r.Claim(b, "not a host"); res.OK || res.Reason != muxproto.ClaimInvalid {
		t.Fatalf("invalid: got %+v", res)
	}
}

// A claimed domain must outlive the session that claimed it, or a reconnecting
// agent would lose its hostname.
func TestClaimedDomainAllowsLaterRegistration(t *testing.T) {
	r, db := newReg(t, Config{AllowCustomDomains: true})
	tok, _, _ := db.CreateToken("a")

	if res := r.Claim(tok, "mine.example.com"); !res.OK {
		t.Fatalf("claim: %+v", res)
	}
	spec := muxproto.TunnelSpec{Name: "web", Kind: muxproto.KindHTTP, Host: "mine.example.com"}
	tun, err := r.Register(tok, nil, spec, "id1")
	if err != nil {
		t.Fatalf("register own claimed domain: %v", err)
	}

	// While it is live, nobody else serves that host.
	other, _, _ := db.CreateToken("b")
	if _, err := r.Register(other, nil, spec, "id2"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("other token: got %v, want ErrForbidden", err)
	}

	// Releasing the live tunnel keeps the durable claim.
	r.Release(tun)
	if _, ok := r.LookupHost("mine.example.com"); ok {
		t.Fatal("released tunnel still routable")
	}
	if _, err := r.Register(tok, nil, spec, "id3"); err != nil {
		t.Fatalf("reconnect lost its domain: %v", err)
	}
}

func TestTCPPortAssignmentAndLookup(t *testing.T) {
	r, db := newReg(t, Config{})
	tok, _, _ := db.CreateToken("a")
	db.AddGrant(tok.ID, store.GrantPortAuto, "")

	spec := muxproto.TunnelSpec{Name: "ssh", Kind: muxproto.KindTCP, LocalAddr: "127.0.0.1:22"}
	tun, err := r.Register(tok, nil, spec, "id1")
	if err != nil {
		t.Fatal(err)
	}
	if tun.PublicPort < 20000 || tun.PublicPort > 20010 {
		t.Fatalf("port %d out of configured range", tun.PublicPort)
	}
	if got, ok := r.LookupPort(tun.PublicPort); !ok || got.ID != tun.ID {
		t.Fatal("port lookup failed")
	}

	// Without a port grant, a tcp tunnel is refused.
	noGrant, _, _ := db.CreateToken("b")
	if _, err := r.Register(noGrant, nil, spec, "id2"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("got %v, want ErrForbidden", err)
	}
}

func TestHostLookupIgnoresCaseAndPort(t *testing.T) {
	r, db := newReg(t, Config{})
	tok, _, _ := db.CreateToken("a")
	db.AddGrant(tok.ID, store.GrantHost, "mc.example.com")

	spec := muxproto.TunnelSpec{Name: "mc", Kind: muxproto.KindMinecraft, Host: "MC.Example.com"}
	if _, err := r.Register(tok, nil, spec, "id1"); err != nil {
		t.Fatal(err)
	}
	// Clients send the Host header with a port, and with arbitrary casing.
	for _, probe := range []string{"mc.example.com", "MC.EXAMPLE.COM", "mc.example.com:25565"} {
		if _, ok := r.LookupHost(probe); !ok {
			t.Fatalf("lookup %q failed", probe)
		}
	}
}
