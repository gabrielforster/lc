// Package store is the persistent registry: tokens, their grants, claimed
// domains and reserved ports.
//
// It is SQLite rather than a config file specifically so a future lc UI can
// read and write the same state with no migration. That also means this package
// should stay free of tunnel-runtime concerns: it holds only what must outlive
// the process. Live yamux sessions and per-tunnel transports belong in the
// registry, not here.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// DB wraps the SQLite handle.
type DB struct{ sql *sql.DB }

// ErrNotFound is returned when a lookup finds no row.
var ErrNotFound = errors.New("store: not found")

// Open opens (creating if needed) the database at path and applies migrations.
func Open(path string) (*DB, error) {
	// Busy timeout keeps concurrent writers — the daemon and, later, the UI —
	// from failing outright on lock contention.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	sdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db := &DB{sql: sdb}
	if err := db.migrate(); err != nil {
		sdb.Close()
		return nil, err
	}
	return db, nil
}

func (d *DB) Close() error { return d.sql.Close() }

// migrations are applied in order and recorded, so applying them repeatedly is
// a no-op. Never edit an applied migration; append a new one.
var migrations = []string{
	`CREATE TABLE tokens (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		token_hash TEXT NOT NULL UNIQUE,
		label      TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		disabled   INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE TABLE grants (
		id       INTEGER PRIMARY KEY AUTOINCREMENT,
		token_id INTEGER NOT NULL REFERENCES tokens(id) ON DELETE CASCADE,
		kind     TEXT NOT NULL,
		value    TEXT NOT NULL,
		UNIQUE(token_id, kind, value)
	)`,
	`CREATE TABLE domains (
		host       TEXT PRIMARY KEY,
		token_id   INTEGER NOT NULL REFERENCES tokens(id) ON DELETE CASCADE,
		claimed_at INTEGER NOT NULL
	)`,
	`CREATE TABLE ports (
		port        INTEGER PRIMARY KEY,
		token_id    INTEGER NOT NULL REFERENCES tokens(id) ON DELETE CASCADE,
		tunnel_name TEXT NOT NULL,
		reserved_at INTEGER NOT NULL,
		UNIQUE(token_id, tunnel_name)
	)`,
}

func (d *DB) migrate() error {
	if _, err := d.sql.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var have int
	err := d.sql.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&have)
	if err != nil {
		return err
	}
	for i := have; i < len(migrations); i++ {
		tx, err := d.sql.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_version (version) VALUES (?)`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// GrantKind is what a grant permits.
type GrantKind string

const (
	// GrantHost permits one exact hostname.
	GrantHost GrantKind = "host"
	// GrantWildcard permits any name under a zone, e.g. ".mc.example.com".
	GrantWildcard GrantKind = "wildcard"
	// GrantPort permits one exact public port.
	GrantPort GrantKind = "port"
	// GrantPortAuto permits automatic port assignment from the server's range.
	GrantPortAuto GrantKind = "port_auto"
)

// Token is an agent credential. The secret itself is never stored or returned
// here — only its hash — so a UI listing tokens cannot leak working credentials.
type Token struct {
	ID       int64
	Label    string
	Created  time.Time
	Disabled bool
}

// HashToken derives the stored form of a token secret.
//
// A plain SHA-256 is deliberate: these are high-entropy machine-generated
// secrets from NewSecret, not user-chosen passwords, so there is nothing for a
// slow KDF to protect against here.
func HashToken(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// NewSecret mints a 256-bit token secret.
func NewSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// CreateToken stores a new token and returns it along with the plaintext
// secret, which is the only time that secret is available.
func (d *DB) CreateToken(label string) (Token, string, error) {
	secret, err := NewSecret()
	if err != nil {
		return Token{}, "", err
	}
	now := time.Now()
	res, err := d.sql.Exec(`INSERT INTO tokens (token_hash, label, created_at) VALUES (?, ?, ?)`,
		HashToken(secret), label, now.Unix())
	if err != nil {
		return Token{}, "", err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Token{}, "", err
	}
	return Token{ID: id, Label: label, Created: now}, secret, nil
}

// TokenBySecret looks up a token by its plaintext secret. A disabled token is
// reported as not found, so callers cannot accidentally honour one.
func (d *DB) TokenBySecret(secret string) (Token, error) {
	var (
		t        Token
		created  int64
		disabled int
	)
	err := d.sql.QueryRow(
		`SELECT id, label, created_at, disabled FROM tokens WHERE token_hash = ?`,
		HashToken(secret),
	).Scan(&t.ID, &t.Label, &created, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, ErrNotFound
	}
	if err != nil {
		return Token{}, err
	}
	if disabled != 0 {
		return Token{}, ErrNotFound
	}
	t.Created = time.Unix(created, 0)
	t.Disabled = false
	return t, nil
}

// ListTokens returns all tokens, including disabled ones, for administration.
func (d *DB) ListTokens() ([]Token, error) {
	rows, err := d.sql.Query(`SELECT id, label, created_at, disabled FROM tokens ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Token
	for rows.Next() {
		var (
			t        Token
			created  int64
			disabled int
		)
		if err := rows.Scan(&t.ID, &t.Label, &created, &disabled); err != nil {
			return nil, err
		}
		t.Created = time.Unix(created, 0)
		t.Disabled = disabled != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// Grant is one permission attached to a token.
type Grant struct {
	Kind  GrantKind
	Value string
}

// AddGrant attaches a permission to a token. Re-adding an identical grant is a
// no-op rather than an error, so administration is idempotent.
func (d *DB) AddGrant(tokenID int64, kind GrantKind, value string) error {
	_, err := d.sql.Exec(
		`INSERT OR IGNORE INTO grants (token_id, kind, value) VALUES (?, ?, ?)`,
		tokenID, string(kind), value)
	return err
}

// Grants returns every grant held by a token.
func (d *DB) Grants(tokenID int64) ([]Grant, error) {
	rows, err := d.sql.Query(`SELECT kind, value FROM grants WHERE token_id = ?`, tokenID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Grant
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.Kind, &g.Value); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
