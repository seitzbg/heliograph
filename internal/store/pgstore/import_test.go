package pgstore

import (
	"context"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/seitzbg/heliograph/internal/config"
)

// TestValidateImportRowRejectsInvalidPingsAndLoss covers ImportSamples' defense-in-depth
// backstop (Finding #5): the CLI importer (cmd/smoked's runHistory) is expected to have
// already filtered these out, but validateImportRow must independently refuse to let a
// semantically invalid row reach the INSERT — a pings<1 row would make raw LossFraction
// read as a false 0% and NULLIF(pings,0) blank out the aggregate loss (see the
// samples_hourly/samples_daily view definitions), a pings>MaxPings row risks overflowing
// the smallint column, and a loss<0 or loss>pings row is nonsensical on its face.
func TestValidateImportRowRejectsInvalidPingsAndLoss(t *testing.T) {
	base := ImportRow{TS: time.Now(), Target: "t", Probe: "FPing", Host: "h", Pings: 20, Loss: 2, MedianSeconds: 0.01}

	cases := []struct {
		name    string
		mutate  func(r ImportRow) ImportRow
		wantErr bool
	}{
		{"valid row unchanged", func(r ImportRow) ImportRow { return r }, false},
		{"pings zero", func(r ImportRow) ImportRow { r.Pings = 0; r.Loss = 0; return r }, true},
		{"pings negative", func(r ImportRow) ImportRow { r.Pings = -1; r.Loss = 0; return r }, true},
		{"pings over MaxPings", func(r ImportRow) ImportRow { r.Pings = config.MaxPings + 1; return r }, true},
		{"pings exactly MaxPings", func(r ImportRow) ImportRow { r.Pings = config.MaxPings; r.Loss = 0; return r }, false},
		{"loss negative", func(r ImportRow) ImportRow { r.Loss = -1; return r }, true},
		{"loss exceeds pings", func(r ImportRow) ImportRow { r.Pings = 5; r.Loss = 6; return r }, true},
		{"loss equals pings (boundary, valid)", func(r ImportRow) ImportRow { r.Pings = 5; r.Loss = 5; return r }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateImportRow(c.mutate(base))
			if (err != nil) != c.wantErr {
				t.Errorf("validateImportRow() err = %v, wantErr %v", err, c.wantErr)
			}
		})
	}
}

// TestImportSamplesRejectsInvalidRowBackstop is the DB-gated proof that ImportSamples
// itself enforces validateImportRow, not just the CLI: a batch containing one row with
// pings=0 (e.g. what a missing/unparsable Database file used to resolve to before the
// CLI-level fix) must be rejected wholesale — as a backstop that should never fire in
// practice, ImportSamples fails the whole call rather than silently skipping the bad row,
// so no rows from the batch land, valid or not.
func TestImportSamplesRejectsInvalidRowBackstop(t *testing.T) {
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
	const target = "imp/invalid-backstop"
	if _, err := s.pool.Exec(ctx, "DELETE FROM samples WHERE target = $1", target); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.pool.Exec(context.Background(), "DELETE FROM samples WHERE target = $1", target)
	})

	base := time.Now().Add(-42 * 24 * time.Hour).Truncate(time.Hour)
	rows := []ImportRow{
		{TS: base, Target: target, Probe: "FPing", Host: "10.0.0.1", Pings: 20, Loss: 0, MedianSeconds: 0.01},
		{TS: base.Add(time.Hour), Target: target, Probe: "FPing", Host: "10.0.0.1", Pings: 0, Loss: 0, MedianSeconds: 0.02}, // invalid: pings=0
	}
	n, err := s.ImportSamples(ctx, rows)
	if err == nil {
		t.Fatal("ImportSamples with an invalid (pings=0) row should error")
	}
	if !strings.Contains(err.Error(), "pings") {
		t.Errorf("error should mention pings, got: %v", err)
	}
	if n != 0 {
		t.Errorf("ImportSamples n = %d, want 0 (the valid row must not be inserted alongside the rejected batch)", n)
	}

	var count int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM samples WHERE target = $1", target).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("samples table has %d row(s) for %s, want 0 (nothing from the rejected batch should have been inserted)", count, target)
	}
}

