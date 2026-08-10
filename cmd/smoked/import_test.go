package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"smokeping-modern/internal/config"
	"smokeping-modern/internal/configstore"
	"smokeping-modern/internal/importer/smokeping"
	"smokeping-modern/internal/store/pgstore"
)

// renderFragmentYAML must marshal a tidy YAML fragment: target fields present,
// but none of the null-emitting Node fields (alerts/alertee/vantages) or a
// literal "null" anywhere in the output.
func TestRenderFragmentYAMLOmitsNulls(t *testing.T) {
	root := &config.Node{Children: map[string]*config.Node{
		"a": {Probe: "FPing", Host: "a.example"},
	}}
	y, err := renderFragmentYAML(root)
	if err != nil {
		t.Fatal(err)
	}
	s := string(y)
	if !strings.Contains(s, "host: a.example") || !strings.Contains(s, "probe: FPing") {
		t.Fatalf("missing target fields:\n%s", s)
	}
	for _, bad := range []string{"alerts:", "alertee:", "vantages:", "null"} {
		if strings.Contains(s, bad) {
			t.Errorf("output should omit %q:\n%s", bad, s)
		}
	}
}

// TestImportCmdWritesCleanYAML runs the CLI end-to-end against a small inline
// SmokePing config dir (no DB): default (no --apply) output should hold the
// expected FPing/DNS targets, skip the unmapped speedtest probe, and contain
// neither a literal "null" nor the word "speedtest".
func TestImportCmdWritesCleanYAML(t *testing.T) {
	dir := t.TempDir()
	targets := "*** Targets ***\nprobe = FPing\n" +
		"+ Local\nhost = localhost\n" +
		"+ Remote\nhost = example.com\nprobe = DNS\nlookup = example.com\n" +
		"+ Bad\nhost = slow.example\nprobe = speedtest\n"
	probes := "*** Probes ***\n+ FPing\nbinary = /usr/sbin/fping\n+ DNS\nbinary = /usr/bin/dig\n"
	database := "*** Database ***\nstep = 300\npings = 20\n"
	writeFile(t, filepath.Join(dir, "Targets"), targets)
	writeFile(t, filepath.Join(dir, "Probes"), probes)
	writeFile(t, filepath.Join(dir, "Database"), database)

	out := filepath.Join(t.TempDir(), "targets.yaml")
	code := importCmd([]string{"smokeping", dir, "--out", out})
	if code != 0 {
		t.Fatalf("importCmd exit code = %d, want 0", code)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "host: localhost") {
		t.Errorf("missing Local target:\n%s", s)
	}
	if !strings.Contains(s, "host: example.com") || !strings.Contains(s, "lookup: example.com") {
		t.Errorf("missing Remote DNS target:\n%s", s)
	}
	if strings.Contains(s, "speedtest") {
		t.Errorf("unmapped speedtest probe leaked into output:\n%s", s)
	}
	if strings.Contains(s, "null") {
		t.Errorf("output should never contain a literal null:\n%s", s)
	}
}

// TestImportCmdRejectsMissingDir covers the CLI-argument-error path.
func TestImportCmdRejectsMissingDir(t *testing.T) {
	if code := importCmd([]string{"notsmokeping"}); code != 2 {
		t.Errorf("importCmd with bad subcommand = %d, want 2", code)
	}
	if code := importCmd([]string{"smokeping"}); code != 2 {
		t.Errorf("importCmd with no dir = %d, want 2", code)
	}
	if code := importCmd([]string{"smokeping", "--out", "x"}); code != 2 {
		t.Errorf("importCmd with dir looking like a flag = %d, want 2", code)
	}
}

