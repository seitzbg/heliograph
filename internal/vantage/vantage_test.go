package vantage

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run the TimescaleDB integration test")
	}
	s, err := New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(s.Close)
	if _, err := s.pool.Exec(context.Background(), "TRUNCATE vantages"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return s
}

func TestRegisterIsActiveListRevoke(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	if err := s.Register(ctx, "nyc"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Re-registering an existing name is idempotent, not an error.
	if err := s.Register(ctx, "nyc"); err != nil {
		t.Fatalf("re-Register: %v", err)
	}

	if active, err := s.IsActive(ctx, "nyc"); err != nil || !active {
		t.Fatalf("IsActive(nyc) = (%v,%v), want (true,nil)", active, err)
	}
	if active, err := s.IsActive(ctx, "ghost"); err != nil || active {
		t.Fatalf("IsActive(ghost) = (%v,%v), want (false,nil)", active, err)
	}

	// last_seen is set after a successful IsActive.
	infos, err := s.List(ctx)
	if err != nil || len(infos) != 1 || infos[0].Name != "nyc" {
		t.Fatalf("List = %v (err %v), want one nyc", infos, err)
	}
	if infos[0].LastSeen.IsZero() {
		t.Error("LastSeen is zero after a successful IsActive")
	}

	if removed, err := s.Revoke(ctx, "nyc"); err != nil || !removed {
		t.Fatalf("Revoke = (%v,%v), want (true,nil)", removed, err)
	}
	if active, err := s.IsActive(ctx, "nyc"); err != nil || active {
		t.Fatalf("IsActive(nyc) after Revoke = (%v,%v), want (false,nil)", active, err)
	}
	if removed, _ := s.Revoke(ctx, "nyc"); removed {
		t.Error("second Revoke removed=true, want false")
	}
}

// TestMigratesLegacyVantageKeysTable covers the upgrade path for an already-provisioned
// production DB: it seeds a pre-mTLS `vantage_keys` table (the old key-hash store, name/
// key_id/key_hash/salt/created_at/last_seen) with a registered vantage, then constructs a
// fresh Store — New()'s schema migration must carry the name/created_at/last_seen into the
// new `vantages` table (never the key material) and drop the obsolete table, so an upgrade
// doesn't silently lose every already-registered vantage.
func TestMigratesLegacyVantageKeysTable(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run the TimescaleDB integration test")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	// Start from a clean slate for this test's two tables.
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS vantages, vantage_keys`); err != nil {
		t.Fatalf("drop pre-existing tables: %v", err)
	}
	// Safety net: if New() below never runs (e.g. it fails before reaching the migration),
	// don't leave the legacy table behind for other tests/runs.
	defer func() { _, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS vantage_keys`) }()

	// Simulate a pre-mTLS production DB: the legacy key-hash table with a registered vantage.
	if _, err := pool.Exec(ctx, `
		CREATE TABLE vantage_keys (
			name       text PRIMARY KEY,
			key_id     text NOT NULL UNIQUE,
			key_hash   bytea NOT NULL,
			salt       bytea NOT NULL,
			created_at timestamptz NOT NULL DEFAULT now(),
			last_seen  timestamptz
		)`); err != nil {
		t.Fatalf("create legacy vantage_keys: %v", err)
	}
	created := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
	lastSeen := time.Now().Add(-1 * time.Hour).UTC().Truncate(time.Second)
	if _, err := pool.Exec(ctx,
		`INSERT INTO vantage_keys (name, key_id, key_hash, salt, created_at, last_seen) VALUES ($1,$2,$3,$4,$5,$6)`,
		"legacy-vantage", "deadbeef", []byte("hash"), []byte("salt"), created, lastSeen); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	// New() runs the schema, which must migrate the legacy table.
	s, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	infos, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var got *Info
	for i := range infos {
		if infos[i].Name == "legacy-vantage" {
			got = &infos[i]
		}
	}
	if got == nil {
		t.Fatalf("List after migration = %v, want it to contain legacy-vantage", infos)
	}
	if !got.Created.Equal(created) {
		t.Errorf("migrated created_at = %v, want %v", got.Created, created)
	}
	if !got.LastSeen.Equal(lastSeen) {
		t.Errorf("migrated last_seen = %v, want %v", got.LastSeen, lastSeen)
	}

	var regclass *string
	if err := pool.QueryRow(ctx, `SELECT to_regclass('vantage_keys')::text`).Scan(&regclass); err != nil {
		t.Fatalf("to_regclass query: %v", err)
	}
	if regclass != nil {
		t.Errorf("vantage_keys still exists after migration, to_regclass = %v", *regclass)
	}

	// Clean up the migrated row so it doesn't leak into other tests sharing "vantages".
	if _, err := s.Revoke(ctx, "legacy-vantage"); err != nil {
		t.Fatalf("cleanup Revoke: %v", err)
	}
}

// TestRegisterRejectsReservedName does not need a DB: Register validates the name (including
// the reserved-name check) before ever touching the pool, so a nil-pool Store is
// safe to call this on — a query against a nil pool would panic, so a call reaching
// the DB would fail loudly rather than silently registering the name.
func TestRegisterRejectsReservedName(t *testing.T) {
	s := &Store{}
	if err := s.Register(context.Background(), "local"); err == nil {
		t.Fatal("Register(\"local\") err = nil, want an error rejecting the reserved vantage")
	}
}

func TestRegisterRejectsInvalidName(t *testing.T) {
	s := &Store{}
	if err := s.Register(context.Background(), "bad name"); err == nil {
		t.Fatal("Register(\"bad name\") err = nil, want an error rejecting the invalid name")
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"nyc", "us-east", "us_east.1", "A9", "local"} {
		if !ValidName(ok) {
			t.Errorf("ValidName(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "-x", ".y", "a b", "a:b", "a\nb", "a/b", "a{b}"} {
		if ValidName(bad) {
			t.Errorf("ValidName(%q) = true, want false", bad)
		}
	}
}
