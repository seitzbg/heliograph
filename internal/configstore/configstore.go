// Package configstore is the TimescaleDB-backed store for DB-managed config. It holds
// a single versioned config fragment (target branches only) that smoked concatenates
// with the YAML config on every load/reload. Writes use optimistic concurrency so a
// stale editor can't clobber a newer change.
package configstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const schema = `
CREATE TABLE IF NOT EXISTS config_fragment (
	id         smallint    PRIMARY KEY DEFAULT 1,
	version    integer     NOT NULL,
	doc        jsonb       NOT NULL,
	updated_at timestamptz NOT NULL DEFAULT now()
);`

// ErrConflict is returned by Set when expectedVersion does not match the stored version
// (optimistic concurrency — re-read and retry).
var ErrConflict = errors.New("configstore: version conflict")

type Store struct{ pool *pgxpool.Pool }

func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("configstore: connect: %w", err)
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, fmt.Errorf("configstore: migrate: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

// Get returns the stored fragment and its version. An absent row (no DB config yet)
// returns (nil, 0, nil) — the caller treats that as "no DB source".
func (s *Store) Get(ctx context.Context) (json.RawMessage, int, error) {
	var doc json.RawMessage
	var version int
	err := s.pool.QueryRow(ctx, `SELECT doc, version FROM config_fragment WHERE id = 1`).Scan(&doc, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("configstore: get: %w", err)
	}
	return doc, version, nil
}

// Set writes doc with optimistic concurrency. expectedVersion is the version the caller
// last read (0 for the first-ever write). A mismatch returns ErrConflict and writes
// nothing; on success the stored version becomes expectedVersion+1.
func (s *Store) Set(ctx context.Context, doc json.RawMessage, expectedVersion int) error {
	if expectedVersion == 0 {
		ct, err := s.pool.Exec(ctx,
			`INSERT INTO config_fragment (id, version, doc) VALUES (1, 1, $1)
			 ON CONFLICT (id) DO NOTHING`, doc)
		if err != nil {
			return fmt.Errorf("configstore: set(insert): %w", err)
		}
		if ct.RowsAffected() == 0 {
			return ErrConflict
		}
		return nil
	}
	ct, err := s.pool.Exec(ctx,
		`UPDATE config_fragment SET version = version + 1, doc = $1, updated_at = now()
		 WHERE id = 1 AND version = $2`, doc, expectedVersion)
	if err != nil {
		return fmt.Errorf("configstore: set(update): %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrConflict
	}
	return nil
}
