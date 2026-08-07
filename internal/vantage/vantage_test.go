package vantage

import (
	"context"
	"os"
	"strings"
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
	if _, err := s.pool.Exec(context.Background(), "TRUNCATE vantage_keys"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return s
}

func TestAddVerifyListRevoke(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	key, err := s.Add(ctx, "nyc")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !strings.HasPrefix(key, "smk_") || len(strings.Split(key, "_")) != 3 {
		t.Fatalf("key format = %q, want smk_<id>_<secret>", key)
	}

	if name, ok, err := s.Verify(ctx, key); err != nil || !ok || name != "nyc" {
		t.Fatalf("Verify(valid) = (%q,%v,%v), want (nyc,true,nil)", name, ok, err)
	}
	if _, ok, _ := s.Verify(ctx, "garbage"); ok {
		t.Error("Verify(garbage) ok=true, want false")
	}
	if _, ok, _ := s.Verify(ctx, key+"x"); ok {
		t.Error("Verify(tampered secret) ok=true, want false")
	}

	// last_seen is set after a successful verify.
	infos, err := s.List(ctx)
	if err != nil || len(infos) != 1 || infos[0].Name != "nyc" {
		t.Fatalf("List = %v (err %v), want one nyc", infos, err)
	}
	if infos[0].LastSeen.IsZero() {
		t.Error("LastSeen is zero after a successful Verify")
	}

	// Re-Add rotates: the old key stops working, a new one works.
	key2, err := s.Add(ctx, "nyc")
	if err != nil {
		t.Fatalf("re-Add: %v", err)
	}
	if _, ok, _ := s.Verify(ctx, key); ok {
		t.Error("old key still verifies after rotation")
	}
	if _, ok, _ := s.Verify(ctx, key2); !ok {
		t.Error("rotated key does not verify")
	}

	if removed, err := s.Revoke(ctx, "nyc"); err != nil || !removed {
		t.Fatalf("Revoke = (%v,%v), want (true,nil)", removed, err)
	}
	if _, ok, _ := s.Verify(ctx, key2); ok {
		t.Error("revoked key still verifies")
	}
	if removed, _ := s.Revoke(ctx, "nyc"); removed {
		t.Error("second Revoke removed=true, want false")
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

func TestAgentSnippet(t *testing.T) {
	out := AgentSnippet("nyc", "smk_abc_def")
	if !strings.Contains(out, "nyc") || !strings.Contains(out, "smk_abc_def") {
		t.Errorf("AgentSnippet missing name or key:\n%s", out)
	}
}