// TestExecWithRetryNonLockErrorReturnsImmediately covers RefreshAggregates' retry
// wrapper (execWithRetry): a non-55P03 error (here, a syntax error) must return on
// the first attempt, not be mistaken for the "concurrent refresh" lock-contention
// case and burn through refreshRetryAttempts * refreshRetryDelay of sleeping.
// Simulating a live 55P03 (a real overlapping concurrent-refresh) isn't practical
// here — refreshAgg's own retry loop in pgstore_test.go already exercises that
// SQLSTATE end-to-end against a real background refresh policy — so this test
// only pins down the "any other error returns immediately" half of the contract.
func TestExecWithRetryNonLockErrorReturnsImmediately(t *testing.T) {
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

	start := time.Now()
	err = s.execWithRetry(ctx, "SELECT this is not valid sql")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("execWithRetry with invalid SQL should error")
	}
	// A retried 55P03 would sleep at least refreshRetryDelay between attempts;
	// a syntax error must come straight back well under that.
	if elapsed >= refreshRetryDelay {
		t.Errorf("execWithRetry took %v for a non-55P03 error, want well under the %v retry delay (it should not have retried)", elapsed, refreshRetryDelay)
	}
}

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

// TestImportSamplesNaNMedianStoresNull is the regression for the review finding:
// ImportSamples bound a total-loss round's NaN median straight into $7 without the
// nanToNil guard the live buildBatch path uses, so it stored as a literal NaN instead
// of SQL NULL. avg()/max() over a NaN input is NaN, so a single such row poisoned its
// whole hourly/daily aggregate bucket. Task 2's RRD extraction will produce exactly
// this shape (a total-loss round has no median), so it must be closed before that
// lands. This inserts one NaN-median row alongside two normal rows in the same hour
// bucket, and asserts (a) the raw column is NULL, not NaN, and (b) the bucket's
// aggregate median is a real number, not poisoned by the NaN row.
func TestImportSamplesNaNMedianStoresNull(t *testing.T) {
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
	const target = "imp/nan"
	// Same cleanup pattern as TestImportSamplesIdempotentAndAggregates: base is
	// deterministic within the hour, so clear this test's own target first so re-runs
	// against the shared DB don't see stale rows from a previous run.
	if _, err := s.pool.Exec(ctx, "DELETE FROM samples WHERE target = $1", target); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableDownsampling(ctx); err != nil {
		t.Fatal(err) // caggs must exist
	}
	base := time.Now().Add(-41 * 24 * time.Hour).Truncate(time.Hour) // older than raw retention window
	nanTS := base.Add(20 * time.Minute)
	rows := []ImportRow{
		{TS: base, Target: target, Probe: "FPing", Host: "10.0.0.1", Pings: 20, Loss: 0, MedianSeconds: 0.01},
		{TS: base.Add(10 * time.Minute), Target: target, Probe: "FPing", Host: "10.0.0.1", Pings: 20, Loss: 0, MedianSeconds: 0.02},
		// Total-loss round: RRD extraction (and a real total-loss round) has no median.
		{TS: nanTS, Target: target, Probe: "FPing", Host: "10.0.0.1", Pings: 20, Loss: 20, MedianSeconds: math.NaN()},
	}
	n, err := s.ImportSamples(ctx, rows)
	if err != nil || n != 3 {
		t.Fatalf("insert: n=%d err=%v", n, err)
	}

	// (a) the raw column must be SQL NULL, not a literal NaN.
	var raw *float64
	if err := s.pool.QueryRow(ctx,
		`SELECT median_seconds FROM samples WHERE target=$1 AND vantage='local' AND ts=$2`,
		target, nanTS.UTC()).Scan(&raw); err != nil {
		t.Fatalf("query raw median_seconds: %v", err)
	}
	if raw != nil {
		t.Fatalf("raw median_seconds = %v, want NULL (NaN must not be stored as a literal value)", *raw)
	}

	if err := s.RefreshAggregates(ctx, base.Add(-time.Hour), time.Now()); err != nil {
		t.Fatal(err)
	}

	// (b) the hourly bucket containing all three rows must report a real median_avg —
	// a stored NaN would poison avg()/max() for the whole bucket.
	pts, err := s.Rollup(ctx, target, "local", "1h", base.Add(-2*time.Hour), time.Now())
	if err != nil || len(pts) == 0 {
		t.Fatalf("rollup after refresh: pts=%d err=%v", len(pts), err)
	}
	p := pts[0]
	if p.Rounds != 3 {
		t.Errorf("Rounds = %d, want 3 (all imported rows)", p.Rounds)
	}
	if p.MedianRounds != 2 {
		t.Errorf("MedianRounds = %d, want 2 (the NaN/total-loss round excluded)", p.MedianRounds)
	}
	if math.IsNaN(p.MedianAvg) {
		t.Fatal("bucket MedianAvg is NaN — the NaN-median row poisoned the aggregate")
	}
	if math.Abs(p.MedianAvg-0.015) > 1e-9 {
		t.Errorf("bucket MedianAvg = %v, want ~0.015 (avg of the two real medians, NaN row excluded)", p.MedianAvg)
	}
}
