package pgstore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// waitReadyBackoff is the gap between connection attempts in WaitReady. Short enough
// that the collector starts promptly once the database is up, long enough not to spin.
const waitReadyBackoff = time.Second

// pingAttemptTimeout bounds a single connection attempt so one that hangs (a database
// that accepts TCP but never completes the startup handshake) can't consume the whole
// wait budget before the next retry.
const pingAttemptTimeout = 5 * time.Second

// WaitReady blocks until the database at dsn accepts a connection, retrying with a short
// backoff, or until ctx is done (pass a context.WithTimeout to bound the wait). It exists
// so the collector waits out a database that is still starting — notably TimescaleDB's
// first-run init, where the container's socket-based healthcheck can report healthy before
// the TCP listener is up — instead of crashing on the first refused connection. onRetry,
// if non-nil, is called with each failed attempt's error so the caller can log progress.
func WaitReady(ctx context.Context, dsn string, onRetry func(error)) error {
	return retryUntilReady(ctx, func(c context.Context) error { return pingOnce(c, dsn) }, waitReadyBackoff, onRetry)
}

// retryUntilReady calls probe until it returns nil or ctx is done, waiting backoff between
// attempts and reporting each failure to onRetry. It is the pure retry loop behind
// WaitReady, separated so the retry behavior is testable without a database.
func retryUntilReady(ctx context.Context, probe func(context.Context) error, backoff time.Duration, onRetry func(error)) error {
	for {
		err := probe(ctx)
		if err == nil {
			return nil
		}
		if onRetry != nil {
			onRetry(err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("pgstore: database not ready: %w (last attempt: %v)", ctx.Err(), err)
		case <-time.After(backoff):
		}
	}
}

// pingOnce opens a pool and pings it under a bounded per-attempt timeout, then closes it.
// A fresh pool per attempt keeps the probe independent of any half-open state from an
// earlier failed attempt against a database that was still coming up.
func pingOnce(ctx context.Context, dsn string) error {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	pctx, cancel := context.WithTimeout(ctx, pingAttemptTimeout)
	defer cancel()
	return pool.Ping(pctx)
}
