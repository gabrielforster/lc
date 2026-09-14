package store

import (
	"database/sql"
	"errors"
	"time"
)

// ErrTaken means another token already owns the resource.
var ErrTaken = errors.New("store: already claimed by another token")

// ClaimDomain records host as owned by tokenID.
//
// Claiming is idempotent for the owner, so an agent reconnecting and
// re-claiming its own hostname succeeds rather than colliding with itself.
func (d *DB) ClaimDomain(tokenID int64, host string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var owner int64
	err = tx.QueryRow(`SELECT token_id FROM domains WHERE host = ?`, host).Scan(&owner)
	switch {
	case err == nil:
		if owner != tokenID {
			return ErrTaken
		}
		return tx.Commit() // already ours
	case errors.Is(err, sql.ErrNoRows):
	default:
		return err
	}

	if _, err := tx.Exec(
		`INSERT INTO domains (host, token_id, claimed_at) VALUES (?, ?, ?)`,
		host, tokenID, time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// DomainOwner returns the token owning host, or ErrNotFound.
func (d *DB) DomainOwner(host string) (int64, error) {
	var id int64
	err := d.sql.QueryRow(`SELECT token_id FROM domains WHERE host = ?`, host).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

// ReleaseDomain drops a claim. Releasing a domain owned by someone else fails
// rather than silently doing nothing.
func (d *DB) ReleaseDomain(tokenID int64, host string) error {
	res, err := d.sql.Exec(`DELETE FROM domains WHERE host = ? AND token_id = ?`, host, tokenID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Domains lists the hostnames owned by a token.
func (d *DB) Domains(tokenID int64) ([]string, error) {
	rows, err := d.sql.Query(`SELECT host FROM domains WHERE token_id = ? ORDER BY host`, tokenID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// AllDomains lists every claimed hostname. The TLS layer uses this as the
// autocert allowlist, so a certificate can only be requested for a name some
// token actually owns.
func (d *DB) AllDomains() ([]string, error) {
	rows, err := d.sql.Query(`SELECT host FROM domains ORDER BY host`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ReservePort returns the public port for (tokenID, tunnelName), allocating one
// from [min,max] on first use.
//
// The reservation is persistent, which is the point: an agent that reconnects,
// or a server that restarts, hands the same tunnel back the same port. That
// removes the "your port changes every reconnect" wart of purely dynamic
// allocation while keeping allocation automatic.
//
// A non-zero want asks for a specific port, which succeeds only if it is free
// or already this tunnel's.
func (d *DB) ReservePort(tokenID int64, tunnelName string, want, min, max int) (int, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// An existing reservation wins, so reconnects are stable.
	var existing int
	err = tx.QueryRow(`SELECT port FROM ports WHERE token_id = ? AND tunnel_name = ?`,
		tokenID, tunnelName).Scan(&existing)
	switch {
	case err == nil:
		if want != 0 && want != existing {
			// An explicit, different request replaces the old reservation.
			if err := reassign(tx, tokenID, tunnelName, want); err != nil {
				return 0, err
			}
			return want, tx.Commit()
		}
		return existing, tx.Commit()
	case errors.Is(err, sql.ErrNoRows):
	default:
		return 0, err
	}

	port := want
	if port == 0 {
		if port, err = freePort(tx, min, max); err != nil {
			return 0, err
		}
	}
	if err := reassign(tx, tokenID, tunnelName, port); err != nil {
		return 0, err
	}
	return port, tx.Commit()
}

// reassign points a port at a tunnel, refusing to steal another token's port.
func reassign(tx *sql.Tx, tokenID int64, tunnelName string, port int) error {
	var owner int64
	err := tx.QueryRow(`SELECT token_id FROM ports WHERE port = ?`, port).Scan(&owner)
	switch {
	case err == nil:
		if owner != tokenID {
			return ErrTaken
		}
	case errors.Is(err, sql.ErrNoRows):
	default:
		return err
	}

	if _, err := tx.Exec(`DELETE FROM ports WHERE port = ? OR (token_id = ? AND tunnel_name = ?)`,
		port, tokenID, tunnelName); err != nil {
		return err
	}
	_, err = tx.Exec(
		`INSERT INTO ports (port, token_id, tunnel_name, reserved_at) VALUES (?, ?, ?, ?)`,
		port, tokenID, tunnelName, time.Now().Unix())
	return err
}

// ErrNoPorts means the configured range is exhausted.
var ErrNoPorts = errors.New("store: no free port in range")

func freePort(tx *sql.Tx, min, max int) (int, error) {
	if min <= 0 || max < min {
		return 0, errors.New("store: invalid port range")
	}
	taken := map[int]bool{}
	rows, err := tx.Query(`SELECT port FROM ports WHERE port BETWEEN ? AND ?`, min, max)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return 0, err
		}
		taken[p] = true
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for p := min; p <= max; p++ {
		if !taken[p] {
			return p, nil
		}
	}
	return 0, ErrNoPorts
}

// PortReservation is one persisted port assignment.
type PortReservation struct {
	Port       int
	TunnelName string
}

// Ports lists a token's reservations.
func (d *DB) Ports(tokenID int64) ([]PortReservation, error) {
	rows, err := d.sql.Query(
		`SELECT port, tunnel_name FROM ports WHERE token_id = ? ORDER BY port`, tokenID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PortReservation
	for rows.Next() {
		var r PortReservation
		if err := rows.Scan(&r.Port, &r.TunnelName); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
