// Package pgstore is a TimescaleDB (PostgreSQL) implementation of store.Store.
// Each measurement round is one row in the `samples` hypertable, keeping the
// raw per-round sample array so smoke bands can be computed from the full
// distribution — the durable equivalent of SmokePing's RRD, minus the fixed
// schema (see codemap 07 §4).
package pgstore

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"smokeping-modern/internal/scheduler"
)

type PGStore struct {
	ctx     context.Context
	pool    *pgxpool.Pool
	histCap int
	onErr   func(error)
}

// New connects to the DSN, applies the schema, and returns a store. histCap
// bounds how many recent rounds History returns per target. onErr (optional)
// receives async write errors; defaults to ignoring them.
func New(ctx context.Context, dsn string, histCap int, onErr func(error)) (*PGStore, error) {
	if histCap <= 0 {
		histCap = 1024
	}
	if onErr == nil {
		onErr = func(error) {}
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgstore: connect: %w", err)
	}
	s := &PGStore{ctx: ctx, pool: pool, histCap: histCap, onErr: onErr}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

func (s *PGStore) Close() { s.pool.Close() }

const schema = `
CREATE EXTENSION IF NOT EXISTS timescaledb;
CREATE TABLE IF NOT EXISTS samples (
	ts             timestamptz        NOT NULL,
	target         text               NOT NULL,
	probe          text               NOT NULL,
	host           text               NOT NULL,
	vantage        text               NOT NULL DEFAULT 'master',
	pings          smallint           NOT NULL,
	loss           smallint           NOT NULL,
	median_seconds double precision,
	rtts_seconds   double precision[] NOT NULL,
	err            text,
	duration_ms    double precision
);
SELECT create_hypertable('samples', 'ts', if_not_exists => TRUE);
CREATE INDEX IF NOT EXISTS samples_target_ts ON samples (target, ts DESC);
`

func (s *PGStore) migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("pgstore: migrate: %w", err)
	}
	return nil
}

// nanToNil converts NaN -> nil so lost/median gaps are stored as SQL NULL.
func nanToNil(v float64) *float64 {
	if math.IsNaN(v) {
		return nil
	}
	return &v
}

func centeredToDB(c []float64) []*float64 {
	out := make([]*float64, len(c))
	for i, v := range c {
		out[i] = nanToNil(v)
	}
	return out
}

func dbToCentered(a []*float64) []float64 {
	out := make([]float64, len(a))
	for i, p := range a {
		if p == nil {
			out[i] = math.NaN()
		} else {
			out[i] = *p
		}
	}
	return out
}

func (s *PGStore) Add(outcomes []scheduler.Outcome) {
	if len(outcomes) == 0 {
		return
	}
	batch := &pgx.Batch{}
	for _, o := range outcomes {
		var errText *string
		if o.Err != nil {
			t := o.Err.Error()
			errText = &t
		}
		batch.Queue(
			`INSERT INTO samples
			   (ts, target, probe, host, vantage, pings, loss, median_seconds, rtts_seconds, err, duration_ms)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			o.When.UTC(), o.Target.Name, o.ProbeName, o.Target.Host, "master",
			o.Computed.Pings, o.Computed.Loss, nanToNil(o.Computed.Median),
			centeredToDB(o.Computed.Centered), errText,
			float64(o.Duration.Microseconds())/1000.0,
		)
	}
	br := s.pool.SendBatch(s.ctx, batch)
	defer br.Close()
	for range outcomes {
		if _, err := br.Exec(); err != nil {
			s.onErr(fmt.Errorf("pgstore: insert: %w", err))
			return
		}
	}
}

func (s *PGStore) Keys() []string {
	rows, err := s.pool.Query(s.ctx, `SELECT target FROM samples GROUP BY target ORDER BY min(ts)`)
	if err != nil {
		s.onErr(err)
		return nil
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			s.onErr(err)
			return keys
		}
		keys = append(keys, k)
	}
	return keys
}

func (s *PGStore) Latest(key string) (scheduler.Outcome, bool) {
	row := s.pool.QueryRow(s.ctx,
		`SELECT ts, target, probe, host, pings, loss, median_seconds, rtts_seconds, err
		   FROM samples WHERE target=$1 ORDER BY ts DESC LIMIT 1`, key)
	o, err := scanOutcome(row)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.onErr(err)
		}
		return scheduler.Outcome{}, false
	}
	return o, true
}

func (s *PGStore) History(key string) []scheduler.Outcome {
	rows, err := s.pool.Query(s.ctx,
		`SELECT ts, target, probe, host, pings, loss, median_seconds, rtts_seconds, err
		   FROM samples WHERE target=$1 ORDER BY ts DESC LIMIT $2`, key, s.histCap)
	if err != nil {
		s.onErr(err)
		return nil
	}
	defer rows.Close()
	var out []scheduler.Outcome
	for rows.Next() {
		o, err := scanOutcome(rows)
		if err != nil {
			s.onErr(err)
			return out
		}
		out = append(out, o)
	}
	// query is DESC; History returns oldest->newest to match MemStore.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

type scannable interface {
	Scan(dest ...any) error
}

func scanOutcome(row scannable) (scheduler.Outcome, error) {
	var (
		o        scheduler.Outcome
		median   *float64
		centered []*float64
		errText  *string
	)
	if err := row.Scan(
		&o.When, &o.Target.Name, &o.ProbeName, &o.Target.Host,
		&o.Computed.Pings, &o.Computed.Loss, &median, &centered, &errText,
	); err != nil {
		return o, err
	}
	o.Computed.Median = math.NaN()
	if median != nil {
		o.Computed.Median = *median
	}
	o.Computed.Centered = dbToCentered(centered)
	o.Computed.Sorted = sortedNonNaN(o.Computed.Centered)
	if errText != nil {
		o.Err = errors.New(*errText)
	}
	return o, nil
}

func sortedNonNaN(c []float64) []float64 {
	out := make([]float64, 0, len(c))
	for _, v := range c {
		if !math.IsNaN(v) {
			out = append(out, v)
		}
	}
	sort.Float64s(out)
	return out
}

var _ interface {
	Add([]scheduler.Outcome)
	Keys() []string
	Latest(string) (scheduler.Outcome, bool)
	History(string) []scheduler.Outcome
} = (*PGStore)(nil)