// TestImportCmdMissingTargetsFileErrors covers the read path: a dir with no
// Targets file (wrong dir, or an install directory the caller pointed at by
// mistake) must fail loudly with a non-zero exit and write nothing, not
// silently succeed with an empty `targets: {}` fragment.
func TestImportCmdMissingTargetsFileErrors(t *testing.T) {
	dir := t.TempDir() // empty: no Targets file (Probes/Database absent too)
	out := filepath.Join(t.TempDir(), "targets.yaml")
	code := importCmd([]string{"smokeping", dir, "--out", out})
	if code == 0 {
		t.Fatalf("importCmd against a dir with no Targets file = %d, want non-zero", code)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("importCmd should not have written %s when Targets is missing", out)
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// resetConfigFragmentRow clears the shared config_fragment row (id=1) so a DB-gated test
// starts from a known-empty DB config, mirroring the reset TestConfigImportCmd (main_test.go)
// performs before exercising configCmd against the same table.
func resetConfigFragmentRow(t *testing.T, dsn string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `DELETE FROM config_fragment WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
}

// applyFragment (behind `smoked import smokeping <dir> --apply`) merges via
// config.AppendImport, which is now deliberately context-free (Finding #6): it can't tell
// whether a leaf's probe/params/alerts resolve once inherited from the real base config. The
// optional --config DIR effective-validates the merged fragment against that base config
// (LoadPath + AppendDBFragment + Monitors(), the same composition buildRuntime performs)
// before persisting, so an operator gets an immediate reject instead of a silently-stored
// fragment the daemon only discovers is broken at its next reload.

// --config given, fragment relies on the base's tree-wide probe: accepted and persisted.
func TestApplyFragmentConfigFlagAcceptsInheritedFragment(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run applyFragment --config test")
	}
	ctx := context.Background()
	cs, err := configstore.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	resetConfigFragmentRow(t, dsn)
	if _, _, e := cs.Get(ctx); e != nil {
		t.Fatal(e)
	}

	baseDir := t.TempDir()
	writeFile(t, filepath.Join(baseDir, "default.yaml"), "targets:\n  probe: TCPConnect\n")

	root := &config.Node{Children: map[string]*config.Node{
		"af-inh": {Host: "127.0.0.1"}, // no probe: relies on the base's tree-wide probe
	}}
	if rc := applyFragment(dsn, root, baseDir); rc != 0 {
		t.Fatalf("applyFragment --config on an inheriting fragment: rc=%d, want 0", rc)
	}
	doc, ver, _ := cs.Get(ctx)
	if ver < 1 || !strings.Contains(string(doc), "af-inh") {
		t.Fatalf("inherited fragment not persisted: v%d %s", ver, doc)
	}
}

// --config given, fragment has no probe anywhere (base sets no tree-wide probe either):
// rejected, non-zero exit, nothing persisted.
func TestApplyFragmentConfigFlagRejectsInvalid(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run applyFragment --config test")
	}
	ctx := context.Background()
	cs, err := configstore.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	resetConfigFragmentRow(t, dsn)
	_, verBefore, e := cs.Get(ctx)
	if e != nil {
		t.Fatal(e)
	}

	baseDir := t.TempDir()
	writeFile(t, filepath.Join(baseDir, "default.yaml"), "targets:\n  children: {}\n")

	root := &config.Node{Children: map[string]*config.Node{
		"af-bad": {Host: "127.0.0.1"}, // no probe, nothing to inherit either
	}}
	rc := applyFragment(dsn, root, baseDir)
	if rc == 0 {
		t.Fatal("applyFragment --config should reject a genuinely-invalid fragment, got rc=0")
	}
	doc, verAfter, _ := cs.Get(ctx)
	if verAfter != verBefore || strings.Contains(string(doc), "af-bad") {
		t.Fatalf("rejected fragment must not persist: v%d -> v%d, doc=%s", verBefore, verAfter, doc)
	}
}

// --config omitted: only AppendImport's context-free checks run, so an inheriting fragment is
// accepted and persisted without a base config to check against; a note explains it's
// validated at the daemon's next reload instead.
func TestApplyFragmentWithoutConfigFlagAcceptsInheritedFragment(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run applyFragment test")
	}
	ctx := context.Background()
	cs, err := configstore.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	resetConfigFragmentRow(t, dsn)
	if _, _, e := cs.Get(ctx); e != nil {
		t.Fatal(e)
	}

	root := &config.Node{Children: map[string]*config.Node{
		"af-noconf": {Host: "127.0.0.1"},
	}}
	var rc int
	out := captureStdout(t, func() { rc = applyFragment(dsn, root, "") })
	if rc != 0 {
		t.Fatalf("applyFragment without --config should accept a context-free-valid fragment: rc=%d", rc)
	}
	if !strings.Contains(out, "next reload") {
		t.Fatalf("expected a note about daemon-reload validation, got:\n%s", out)
	}
	doc, ver, _ := cs.Get(ctx)
	if ver < 1 || !strings.Contains(string(doc), "af-noconf") {
		t.Fatalf("fragment not persisted: v%d %s", ver, doc)
	}
}

// TestResolveDataDirSibling covers the common linuxserver/SmokePing layout:
// config/ and data/ as siblings.
func TestResolveDataDirSibling(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	dataDir := filepath.Join(root, "data")
	mkdir(t, configDir)
	mkdir(t, dataDir)

	got, err := resolveDataDir(configDir, "")
	if err != nil {
		t.Fatal(err)
	}
	assertSamePath(t, got, dataDir)
}

// TestResolveDataDirSubdir covers a data/ directly under the config dir,
// with no sibling ../data present.
func TestResolveDataDirSubdir(t *testing.T) {
	configDir := t.TempDir()
	dataDir := filepath.Join(configDir, "data")
	mkdir(t, dataDir)

	got, err := resolveDataDir(configDir, "")
	if err != nil {
		t.Fatal(err)
	}
	assertSamePath(t, got, dataDir)
}

// TestResolveDataDirExplicit covers an explicit --data: trusted as given,
// with no existence check (a bad path surfaces its own clear error later,
// from Reconcile/ExtractRRD).
func TestResolveDataDirExplicit(t *testing.T) {
	got, err := resolveDataDir(t.TempDir(), "/does/not/exist")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/does/not/exist" {
		t.Errorf("resolveDataDir with explicit --data = %q, want %q", got, "/does/not/exist")
	}
}

// TestResolveDataDirMissing covers neither sibling nor subdir existing and no
// explicit --data: a clear, non-nil error rather than a silent bad guess.
func TestResolveDataDirMissing(t *testing.T) {
	configDir := t.TempDir()
	_, err := resolveDataDir(configDir, "")
	if err == nil {
		t.Fatal("resolveDataDir with no data dir anywhere should error, got nil")
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// assertSamePath compares two paths after Abs+Clean, since resolveDataDir's
// sibling candidate goes through filepath.Join(configDir, "..", "data")
// rather than a pre-cleaned path.
func assertSamePath(t *testing.T, got, want string) {
	t.Helper()
	ga, _ := filepath.Abs(got)
	wa, _ := filepath.Abs(want)
	if filepath.Clean(ga) != filepath.Clean(wa) {
		t.Errorf("path = %q, want %q", got, want)
	}
}

// TestValidTargetPingsRejectsUnresolvableAndOversizeCounts is the regression for
// Finding #5: a target whose Database file is missing/unreadable (and which has no
// probe/target-level pings override) resolves smokeping.Targets' default pings to 0 —
// validTargetPings must reject that (and any other non-positive or over-MaxPings value)
// with an error naming the target and the likely cause, while leaving a normally-resolved
// count (Database present, or an override) alone.
func TestValidTargetPingsRejectsUnresolvableAndOversizeCounts(t *testing.T) {
	cases := []struct {
		name    string
		pings   int
		wantErr bool
	}{
		{"zero (unresolvable — missing Database)", 0, true},
		{"negative", -1, true},
		{"valid minimum", 1, false},
		{"valid typical (Database default)", 20, false},
		{"at MaxPings", config.MaxPings, false},
		{"over MaxPings", config.MaxPings + 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validTargetPings(smokeping.ImportTarget{Name: "A/B", Pings: c.pings})
			if (err != nil) != c.wantErr {
				t.Errorf("validTargetPings(pings=%d) err = %v, wantErr %v", c.pings, err, c.wantErr)
			}
		})
	}
	// The zero case specifically must hint at the missing Database file — this is the
	// actual failure mode the finding describes, and a generic "invalid" message would
	// leave an operator guessing why a valid-looking target failed to import.
	err := validTargetPings(smokeping.ImportTarget{Name: "A/B", Pings: 0})
	if err == nil || !strings.Contains(err.Error(), "Database") {
		t.Errorf("pings=0 error should hint at the missing Database file, got: %v", err)
	}
	if err == nil || !strings.Contains(err.Error(), "A/B") {
		t.Errorf("error should name the target (A/B), got: %v", err)
	}
}

// TestValidLossSamplesDropsOutOfRangeLoss covers the chosen policy for a sample whose
// extracted loss can't be reconciled with the target's resolved pings (negative, or
// greater than pings): drop that sample (with the caller expected to warn), keep the
// rest — one bad round in years of RRD history shouldn't lose the rest of it, mirroring
// ExtractRRD's own per-row tolerance for a true gap.
func TestValidLossSamplesDropsOutOfRangeLoss(t *testing.T) {
	now := time.Now()
	samples := []smokeping.RRDSample{
		{TS: now, Loss: 0},
		{TS: now.Add(time.Minute), Loss: 5},      // == pings, boundary valid
		{TS: now.Add(2 * time.Minute), Loss: 6},  // > pings, drop
		{TS: now.Add(3 * time.Minute), Loss: -1}, // negative, drop
	}
	valid, dropped := validLossSamples(samples, 5)
	if dropped != 2 {
		t.Errorf("dropped = %d, want 2", dropped)
	}
	if len(valid) != 2 {
		t.Fatalf("len(valid) = %d, want 2: %+v", len(valid), valid)
	}
	for _, s := range valid {
		if s.Loss > 5 || s.Loss < 0 {
			t.Errorf("valid sample has out-of-range loss: %+v", s)
		}
	}
}

// TestValidLossSamplesNoneDroppedReturnsAllUnchanged is the regression case for --
// every sample has an in-range loss, so nothing is dropped and every sample is kept.
func TestValidLossSamplesNoneDroppedReturnsAllUnchanged(t *testing.T) {
	now := time.Now()
	samples := []smokeping.RRDSample{{TS: now, Loss: 0}, {TS: now.Add(time.Minute), Loss: 3}}
	valid, dropped := validLossSamples(samples, 20)
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0", dropped)
	}
	if len(valid) != 2 {
		t.Errorf("len(valid) = %d, want 2", len(valid))
	}
}

// captureStdout redirects os.Stdout for the duration of fn and returns
// whatever it wrote — used to assert on importCmd's --report/--history
// output without threading an io.Writer through importCmd's flag-parsing
// entry point.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	w.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// captureOutput redirects both os.Stdout and os.Stderr for the duration of fn and
// returns each captured separately — used where a single importCmd run needs asserting
// on both its stdout (the summary/per-target progress lines) and stderr (per-target
// warnings/errors, e.g. an unresolvable ping count).
func captureOutput(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	origOut, origErr := os.Stdout, os.Stderr
	ro, wo, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	re, we, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = wo, we
	defer func() { os.Stdout, os.Stderr = origOut, origErr }()

	fn()

	wo.Close()
	we.Close()
	var bufOut, bufErr bytes.Buffer
	if _, err := io.Copy(&bufOut, ro); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(&bufErr, re); err != nil {
		t.Fatal(err)
	}
	return bufOut.String(), bufErr.String()
}

// TestImportCmdReport builds a temp SmokePing config dir with a sibling data/
// tree (two targets with .rrd, one config-only, one orphan .rrd) and asserts
// --report prints the matched/config-only/orphan counts and exits 0, without
// writing anything (no --dsn needed).
func TestImportCmdReport(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	dataDir := filepath.Join(root, "data")
	mkdir(t, configDir)
	mkdir(t, dataDir)

	targets := "*** Targets ***\nprobe = FPing\n" +
		"+ Alpha\nhost = alpha.example\n" +
		"+ Beta\nhost = beta.example\n"
	writeFile(t, filepath.Join(configDir, "Targets"), targets)
	writeFile(t, filepath.Join(configDir, "Probes"), "*** Probes ***\n+ FPing\nbinary = /usr/sbin/fping\n")
	writeFile(t, filepath.Join(configDir, "Database"), "*** Database ***\nstep = 300\npings = 20\n")

	// Alpha has a matching .rrd, Beta does not (config-only); Orphan.rrd has
	// no matching target.
	writeFile(t, filepath.Join(dataDir, "Alpha.rrd"), "fake-rrd")
	writeFile(t, filepath.Join(dataDir, "Orphan.rrd"), "fake-rrd")

	var code int
	stdout := captureStdout(t, func() {
		code = importCmd([]string{"smokeping", configDir, "--report"})
	})
	if code != 0 {
		t.Fatalf("importCmd --report exit code = %d, want 0\nstdout:\n%s", code, stdout)
	}
	for _, want := range []string{"2 target", "1 matched", "1 config-only", "1 orphan"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("--report output missing %q:\n%s", want, stdout)
		}
	}
	if !strings.Contains(stdout, "Beta") {
		t.Errorf("--report output should name the config-only target Beta:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Orphan") {
		t.Errorf("--report output should name the orphan Orphan:\n%s", stdout)
	}
}

// TestImportCmdReportMissingDataDir covers the resolveDataDir failure path
// surfacing through --report as a clean non-zero exit, not a panic.
func TestImportCmdReportMissingDataDir(t *testing.T) {
	configDir := t.TempDir() // no sibling or subdir data/
	writeFile(t, filepath.Join(configDir, "Targets"), "*** Targets ***\nprobe = FPing\n+ Alpha\nhost = alpha.example\n")

	code := importCmd([]string{"smokeping", configDir, "--report"})
	if code == 0 {
		t.Fatal("importCmd --report with no resolvable data dir should be non-zero")
	}
}

// TestRefreshWindowForPadsNarrowRange is the regression for a bug the gated
// e2e test caught: RefreshAggregates errors "refresh window too small" from
// TimescaleDB unless the window covers at least two buckets of the aggregate
// being refreshed, and the binding constraint (samples_daily's 1-day bucket,
// since RefreshAggregates refreshes both hourly and daily over the same
// window) needs at least 2 days. A --history run whose extracted samples
// span under an hour (a short/sparse RRD, or this package's own e2e test
// fixture) must still get a wide-enough window, capped the same way
// RefreshAggregates itself caps `until` (now()-1h) so the guarantee survives
// that cap rather than being eaten by it.
func TestRefreshWindowForPadsNarrowRange(t *testing.T) {
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)

	// Narrow, safely-in-the-past range: until unchanged (not future/recent
	// enough to be capped), from pulled back to guarantee minRefreshSpan.
	minTS := now.Add(-2 * time.Hour)
	maxTS := now.Add(-1*time.Hour - 15*time.Minute)
	from, until := refreshWindowFor(minTS, maxTS, now)
	if !until.Equal(maxTS) {
		t.Errorf("until = %v, want unchanged maxTS %v", until, maxTS)
	}
	if !from.Equal(maxTS.Add(-minRefreshSpan)) {
		t.Errorf("from = %v, want maxTS-minRefreshSpan = %v", from, maxTS.Add(-minRefreshSpan))
	}
	if until.Sub(from) != minRefreshSpan {
		t.Errorf("window = %v, want exactly minRefreshSpan (%v)", until.Sub(from), minRefreshSpan)
	}

	// Already-wide range: returned unchanged (no need to pad, and never
	// narrowed either).
	wideMin := now.Add(-100 * time.Hour)
	wideMax := now.Add(-2 * time.Hour)
	gotFrom, gotUntil := refreshWindowFor(wideMin, wideMax, now)
	if !gotFrom.Equal(wideMin) || !gotUntil.Equal(wideMax) {
		t.Errorf("wide range: got (%v,%v), want unchanged (%v,%v)", gotFrom, gotUntil, wideMin, wideMax)
	}

	// Recent/near-"now" range: until gets capped at now-1h (mirroring
	// RefreshAggregates' own cap), and from is pulled back from THAT capped
	// value, not the raw (future-ish) maxTS — otherwise the guaranteed span
	// would be silently eaten by RefreshAggregates' own internal clamp.
	recentMin := now.Add(-30 * time.Minute)
	recentMax := now
	rFrom, rUntil := refreshWindowFor(recentMin, recentMax, now)
	wantUntil := now.Add(-time.Hour)
	if !rUntil.Equal(wantUntil) {
		t.Errorf("recent range: until = %v, want capped to now-1h = %v", rUntil, wantUntil)
	}
	if !rFrom.Equal(wantUntil.Add(-minRefreshSpan)) {
		t.Errorf("recent range: from = %v, want capped-until - minRefreshSpan = %v", rFrom, wantUntil.Add(-minRefreshSpan))
	}
}

// TestPrintHistorySummaryPartialRefreshWording is the regression for finding
// 2 of the Task-4 review: printHistorySummary used to print an unqualified
// "aggregate refresh: done" even when RefreshAggregates' own now()-1h cap
// left the newest imported samples outside the refreshed range (a still-live
// SmokePing source being imported mid-collection). Raw data is never lost
// either way, but the summary must say so rather than overclaim.
func TestPrintHistorySummaryPartialRefreshWording(t *testing.T) {
	var full bytes.Buffer
	printHistorySummary(&full, 3, 100, 0, 0, 0, true, true, false)
	if !strings.Contains(full.String(), "aggregate refresh: done") || strings.Contains(full.String(), "most recent") {
		t.Errorf("fully-covered refresh should report a plain \"done\", got:\n%s", full.String())
	}

	var partial bytes.Buffer
	printHistorySummary(&partial, 3, 100, 0, 0, 0, true, true, true)
	s := partial.String()
	if !strings.Contains(s, "most recent") || !strings.Contains(s, "background refresh policy") {
		t.Errorf("partially-covered refresh should note the trailing gap and the background policy, got:\n%s", s)
	}
}

// TestPrintHistorySummaryReportsFailed covers the new failed-target count (Finding
// #5): a run with at least one failed target must mention it in the summary, matching
// how config-only/orphan counts are already surfaced.
func TestPrintHistorySummaryReportsFailed(t *testing.T) {
	var buf bytes.Buffer
	printHistorySummary(&buf, 2, 50, 0, 0, 1, true, true, false)
	if !strings.Contains(buf.String(), "1 failed") {
		t.Errorf("summary should report the failed-target count, got:\n%s", buf.String())
	}
}

// mustRunRRDTool runs an rrdtool subcommand and fails the test with its
// combined output on error (local twin of internal/importer/smokeping's
// rrd_test.go mustRun — test helpers aren't shared across packages).
func mustRunRRDTool(t *testing.T, bin string, args ...string) {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", bin, args, err, out)
	}
}

// TestImportCmdHistoryE2E is the DB+rrdtool-gated end-to-end test for
// --history: build a 2-target synthetic SmokePing dir with real .rrd files
// (via rrdtool create+update), run importCmd with --history --dsn, assert
// exit 0 and that pgstore now has history for a target, and that a second
// run inserts 0 new rows (idempotent).
func TestImportCmdHistoryE2E(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("SMOKE_TEST_DSN not set")
	}
	rrdtoolBin, err := exec.LookPath("rrdtool")
	if err != nil {
		t.Skip("rrdtool not on PATH")
	}

	// This test's ImpHist* targets are unique per run (see suffix below), so they
	// never collide with a concurrent run — but nothing ever deletes the rows a
	// run inserts, so the shared samples table accretes them across every CI/local
	// run forever. Mirror the pgstore DB tests' self-isolation (delete-before, plus
	// a Cleanup in case a later assertion fails mid-test) so re-runs stay stable.
	// A bare pgxpool connection is used here (rather than pgstore.New) since the
	// cleanup only needs a raw DELETE and pgstore exposes no generic Exec.
	ctxCleanup := context.Background()
	deleteImpHistRows := func() {
		pool, err := pgxpool.New(ctxCleanup, dsn)
		if err != nil {
			t.Fatalf("cleanup: connect: %v", err)
		}
		defer pool.Close()
		if _, err := pool.Exec(ctxCleanup, "DELETE FROM samples WHERE target LIKE 'ImpHist%'"); err != nil {
			t.Fatalf("cleanup: delete ImpHist* rows: %v", err)
		}
	}
	deleteImpHistRows()
	t.Cleanup(deleteImpHistRows)

	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	dataDir := filepath.Join(root, "data")
	mkdir(t, configDir)
	mkdir(t, dataDir)

	// Unique target names per run so a re-run of the suite (or a previous
	// failed attempt) against the shared DB never collides with stale rows.
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	nameA := "ImpHistA" + suffix
	nameB := "ImpHistB" + suffix

	targetsBody := fmt.Sprintf("*** Targets ***\nprobe = FPing\n+ %s\nhost = a.example\n+ %s\nhost = b.example\n", nameA, nameB)
	writeFile(t, filepath.Join(configDir, "Targets"), targetsBody)
	writeFile(t, filepath.Join(configDir, "Probes"), "*** Probes ***\n+ FPing\nbinary = /usr/sbin/fping\n")
	writeFile(t, filepath.Join(configDir, "Database"), "*** Database ***\nstep = 300\npings = 20\n")

	start := time.Now().Add(-2 * time.Hour).Truncate(300 * time.Second).Unix()
	for _, name := range []string{nameA, nameB} {
		rrd := filepath.Join(dataDir, name+".rrd")
		mustRunRRDTool(t, rrdtoolBin, "create", rrd, "--start", fmt.Sprint(start-300), "--step", "300",
			"DS:median:GAUGE:600:0:180", "DS:loss:GAUGE:600:0:20", "RRA:AVERAGE:0.5:1:100")
		for i := 0; i < 10; i++ {
			ts := start + int64(i)*300
			mustRunRRDTool(t, rrdtoolBin, "update", rrd, fmt.Sprintf("%d:%f:%d", ts, 0.010+float64(i)*0.001, i%3))
		}
	}

	var code int
	first := captureStdout(t, func() {
		code = importCmd([]string{"smokeping", configDir, "--history", "--dsn", dsn, "--rrdtool", rrdtoolBin})
	})
	if code != 0 {
		t.Fatalf("importCmd --history exit code = %d, want 0\nstdout:\n%s", code, first)
	}

	ctx := context.Background()
	s, err := pgstore.New(ctx, dsn, 8, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	hist, err := s.HistoryVantage(ctx, nameA, "local")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) == 0 {
		t.Fatalf("HistoryVantage(%s) empty after --history import", nameA)
	}

	second := captureStdout(t, func() {
		code = importCmd([]string{"smokeping", configDir, "--history", "--dsn", dsn, "--rrdtool", rrdtoolBin})
	})
	if code != 0 {
		t.Fatalf("importCmd --history (2nd run) exit code = %d, want 0\nstdout:\n%s", code, second)
	}
	if !strings.Contains(second, "0 row") {
		t.Errorf("2nd --history run should report 0 rows inserted (idempotent), got:\n%s", second)
	}
}

// TestImportCmdHistoryPartialFailureMissingPings is the DB+rrdtool-gated regression for
// Finding #5: a target with no Database file and no probe/target pings override resolves
// smokeping.Targets' pings to 0 — before the fix, --history wrote that straight into
// pgstore.ImportRow{Pings: 0}, a semantically invalid row (raw LossFraction reads as a
// false 0%, aggregate loss goes NULL via NULLIF(pings,0)). Now that target must be
// reported as failed (naming it and hinting at the missing Database file), its rows must
// never be inserted, --history must still import the other, validly-resolved target, and
// the run must exit with exitPartialFailure rather than 0.
func TestImportCmdHistoryPartialFailureMissingPings(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("SMOKE_TEST_DSN not set")
	}
	rrdtoolBin, err := exec.LookPath("rrdtool")
	if err != nil {
		t.Skip("rrdtool not on PATH")
	}

	ctxCleanup := context.Background()
	deleteRows := func() {
		pool, err := pgxpool.New(ctxCleanup, dsn)
		if err != nil {
			t.Fatalf("cleanup: connect: %v", err)
		}
		defer pool.Close()
		if _, err := pool.Exec(ctxCleanup, "DELETE FROM samples WHERE target LIKE 'ImpFail%'"); err != nil {
			t.Fatalf("cleanup: delete ImpFail* rows: %v", err)
		}
	}
	deleteRows()
	t.Cleanup(deleteRows)

	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	dataDir := filepath.Join(root, "data")
	mkdir(t, configDir)
	mkdir(t, dataDir)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	good := "ImpFailGood" + suffix
	bad := "ImpFailBad" + suffix

	// good has an inline pings override (so it resolves fine with no Database file at
	// all); bad has no override anywhere, so with no Database file its pings resolves to
	// the unresolvable 0 — the case this fix must catch.
	targetsBody := fmt.Sprintf("*** Targets ***\nprobe = FPing\n+ %s\nhost = a.example\npings = 20\n+ %s\nhost = b.example\n", good, bad)
	writeFile(t, filepath.Join(configDir, "Targets"), targetsBody)
	writeFile(t, filepath.Join(configDir, "Probes"), "*** Probes ***\n+ FPing\nbinary = /usr/sbin/fping\n")
	// Deliberately no Database file.

	start := time.Now().Add(-2 * time.Hour).Truncate(300 * time.Second).Unix()
	for _, name := range []string{good, bad} {
		rrd := filepath.Join(dataDir, name+".rrd")
		mustRunRRDTool(t, rrdtoolBin, "create", rrd, "--start", fmt.Sprint(start-300), "--step", "300",
			"DS:median:GAUGE:600:0:180", "DS:loss:GAUGE:600:0:20", "RRA:AVERAGE:0.5:1:100")
		for i := 0; i < 5; i++ {
			ts := start + int64(i)*300
			mustRunRRDTool(t, rrdtoolBin, "update", rrd, fmt.Sprintf("%d:%f:%d", ts, 0.010+float64(i)*0.001, i%3))
		}
	}

	var code int
	stdout, stderr := captureOutput(t, func() {
		code = importCmd([]string{"smokeping", configDir, "--history", "--dsn", dsn, "--rrdtool", rrdtoolBin})
	})
	if code != exitPartialFailure {
		t.Fatalf("importCmd --history exit code = %d, want %d (partial failure)\nstdout:\n%s\nstderr:\n%s",
			code, exitPartialFailure, stdout, stderr)
	}
	if !strings.Contains(stderr, bad) {
		t.Errorf("stderr should name the failed target %s, got:\n%s", bad, stderr)
	}
	if !strings.Contains(stderr, "Database") {
		t.Errorf("stderr should hint at the missing Database file, got:\n%s", stderr)
	}
	if !strings.Contains(stdout, "1 failed") {
		t.Errorf("stdout summary should report 1 failed target, got:\n%s", stdout)
	}

	ctx := context.Background()
	s, err := pgstore.New(ctx, dsn, 8, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	histGood, err := s.HistoryVantage(ctx, good, "local")
	if err != nil {
		t.Fatal(err)
	}
	if len(histGood) == 0 {
		t.Fatalf("HistoryVantage(%s) empty — the valid target should still be imported", good)
	}
	histBad, err := s.HistoryVantage(ctx, bad, "local")
	if err != nil {
		t.Fatal(err)
	}
	if len(histBad) != 0 {
		t.Fatalf("HistoryVantage(%s) = %d row(s), want 0 — the unresolvable-pings target must not be imported", bad, len(histBad))
	}
}

// TestImportCmdHistoryFailsWithoutAggregates is the DB+rrdtool-gated regression for
// review Finding #3: when the hourly/daily continuous aggregates are absent (downsampling
// never enabled — production init doesn't create them; only `smoked -downsample` does),
// --history used to print a warning and import the raw rows anyway, then skip the
// aggregate refresh. That silently produced a DB where any imported history older than the
// eventual 30-day raw retention window would NEVER be materialized into samples_daily — an
// operator who later ran -downsample would see it get retention-deleted without ever having
// shown up in the long-range view. --history must instead refuse to import anything at all
// (non-zero exit, zero rows written, an actionable stderr message naming `-downsample`) so
// the operator is forced into the correct order: enable downsampling first, then import.
func TestImportCmdHistoryFailsWithoutAggregates(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("SMOKE_TEST_DSN not set")
	}
	rrdtoolBin, err := exec.LookPath("rrdtool")
	if err != nil {
		t.Skip("rrdtool not on PATH")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	// Drop the continuous aggregates to simulate a DB where downsampling has never been
	// enabled — mirroring the drop+recreate pattern pgstore's own migration tests already
	// use against this shared test DB (see TestMigrateAggregatesAddsMedianRounds). Restored
	// in Cleanup via EnableDownsampling (idempotent) so later tests — in this package and in
	// internal/store/pgstore, which several tests here rely on having aggregates present —
	// see them back in place.
	for _, view := range []string{"samples_daily", "samples_hourly"} {
		if _, err := pool.Exec(ctx, "DROP MATERIALIZED VIEW IF EXISTS "+view+" CASCADE"); err != nil {
			t.Fatalf("drop %s: %v", view, err)
		}
	}
	t.Cleanup(func() {
		cctx := context.Background()
		s, err := pgstore.New(cctx, dsn, 8, func(error) {})
		if err != nil {
			t.Errorf("cleanup: restore aggregates: connect: %v", err)
			return
		}
		defer s.Close()
		if err := s.EnableDownsampling(cctx); err != nil {
			t.Errorf("cleanup: restore aggregates: %v", err)
		}
	})

	var before int64
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM samples").Scan(&before); err != nil {
		t.Fatalf("count samples before: %v", err)
	}

	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	dataDir := filepath.Join(root, "data")
	mkdir(t, configDir)
	mkdir(t, dataDir)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	name := "ImpNoAgg" + suffix
	targetsBody := fmt.Sprintf("*** Targets ***\nprobe = FPing\n+ %s\nhost = a.example\n", name)
	writeFile(t, filepath.Join(configDir, "Targets"), targetsBody)
	writeFile(t, filepath.Join(configDir, "Probes"), "*** Probes ***\n+ FPing\nbinary = /usr/sbin/fping\n")
	writeFile(t, filepath.Join(configDir, "Database"), "*** Database ***\nstep = 300\npings = 20\n")

	start := time.Now().Add(-2 * time.Hour).Truncate(300 * time.Second).Unix()
	rrd := filepath.Join(dataDir, name+".rrd")
	mustRunRRDTool(t, rrdtoolBin, "create", rrd, "--start", fmt.Sprint(start-300), "--step", "300",
		"DS:median:GAUGE:600:0:180", "DS:loss:GAUGE:600:0:20", "RRA:AVERAGE:0.5:1:100")
	for i := 0; i < 5; i++ {
		ts := start + int64(i)*300
		mustRunRRDTool(t, rrdtoolBin, "update", rrd, fmt.Sprintf("%d:%f:%d", ts, 0.010+float64(i)*0.001, i%3))
	}

	var code int
	stdout, stderr := captureOutput(t, func() {
		code = importCmd([]string{"smokeping", configDir, "--history", "--dsn", dsn, "--rrdtool", rrdtoolBin})
	})
	if code == 0 {
		t.Fatalf("importCmd --history with no aggregates: exit = 0, want non-zero\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if !strings.Contains(stderr, "-downsample") {
		t.Errorf("stderr should point the operator at -downsample, got:\n%s", stderr)
	}
	if !strings.Contains(strings.ToLower(stderr), "no rows") {
		t.Errorf("stderr should state that no rows were imported, got:\n%s", stderr)
	}

	var after int64
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM samples").Scan(&after); err != nil {
		t.Fatalf("count samples after: %v", err)
	}
	if after != before {
		t.Errorf("samples count changed (before=%d, after=%d) — no rows should have been inserted", before, after)
	}
}

// TestImportCmdHistoryMaterializesOldHistoryIntoDailyAggregate is the DB+rrdtool-gated
// regression proving the supported path (aggregates present) actually fixes review Finding
// #3: history OLDER than the 30-day raw retention window must be reflected in samples_daily
// (the daily continuous aggregate) after --history, not just sit in the raw `samples` table
// where the eventual retention policy would delete it unmaterialized. This directly exercises
// runHistory's refreshWindowFor(minTS, maxTS, now) covering the FULL extracted range, however
// old, not just a trailing window.
func TestImportCmdHistoryMaterializesOldHistoryIntoDailyAggregate(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("SMOKE_TEST_DSN not set")
	}
	rrdtoolBin, err := exec.LookPath("rrdtool")
	if err != nil {
		t.Skip("rrdtool not on PATH")
	}

	ctx := context.Background()
	s, err := pgstore.New(ctx, dsn, 8, func(error) {})
	if err != nil {
		t.Fatalf("pgstore.New: %v", err)
	}
	defer s.Close()
	if err := s.EnableDownsampling(ctx); err != nil {
		t.Fatalf("EnableDownsampling: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	deleteRows := func() {
		if _, err := pool.Exec(ctx, "DELETE FROM samples WHERE target LIKE 'ImpOld%'"); err != nil {
			t.Fatalf("cleanup: delete ImpOld* rows: %v", err)
		}
	}
	deleteRows()
	t.Cleanup(deleteRows)

	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	dataDir := filepath.Join(root, "data")
	mkdir(t, configDir)
	mkdir(t, dataDir)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	name := "ImpOld" + suffix
	targetsBody := fmt.Sprintf("*** Targets ***\nprobe = FPing\n+ %s\nhost = old.example\n", name)
	writeFile(t, filepath.Join(configDir, "Targets"), targetsBody)
	writeFile(t, filepath.Join(configDir, "Probes"), "*** Probes ***\n+ FPing\nbinary = /usr/sbin/fping\n")
	writeFile(t, filepath.Join(configDir, "Database"), "*** Database ***\nstep = 300\npings = 20\n")

	// 40 days old — strictly older than the 30-day raw retention (pgstore.EnableDownsampling
	// installs `add_retention_policy('samples', INTERVAL '30 days', ...)`) — so this data
	// proves its point only if it shows up via samples_daily, not merely in raw `samples`.
	start := time.Now().Add(-40 * 24 * time.Hour).Truncate(300 * time.Second).Unix()
	rrd := filepath.Join(dataDir, name+".rrd")
	mustRunRRDTool(t, rrdtoolBin, "create", rrd, "--start", fmt.Sprint(start-300), "--step", "300",
		"DS:median:GAUGE:600:0:180", "DS:loss:GAUGE:600:0:20", "RRA:AVERAGE:0.5:1:100")
	for i := 0; i < 10; i++ {
		ts := start + int64(i)*300
		mustRunRRDTool(t, rrdtoolBin, "update", rrd, fmt.Sprintf("%d:%f:%d", ts, 0.010+float64(i)*0.001, i%3))
	}

	var code int
	out := captureStdout(t, func() {
		code = importCmd([]string{"smokeping", configDir, "--history", "--dsn", dsn, "--rrdtool", rrdtoolBin})
	})
	if code != 0 {
		t.Fatalf("importCmd --history exit code = %d, want 0\nstdout:\n%s", code, out)
	}

	var rounds int
	var bucketAge time.Duration
	row := pool.QueryRow(ctx,
		`SELECT rounds, now() - bucket FROM samples_daily WHERE target=$1 AND vantage='local' ORDER BY bucket LIMIT 1`, name)
	if err := row.Scan(&rounds, &bucketAge); err != nil {
		t.Fatalf("query samples_daily for %s: %v (old history was not materialized into the daily aggregate)", name, err)
	}
	if rounds == 0 {
		t.Errorf("samples_daily bucket for %s has rounds=0, want >0", name)
	}
	if bucketAge < 30*24*time.Hour {
		t.Errorf("samples_daily bucket for %s is only %v old, want >30 days (fixture data must actually be OLD)", name, bucketAge)
	}
}
