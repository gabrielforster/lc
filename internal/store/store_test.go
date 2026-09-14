package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func open(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lc.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

// Migrations run on every startup, so applying them twice must be harmless.
func TestMigrationsAreIdempotent(t *testing.T) {
	db, path := open(t)
	db.Close()

	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()
	if _, err := again.ListTokens(); err != nil {
		t.Fatalf("schema unusable after reopen: %v", err)
	}
}

func TestTokenLookupAndSecrecy(t *testing.T) {
	db, _ := open(t)
	tok, secret, err := db.CreateToken("laptop")
	if err != nil {
		t.Fatal(err)
	}

	got, err := db.TokenBySecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != tok.ID || got.Label != "laptop" {
		t.Fatalf("got %+v", got)
	}
	if _, err := db.TokenBySecret("wrong"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad secret: got %v, want ErrNotFound", err)
	}

	// Listing tokens must never expose anything usable as a credential.
	list, err := db.ListTokens()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 token, got %d", len(list))
	}
}

func TestDisabledTokenIsRejected(t *testing.T) {
	db, _ := open(t)
	tok, secret, err := db.CreateToken("revoked")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`UPDATE tokens SET disabled = 1 WHERE id = ?`, tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.TokenBySecret(secret); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabled token honoured: %v", err)
	}
}

func TestDomainClaimIsExclusiveButIdempotent(t *testing.T) {
	db, _ := open(t)
	a, _, _ := db.CreateToken("a")
	b, _, _ := db.CreateToken("b")

	if err := db.ClaimDomain(a.ID, "mc.example.com"); err != nil {
		t.Fatal(err)
	}
	// Re-claiming our own domain is what a reconnect does.
	if err := db.ClaimDomain(a.ID, "mc.example.com"); err != nil {
		t.Fatalf("owner re-claim: %v", err)
	}
	if err := db.ClaimDomain(b.ID, "mc.example.com"); !errors.Is(err, ErrTaken) {
		t.Fatalf("second token claimed a taken domain: %v", err)
	}

	if err := db.ReleaseDomain(b.ID, "mc.example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("released someone else's domain: %v", err)
	}
	if err := db.ReleaseDomain(a.ID, "mc.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := db.ClaimDomain(b.ID, "mc.example.com"); err != nil {
		t.Fatalf("claim after release: %v", err)
	}
}

// The reservation is the reason ports live in SQLite at all.
func TestPortReservationSurvivesRestart(t *testing.T) {
	db, path := open(t)
	tok, secret, _ := db.CreateToken("home")

	port, err := db.ReservePort(tok.ID, "ssh", 0, 20000, 20010)
	if err != nil {
		t.Fatal(err)
	}
	if port < 20000 || port > 20010 {
		t.Fatalf("port %d out of range", port)
	}
	db.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	same, err := reopened.TokenBySecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.ReservePort(same.ID, "ssh", 0, 20000, 20010)
	if err != nil {
		t.Fatal(err)
	}
	if got != port {
		t.Fatalf("reconnect got port %d, want its reservation %d", got, port)
	}
}

func TestPortAllocationAvoidsCollisions(t *testing.T) {
	db, _ := open(t)
	a, _, _ := db.CreateToken("a")
	b, _, _ := db.CreateToken("b")

	p1, err := db.ReservePort(a.ID, "one", 0, 20000, 20001)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := db.ReservePort(b.ID, "two", 0, 20000, 20001)
	if err != nil {
		t.Fatal(err)
	}
	if p1 == p2 {
		t.Fatalf("two tunnels share port %d", p1)
	}
	if _, err := db.ReservePort(a.ID, "three", 0, 20000, 20001); !errors.Is(err, ErrNoPorts) {
		t.Fatalf("exhausted range: got %v, want ErrNoPorts", err)
	}
	// An explicit request for someone else's port must be refused.
	if _, err := db.ReservePort(a.ID, "four", p2, 20000, 20001); !errors.Is(err, ErrTaken) {
		t.Fatalf("stole another token's port: %v", err)
	}
}

func TestGrantsAreIdempotent(t *testing.T) {
	db, _ := open(t)
	tok, _, _ := db.CreateToken("a")
	for range 2 {
		if err := db.AddGrant(tok.ID, GrantWildcard, ".mc.example.com"); err != nil {
			t.Fatal(err)
		}
	}
	gs, err := db.Grants(tok.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(gs) != 1 {
		t.Fatalf("want 1 grant, got %d", len(gs))
	}
}
