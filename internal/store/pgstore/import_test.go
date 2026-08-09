package pgstore

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestImportSamplesIdempotentAndAggregates covers the Slice B backfill primitives:
// ImportSamples bulk-inserts historical rows idempotently (ON CONFLICT ... DO NOTHING),
// AggregatesExist reports whether the continuous aggregates are present, and
// RefreshAggregates materializes an arbitrary historical range into them (unlike
// backfillAggregates, which only covers the trailing policy windows).
func TestImportSamplesIdempotentAndAggregates(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("SMOKE_TEST_DSN not set")
	}
	ctx := context.Background()
	s, err := New(ctx, dsn, 8, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// base is deterministic within the hour, so a prior run of this same test (this
	// process or an earlier CI attempt against the shared DB) can leave rows at the
	// same (target,vantage,ts) — clear them first so the "fresh insert" assertion below
	// isn't at the mercy of what a previous run happened to leave behind.
	if _, err := s.pool.Exec(ctx, "DELETE FROM samples WHERE target = 'imp/A'"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableDownsampling(ctx); err != nil {
		t.Fatal(err) // caggs must exist
	}
	base := time.Now().Add(-40 * 24 * time.Hour).Truncate(time.Hour) // older than raw retention window
	rows := []ImportRow{
		{TS: base, Target: "imp/A", Probe: "FPing", Host: "10.0.0.1", Pings: 20, Loss: 0, MedianSeconds: 0.01},
		{TS: base.Add(time.Hour), Target: "imp/A", Probe: "FPing", Host: "10.0.0.1", Pings: 20, Loss: 2, MedianSeconds: 0.02},
	}
	n, err := s.ImportSamples(ctx, rows)
	if err != nil || n != 2 {
		t.Fatalf("insert: n=%d err=%v", n, err)
	}
	n2, _ := s.ImportSamples(ctx, rows) // idempotent
	if n2 != 0 {
		t.Fatalf("re-insert should add 0, got %d", n2)
	}
	ok, _ := s.AggregatesExist(ctx)
	if !ok {
		t.Fatal("aggregates should exist")
	}
	if err := s.RefreshAggregates(ctx, base.Add(-time.Hour), time.Now()); err != nil {
		t.Fatal(err)
	}
	// hourly cagg now has imp/A rows in the imported range
	pts, err := s.Rollup(ctx, "imp/A", "local", "1h", base.Add(-2*time.Hour), time.Now())
	if err != nil || len(pts) == 0 {
		t.Fatalf("rollup after refresh: pts=%d err=%v", len(pts), err)
	}
}
