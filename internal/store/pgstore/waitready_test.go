package pgstore

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// TestWaitReadySucceedsAgainstRealDB confirms the real ping path returns nil promptly
// when the database is up. Skipped unless SMOKE_TEST_DSN is set (the db-test CI job sets
// it); mirrors the other pgstore integration tests' skip convention.
func TestWaitReadySucceedsAgainstRealDB(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run the TimescaleDB integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := WaitReady(ctx, dsn, func(e error) { t.Logf("retry: %v", e) }); err != nil {
		t.Fatalf("WaitReady against a live database should succeed, got %v", err)
	}
}

// TestRetryUntilReadyRetriesThenSucceeds: the probe is retried until it succeeds. This
// is the core of the first-start fix — the collector waits out a database that is still
// coming up instead of giving up on the first failed attempt.
func TestRetryUntilReadyRetriesThenSucceeds(t *testing.T) {
	var calls, retries int
	probe := func(context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("not ready")
		}
		return nil
	}
	err := retryUntilReady(context.Background(), probe, time.Millisecond, func(error) { retries++ })
	if err != nil {
		t.Fatalf("want nil once the probe succeeds, got %v", err)
	}
	if calls != 3 {
		t.Errorf("probe called %d times, want 3 (2 failures then success)", calls)
	}
	if retries != 2 {
		t.Errorf("onRetry called %d times, want 2 (once per failure)", retries)
	}
}

// TestRetryUntilReadyGivesUpWhenCtxDone: retrying is bounded — when the context expires
// before the probe ever succeeds, it returns an error (so a genuinely-misconfigured or
// permanently-down database still fails visibly instead of looping forever).
func TestRetryUntilReadyGivesUpWhenCtxDone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	var retries int
	err := retryUntilReady(ctx, func(context.Context) error { return errors.New("nope") },
		5*time.Millisecond, func(error) { retries++ })
	if err == nil {
		t.Fatal("want an error when the context expires before the probe is ready")
	}
	if retries == 0 {
		t.Error("onRetry should have been called at least once before giving up")
	}
}

// TestWaitReadyErrorsWhenUnreachable exercises the real ping path against a port where
// nothing listens: WaitReady must retry and then error out when the bounded context
// expires, rather than block forever.
func TestWaitReadyErrorsWhenUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	err := WaitReady(ctx, "postgres://smoke:smoke@127.0.0.1:1/smoke?sslmode=disable", nil)
	if err == nil {
		t.Fatal("want an error for an unreachable database")
	}
}
