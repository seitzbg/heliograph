package vantage

import (
	"context"
	"os"
	"testing"
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
