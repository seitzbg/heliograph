// Package vantage is the TimescaleDB-backed registry of federation vantages and their
// API keys. The daemon and the `smoked vantage` CLI are separate processes that share
// this store, so a minted key is usable immediately with no reload. Only a salted hash
// of each key's secret is persisted — the plaintext is shown once and cannot be recovered.
package vantage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const schema = `
CREATE TABLE IF NOT EXISTS vantage_keys (
	name       text PRIMARY KEY,
	key_id     text NOT NULL UNIQUE,
	key_hash   bytea NOT NULL,
	salt       bytea NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now(),
	last_seen  timestamptz
);`

// Info is a vantage's public metadata — never its key material.
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

func randHex(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func hashSecret(salt []byte, secret string) []byte {
	sum := sha256.Sum256(append(append([]byte{}, salt...), []byte(secret)...))
	return sum[:]
}

// Add mints a fresh key for name — rotating any existing one — and returns the one-time
// full key `smk_<keyId>_<secret>`. Only the salted hash is stored.
func (s *Store) Add(ctx context.Context, name string) (string, error) {
	if name == "" {
		return "", errors.New("vantage: empty name")
	}
	keyID, err := randHex(6)
	if err != nil {
		return "", err
	}
	secret, err := randHex(32)
	if err != nil {
		return "", err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO vantage_keys (name, key_id, key_hash, salt, created_at, last_seen)
		VALUES ($1,$2,$3,$4, now(), NULL)
		ON CONFLICT (name) DO UPDATE SET key_id=$2, key_hash=$3, salt=$4, created_at=now(), last_seen=NULL`,
		name, keyID, hashSecret(salt, secret), salt)
	if err != nil {
		return "", fmt.Errorf("vantage: add: %w", err)
	}
	return "smk_" + keyID + "_" + secret, nil
}

// Verify parses a presented key, looks it up by key id, constant-time compares the salted
// hash, and on success bumps last_seen. It returns ok=false for any malformed, unknown, or
// mismatched key — never revealing which, to avoid an authentication oracle.
func (s *Store) Verify(ctx context.Context, presented string) (name string, ok bool, err error) {
	parts := strings.Split(presented, "_")
	if len(parts) != 3 || parts[0] != "smk" || parts[1] == "" || parts[2] == "" {
		return "", false, nil
	}
	keyID, secret := parts[1], parts[2]
	var (
		gotName    string
		hash, salt []byte
	)
	err = s.pool.QueryRow(ctx, `SELECT name, key_hash, salt FROM vantage_keys WHERE key_id=$1`, keyID).
		Scan(&gotName, &hash, &salt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("vantage: verify: %w", err)
	}
	if subtle.ConstantTimeCompare(hashSecret(salt, secret), hash) != 1 {
		return "", false, nil
	}
	_, _ = s.pool.Exec(ctx, `UPDATE vantage_keys SET last_seen=now() WHERE key_id=$1`, keyID) // best-effort
	return gotName, true, nil
}

func (s *Store) List(ctx context.Context) ([]Info, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, created_at, last_seen FROM vantage_keys ORDER BY name`)
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
	tag, err := s.pool.Exec(ctx, `DELETE FROM vantage_keys WHERE name=$1`, name)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// AgentSnippet renders a ready-to-paste smoke-agent config block for a freshly minted key.
func AgentSnippet(name, fullKey string) string {
	return fmt.Sprintf("# smoke-agent config for vantage %q\n# hub URL: set to your https reverse-proxy endpoint\nvantage: %s\nkey: %s\n", name, name, fullKey)
}
