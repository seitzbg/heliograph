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

	"smokeping-modern/internal/config"
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
