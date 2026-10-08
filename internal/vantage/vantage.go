// Package vantage is the TimescaleDB-backed registry of federation vantages, plus the hub's
// federation CA (ca.go). The daemon and the `smoked vantage` CLI are separate processes that
// share this store, so a registered name and a minted client cert are usable immediately with
// no reload. The mTLS listener authenticates an agent's connection against the CA; this store
// decides whether that certificate is still authorized — its name must be registered and its
// serial one this registration issued — and tracks when each vantage was last seen.
package vantage

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/seitzbg/heliograph/internal/store"
)

const schema = `
CREATE TABLE IF NOT EXISTS vantages (
	name       text PRIMARY KEY,
	created_at timestamptz NOT NULL DEFAULT now(),
	last_seen  timestamptz
);

-- legacy_certs marks a vantage registered before client-certificate serials were recorded: its
-- deployed certificate's serial is unknown, so any CA-signed certificate bearing its name is
-- accepted until the vantage is revoked. The column is added with DEFAULT true, which Postgres
-- applies to every row already in the table at that moment, and the default is then switched to
-- false for every later INSERT. On every subsequent startup ADD COLUMN IF NOT EXISTS is a no-op and
-- nothing rewrites existing rows, so a vantage registered after the upgrade is never flagged. The
-- whole schema string runs as one implicit transaction (simple-protocol multi-statement Exec) and
-- ADD COLUMN holds an ACCESS EXCLUSIVE lock until commit, so no registration can land between the
-- two statements, and a concurrent process's ADD COLUMN waits and then finds the column present.
ALTER TABLE vantages ADD COLUMN IF NOT EXISTS legacy_certs boolean NOT NULL DEFAULT true;
ALTER TABLE vantages ALTER COLUMN legacy_certs SET DEFAULT false;

-- Every client certificate IssueClientCert mints, by serial. Revoke deletes the vantages row and
-- this cascade drops its serials, so certificates issued before a revoke stay rejected even after
-- the same name is registered again.
CREATE TABLE IF NOT EXISTS vantage_certs (
	name      text NOT NULL REFERENCES vantages(name) ON DELETE CASCADE,
	serial    text NOT NULL,
	issued_at timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (name, serial)
);

CREATE TABLE IF NOT EXISTS vantage_ca (
	id         int PRIMARY KEY DEFAULT 1,
	cert_pem   bytea NOT NULL,
	key_pem    bytea NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now()
);

-- One-time migration from the pre-mTLS key store: preserve registered vantage
-- names, then drop the obsolete key-hash table. No-op on a fresh DB, idempotent
-- on reruns (the table is gone after the first upgrade). It runs after the
-- legacy_certs default flips to false: a pre-mTLS vantage never held a
-- certificate, so it needs a recorded one from vantage add like any new vantage.
DO $$
BEGIN
	IF to_regclass('vantage_keys') IS NOT NULL THEN
		INSERT INTO vantages (name, created_at, last_seen)
			SELECT name, created_at, last_seen FROM vantage_keys
			ON CONFLICT (name) DO NOTHING;
		DROP TABLE IF EXISTS vantage_keys;
	END IF;
END $$;`

// reserved is the hub's own vantage name — it mirrors store.DefaultVantage.
// Verified with `go build` that importing internal/store here does not actually
// create an import cycle, but this package is the vantage registry/CA store and
// deliberately doesn't depend on internal/store (the timeseries sink) for one
// scalar, so the value is duplicated here as a small documented const instead.
// Registering a vantage named reserved would let an agent authenticate as the hub's
// own vantage, conflating its ingested rounds with the hub's authoritative
// locally-probed data.
const reserved = "local"

// ErrInvalidName and ErrReserved are client-input errors from Register: a bad name shape, or the
// reserved hub name "local". The admin API maps these to a 400/409 rather than a generic 5xx, so the
// operator sees a useful reason instead of "store unavailable" (CODE_REVIEW L5).
var (
	ErrInvalidName = errors.New("vantage: invalid name (use letters, digits, . _ -)")
	ErrReserved    = errors.New(`vantage: "local" is reserved for the hub`)
)

// ErrNotRegistered is IssueClientCert's error for a name with no registry row (never registered,
// or revoked between Register and the mint): a certificate is only ever issued, and recorded, for
// a registered vantage.
var ErrNotRegistered = errors.New("vantage: not registered")

// Info is a vantage's public metadata.
type Info struct {
	Name     string
	Created  time.Time
	LastSeen time.Time // zero = never connected
}

type Store struct{ pool *pgxpool.Pool }

func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("vantage: connect: %w", err)
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("vantage: migrate: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

// Register idempotently reserves name in the vantage registry — a re-Register of an existing
// name is a no-op, not an error, so provisioning is safe to retry. It rejects a malformed name
// or the reserved hub name "local" before ever touching the pool. Registering a name creates no
// credential: only certificates IssueClientCert mints after this registration are authorized,
// so re-registering a revoked name does not revive certificates issued before the revoke.
func (s *Store) Register(ctx context.Context, name string) error {
	if !ValidName(name) {
		return ErrInvalidName
	}
	if name == reserved {
		return ErrReserved
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO vantages (name) VALUES ($1) ON CONFLICT (name) DO NOTHING`, name); err != nil {
		return fmt.Errorf("vantage: register: %w", err)
	}
	return nil
}

// IsActive reports whether a client certificate with CommonName name and the given serial is a
// currently authorized credential, and bumps that vantage's last_seen when it is. It is
// authorized when name is registered (not revoked) and either IssueClientCert recorded serial for
// this registration, or the vantage is a legacy registration from before serials were recorded.
// false, nil (not an error) for an unknown name, an unrecorded serial, or a nil serial; a
// rejected certificate leaves last_seen untouched.
func (s *Store) IsActive(ctx context.Context, name string, serial *big.Int) (bool, error) {
	var active bool
	err := s.pool.QueryRow(ctx, `
		UPDATE vantages v SET last_seen = now()
		WHERE v.name = $1
		  AND (v.legacy_certs OR EXISTS (
		      SELECT 1 FROM vantage_certs c WHERE c.name = v.name AND c.serial = $2))
		RETURNING true`, name, serialKey(serial)).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("vantage: is active: %w", err)
	}
	return active, nil
}

func (s *Store) List(ctx context.Context) ([]Info, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, created_at, last_seen FROM vantages ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Info
	for rows.Next() {
		var in Info
		var last *time.Time
		if err := rows.Scan(&in.Name, &in.Created, &last); err != nil {
			return nil, err
		}
		if last != nil {
			in.LastSeen = *last
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// Revoke removes name from the registry, which also drops every certificate serial recorded for
// it (ON DELETE CASCADE). Every certificate issued for name so far is rejected from its next
// request on, and stays rejected if the name is later registered again.
func (s *Store) Revoke(ctx context.Context, name string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM vantages WHERE name=$1`, name)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ValidName reports whether name is an acceptable vantage identifier. It delegates to
// store.ValidVantageName, the single source of truth shared with config loading and the
// read API — names flow into cert Subject CNs, URLs, DB rows, and file paths, so a name
// can't carry spaces, colons, or newlines.
func ValidName(name string) bool { return store.ValidVantageName(name) }

// serialKey is the vantage_certs.serial encoding of a certificate serial: lowercase hex. A nil
// serial maps to "", which never matches a recorded serial.
func serialKey(serial *big.Int) string {
	if serial == nil {
		return ""
	}
	return serial.Text(16)
}
