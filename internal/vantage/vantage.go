// Package vantage is the TimescaleDB-backed registry of federation vantages, plus the hub's
// federation CA (ca.go). The daemon and the `smoked vantage` CLI are separate processes that
// share this store, so a registered name and a minted client cert are usable immediately with
// no reload. mTLS (a later task) authenticates an agent's connection against the CA; this store
// only tracks which vantage names exist and when each was last seen.
package vantage

import (
	"context"
	"errors"
	"fmt"
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
CREATE TABLE IF NOT EXISTS vantage_ca (
	id         int PRIMARY KEY DEFAULT 1,
	cert_pem   bytea NOT NULL,
	key_pem    bytea NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now()
);

-- One-time migration from the pre-mTLS key store: preserve registered vantage
-- names, then drop the obsolete key-hash table. No-op on a fresh DB, idempotent
-- on reruns (the table is gone after the first upgrade).
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
// or the reserved hub name "local" before ever touching the pool.
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

// IsActive bumps last_seen for name and reports whether it is a known (registered, not revoked)
// vantage. false, nil (not an error) for an unknown name.
func (s *Store) IsActive(ctx context.Context, name string) (bool, error) {
	var active bool
	err := s.pool.QueryRow(ctx,
		`UPDATE vantages SET last_seen=now() WHERE name=$1 RETURNING true`, name).Scan(&active)
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
