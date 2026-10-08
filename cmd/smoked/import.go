// smoked import smokeping <dir> reads a legacy SmokePing config directory
// (Targets, Probes, Database) and turns it into a modern target-tree
// fragment: YAML to stdout/--out by default for review, or merged straight
// into the DB config fragment with --apply (slice A). --report reconciles
// the config against the RRD data directory (dry run, no writes) and
// --history backfills each matched target's RRD history into the DB's
// samples/aggregates (slice B).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/seitzbg/heliograph/internal/config"
	"github.com/seitzbg/heliograph/internal/configstore"
	"github.com/seitzbg/heliograph/internal/importer/smokeping"
	"github.com/seitzbg/heliograph/internal/store/pgstore"
)

// renderFragmentYAML marshals {targets: root} to tidy YAML: JSON first (Node's json
// omitempty tags drop empty scalars/maps), strip the null keys that alerts/alertee/
// vantages emit (those fields deliberately have no omitempty — see config.Node), then
// YAML. This keeps the reviewable output free of `alerts: null`-style noise.
func renderFragmentYAML(root *config.Node) ([]byte, error) {
	b, err := renderFragmentJSON(root)
	if err != nil {
		return nil, err
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	stripNulls(v)
	return yaml.Marshal(v)
}

// renderFragmentJSON is what --apply feeds to config.AppendImport (nulls are fine
// there — AppendImport's decoder treats a null alerts/alertee/vantages the same as
// absent, and JSON is valid input to its YAML-based decoder).
func renderFragmentJSON(root *config.Node) ([]byte, error) {
	return json.Marshal(struct {
		Targets *config.Node `json:"targets,omitempty"`
	}{Targets: root})
}

// stripNulls recursively deletes map keys whose value is JSON null, in place.
// Only object values are ever null-valued for a Node fragment (alerts/alertee/
// vantages: null), so this need not handle nulls inside arrays.
func stripNulls(v any) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	for k, val := range m {
		if val == nil {
			delete(m, k)
			continue
		}
		stripNulls(val)
	}
}

// importCmd implements `smoked import smokeping <dir> [--out FILE] [--apply] [--dsn DSN]
// [--config PATH] [--report] [--history] [--rrdtool PATH] [--data DIR]`.
func importCmd(args []string) int {
	const usage = "usage: smoked import smokeping <dir> [--out FILE] [--apply] [--dsn DSN] [--config PATH] " +
		"[--report] [--history] [--rrdtool PATH] [--data DIR]"
	if len(args) < 1 || args[0] != "smokeping" {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	rest := args[1:]
	if len(rest) == 0 || rest[0] == "" || rest[0][0] == '-' {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	dir := rest[0]
	fs := flag.NewFlagSet("import smokeping", flag.ExitOnError)
	out := fs.String("out", "", "write config YAML to this file (default: stdout)")
	apply := fs.Bool("apply", false, "also merge into the DB config fragment (needs --dsn)")
	dsn := fs.String("dsn", os.Getenv("SMOKED_DSN"), "TimescaleDB DSN (or set SMOKED_DSN)")
	configDir := fs.String("config", "", "the hub's YAML config (file, or dir with default.yaml + conf.d). With --apply, "+
		"effective-validate the merged fragment against it before persisting, instead of relying on the daemon's "+
		"next reload to catch a leaf whose inherited probe/params/alerts don't resolve. With --history, also look "+
		"up targets defined there (alongside the DB config) to find the id their history is stored under")
	report := fs.Bool("report", false, "reconcile config targets against the RRD data dir and print counts (dry run, no writes)")
	history := fs.Bool("history", false, "backfill each matched target's RRD history into samples/aggregates (needs --dsn/SMOKED_DSN and rrdtool)")
	rrdtoolFlag := fs.String("rrdtool", "", "path to the rrdtool binary (default: PATH lookup)")
	dataFlag := fs.String("data", "", "RRD data dir (default: sibling ../data, or ./data, under <dir>)")
	// flag.ExitOnError means Parse never returns a non-nil error (it calls
	// os.Exit(2) itself on a bad flag), so there's no error path to check here.
	fs.Parse(rest[1:])

	// --report/--history are alternate, read-mostly/write-only modes: neither
	// touches the YAML-fragment/--apply path below. If both are given,
	// --history (which itself reconciles and reports the same counts as part
	// of its summary) wins.
	if *history {
		return runHistory(dir, *dataFlag, *rrdtoolFlag, *dsn, *configDir)
	}
	if *report {
		return runReport(dir, *dataFlag)
	}

	// Targets is required: a wrong dir or an incomplete/unreadable checkout
	// must fail loudly rather than silently producing an empty `targets: {}`
	// fragment. Probes and Database are advisory-only param/step sources —
	// missing or unreadable is fine, read tolerantly as "".
	targetsBytes, err := os.ReadFile(filepath.Join(dir, "Targets"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "import: reading Targets: %v\n", err)
		return 1
	}
	readOptional := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		return string(b)
	}
	root, sum, err := smokeping.Parse(string(targetsBytes), readOptional("Probes"), readOptional("Database"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "import: %v\n", err)
		return 1
	}

	yamlBytes, err := renderFragmentYAML(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "import: %v\n", err)
		return 1
	}
	if *out == "" {
		os.Stdout.Write(yamlBytes)
	} else if err := os.WriteFile(*out, yamlBytes, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "import: %v\n", err)
		return 1
	}

	printImportSummary(os.Stderr, sum)

	if *apply {
		if *dsn == "" {
			fmt.Fprintln(os.Stderr, "import: --apply requires --dsn (or SMOKED_DSN)")
			return 2
		}
		if code := applyFragment(*dsn, root, *configDir); code != 0 {
			return code
		}
	}
	return 0
}

// applyFragment merges root's targets into the DB config fragment, mirroring configCmd's
// `config import` flow: read the current fragment, append (idempotent — an unchanged
// re-import reports 0 added), write it back under optimistic concurrency. config.AppendImport
// only performs context-free validation (Finding #6) — a leaf relying on an inherited
// probe/params/alerts can't be judged in isolation — so when configDir is non-empty, the
// merged fragment is additionally effective-validated against that base config (the same
// composition buildRuntime performs) before it's persisted; a genuinely-invalid fragment is
// rejected here instead of only being caught (and logged) at the daemon's next reload.
func applyFragment(dsn string, root *config.Node, configDir string) int {
	importBytes, err := renderFragmentJSON(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "import: %v\n", err)
		return 1
	}
	ctx := context.Background()
	cs, err := configstore.New(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "import: %v\n", err)
		return 1
	}
	defer cs.Close()
	doc, version, err := cs.Get(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "import: %v\n", err)
		return 1
	}
	merged, added, unchanged, err := config.AppendImport(doc, importBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "import: %v\n", err)
		return 1
	}
	if added == 0 {
		fmt.Printf("nothing new to merge (%d top-level branch(es) unchanged)\n", unchanged)
		return 0
	}
	// Mint UUIDs for the imported (id-less) host nodes before persisting, the same as an ordinary
	// admin apply — otherwise they'd store under their display path and the next apply would re-key
	// them to a fresh UUID, orphaning the history collected in between (CODE_REVIEW M8).
	merged, _ = configstore.MintNewIDs(merged)
	if configDir != "" {
		if err := effectiveValidate(configDir, merged); err != nil {
			fmt.Fprintf(os.Stderr, "import: effective validation against %s failed: %v\n", configDir, err)
			return 1
		}
	} else {
		fmt.Println("note: fragment not checked against a running config (--config not given); it's instead")
		fmt.Println("      validated when the config is next built: a running daemon's next reload (SIGHUP)")
		fmt.Println("      rejects and logs an invalid fragment, but a COLD START fails to boot on one")
		fmt.Println("      instead — prefer --config to validate up front.")
	}
	if err := cs.Set(ctx, merged, version); err != nil {
		if errors.Is(err, configstore.ErrConflict) {
			fmt.Fprintf(os.Stderr, "import: %v (re-run to retry)\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "import: %v\n", err)
		}
		return 1
	}
	fmt.Printf("merged %d top-level branch(es) → database config v%d (%d unchanged)\n", added, version+1, unchanged)
	return 0
}

// printImportSummary writes a human-readable report of what Parse produced: target/folder
// counts, the per-modern-probe breakdown, anything skipped (with its reason), any SmokePing
// params that had no home in the modern probe's param set, and the Database file's advisory
// step/pings (never written into the tree — see smokeping.Parse — so call this out
// explicitly for the operator to set by hand).
func printImportSummary(w io.Writer, sum smokeping.Summary) {
	fmt.Fprintf(w, "smokeping import: %d targets, %d folders\n", sum.Targets, sum.Folders)
	for _, probe := range sortedKeys(sum.ByProbe) {
		fmt.Fprintf(w, "  %s: %d\n", probe, sum.ByProbe[probe])
	}
	if len(sum.Skipped) > 0 {
		fmt.Fprintf(w, "skipped %d target(s):\n", len(sum.Skipped))
		for _, sk := range sum.Skipped {
			fmt.Fprintf(w, "  %s (probe=%s): %s\n", sk.Path, sk.Probe, sk.Reason)
		}
	}
	if len(sum.DroppedParams) > 0 {
		fmt.Fprintf(w, "dropped params (no modern equivalent): %v\n", sum.DroppedParams)
	}
	if sum.Step != "" || sum.Pings != 0 {
		fmt.Fprintf(w, "note: SmokePing step=%s pings=%d — set these in default.yaml\n", sum.Step, sum.Pings)
	}
}

// sortedKeys returns m's keys in ascending order, for stable summary output.
func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// resolveDataDir locates the RRD data directory for a SmokePing config dir.
// The linuxserver/SmokePing layout (and the real fixture) puts config/ and
// data/ as SIBLINGS — data/ is NOT generally under the config dir — so an
// explicit --data is trusted as given (no existence check here; a bad path
// surfaces its own clear error from Reconcile/ExtractRRD downstream), and
// absent that, the sibling <configDir>/../data (the common case) is tried
// before the subdirectory <configDir>/data. Neither existing is a clear,
// named error rather than a silent bad guess.
func resolveDataDir(configDir, dataFlag string) (string, error) {
	if dataFlag != "" {
		return dataFlag, nil
	}
	sibling := filepath.Join(configDir, "..", "data")
	subdir := filepath.Join(configDir, "data")
	for _, cand := range []string{sibling, subdir} {
		if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
			return cand, nil
		}
	}
	return "", fmt.Errorf("could not find the RRD data dir (tried %s, %s); pass --data", sibling, subdir)
}

// loadImportTargets reads a SmokePing config dir's Targets/Probes/Database
// files (the same tolerant-read shape as importCmd's YAML path: Targets is
// required, Probes/Database are advisory and read as "" if absent/unreadable)
// and flattens them via smokeping.Targets into the leaf list --report and
// --history both reconcile against the RRD data dir.
func loadImportTargets(dir string) ([]smokeping.ImportTarget, smokeping.Summary, error) {
	targetsBytes, err := os.ReadFile(filepath.Join(dir, "Targets"))
	if err != nil {
		return nil, smokeping.Summary{}, fmt.Errorf("reading Targets: %w", err)
	}
	readOptional := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		return string(b)
	}
	return smokeping.Targets(string(targetsBytes), readOptional("Probes"), readOptional("Database"))
}

// runReport implements --report: a dry-run reconciliation of the config
// against the RRD data dir. It never opens a DB connection and never writes
// anything — it only needs the config dir (and, for the data dir, --data or
// a resolvable sibling/subdir).
func runReport(dir, dataFlag string) int {
	targets, _, err := loadImportTargets(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "import: %v\n", err)
		return 1
	}
	dataDir, err := resolveDataDir(dir, dataFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "import: %v\n", err)
		return 1
	}
	rec, err := smokeping.Reconcile(targets, dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "import: %v\n", err)
		return 1
	}
	printReconciliationSummary(os.Stdout, len(targets), rec)
	return 0
}

// printReconciliationSummary reports config-vs-RRD drift: config is always
// the source of truth (Reconcile never invents a target from an orphan
// .rrd), so a config-only target is one --history will skip for lack of
// history to backfill, and an orphan .rrd is one --history will never touch.
// Orphans are capped in the listing (there can be many after a config
// rename/prune) with a "N more" tail rather than flooding the terminal.
func printReconciliationSummary(w io.Writer, total int, rec smokeping.Reconciliation) {
	fmt.Fprintf(w, "smokeping report: %d target(s), %d matched (have .rrd), %d config-only, %d orphan(s)\n",
		total, len(rec.Matched), len(rec.ConfigOnly), len(rec.Orphans))
	if len(rec.ConfigOnly) > 0 {
		fmt.Fprintln(w, "config-only (no .rrd found under the data dir — --history will skip these):")
		for _, t := range rec.ConfigOnly {
			fmt.Fprintf(w, "  %s\n", t.Name)
		}
	}
	if len(rec.Orphans) > 0 {
		fmt.Fprintln(w, "orphans (.rrd with no matching target — never imported):")
		const limit = 10
		n := len(rec.Orphans)
		if n > limit {
			n = limit
		}
		for _, name := range rec.Orphans[:n] {
			fmt.Fprintf(w, "  %s\n", name)
		}
		if len(rec.Orphans) > n {
			fmt.Fprintf(w, "  ... and %d more\n", len(rec.Orphans)-n)
		}
	}
}

// exitPartialFailure is runHistory's exit code when the run completed but at least one
// matched target could not be safely imported (an unresolvable ping count, an unreadable
// RRD, or no single configured target to store its history under) — as opposed to 0
// (every matched target imported cleanly) or 1 (a hard error, such as a DB insert
// failure, that aborts the whole run outright). The other targets in a partial-failure
// run were still imported; this distinct code exists so a script driving the importer can
// tell "some targets need attention" apart from both a clean run and a total failure.
const exitPartialFailure = 3

// dbConfigLabel names the DB config fragment as a destination source in --history output.
const dbConfigLabel = "the database config"

// historyDest is one configured target a SmokePing target's history could be written to:
// the storage id the hub keys its samples by, and the config source that defines it.
type historyDest struct {
	id, source string
}

// historyDestinations maps each SmokePing target to the configured target the hub will
// show its history under. The hub composes its targets from the YAML config (-config) plus
// the DB config fragment and keys every target's samples by its stable id (config.Node.ID,
// or its flattened path when it has none). `import smokeping --apply` merges the SmokePing
// tree into the DB fragment verbatim (top-level sections become top-level branches, nested
// sections become children) and mints a UUID onto every imported host, so a SmokePing
// target named "A/B" (smokeping.ImportTarget.Name) is the configured target whose flattened
// path is "A/B", and its history belongs under that target's id, not under "A/B".
type historyDestinations struct {
	byPath    map[string][]historyDest
	consulted []string // every config source looked in, for messages
	haveFile  bool     // a --config YAML config was among them
}

// newHistoryDestinations indexes the targets defined by fileCfg (the --config YAML, nil when
// not given; fileLabel names it in messages) and by the DB config fragment dbDoc, by
// flattened path. The two sources are indexed separately rather than composed: a path
// defined in both (a top-level branch the hub would refuse to load twice) or twice within one
// (a node key containing "/") then shows up as ambiguous for just the targets under it.
//
// It returns nil when there is no --config and the DB fragment defines no targets: with no
// config to consult, the caller falls back to keying history by SmokePing path, which is the
// storage id of a YAML target that has no explicit id.
func newHistoryDestinations(fileCfg *config.Config, fileLabel string, dbDoc []byte) (*historyDestinations, error) {
	d := &historyDestinations{byPath: map[string][]historyDest{}}
	add := func(label string, ids []config.TargetIdentity) {
		d.consulted = append(d.consulted, label)
		for _, ti := range ids {
			d.byPath[ti.Path] = append(d.byPath[ti.Path], historyDest{id: ti.ID, source: label})
		}
	}
	if fileCfg != nil {
		d.haveFile = true
		add(fileLabel, fileCfg.TargetIdentities())
	}
	dbCfg := &config.Config{}
	if err := config.AppendDBFragment(dbCfg, dbDoc); err != nil {
		return nil, err
	}
	add(dbConfigLabel, dbCfg.TargetIdentities())
	if !d.haveFile && len(d.byPath) == 0 {
		return nil, nil
	}
	return d, nil
}

// resolve returns the storage id for the SmokePing target named name, or an error saying why
// there is no single configured target to store its history under.
func (d *historyDestinations) resolve(name string) (string, error) {
	dests := d.byPath[name]
	switch len(dests) {
	case 1:
		return dests[0].id, nil
	case 0:
		hint := "was it renamed or moved after the config import?"
		if !d.haveFile {
			hint += " If it is defined in a YAML config, pass that config with --config."
		}
		return "", fmt.Errorf("no configured target at path %q in %s, so no graph would show its history (%s)",
			name, strings.Join(d.consulted, " or "), hint)
	default:
		where := make([]string, len(dests))
		for i, dst := range dests {
			where[i] = fmt.Sprintf("id %q in %s", dst.id, dst.source)
		}
		return "", fmt.Errorf("path %q matches %d configured targets (%s); the hub cannot load a config that "+
			"defines a target twice, so fix the config and re-run", name, len(dests), strings.Join(where, ", "))
	}
}

// loadHistoryDestinations reads the --history destination config: the YAML config at
// configPath (when given) and the DB config fragment. A nil result means neither defines any
// target (see newHistoryDestinations).
func loadHistoryDestinations(ctx context.Context, dsn, configPath string) (*historyDestinations, error) {
	var fileCfg *config.Config
	if configPath != "" {
		var err error
		if fileCfg, err = config.LoadPath(configPath); err != nil {
			return nil, fmt.Errorf("loading --config %s: %w", configPath, err)
		}
	}
	cs, err := configstore.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	defer cs.Close()
	doc, _, err := cs.Get(ctx)
	if err != nil {
		return nil, err
	}
	return newHistoryDestinations(fileCfg, "the YAML config "+configPath, doc)
}

// validTargetPings reports whether t's resolved ping count is safe to import: it must be
// strictly positive and no more than config.MaxPings. A missing/unreadable Database file
// with no probe/target-level override resolves smokeping.Targets' default to 0 (Finding
// #5) — writing that straight into pgstore.ImportRow{Pings: 0} makes raw LossFraction
// read as a false 0% and NULLIF(pings,0) blank out the aggregate loss, so it must be
// caught here instead. A genuinely-optional missing Probes file is NOT an error by
// itself — only pings failing to resolve to >=1 by any precedence path is.
func validTargetPings(t smokeping.ImportTarget) error {
	switch {
	case t.Pings < 1:
		return fmt.Errorf("%s: could not resolve a valid ping count (got %d); is the Database file present with a `pings` setting, or a probe/target pings override?", t.Name, t.Pings)
	case t.Pings > config.MaxPings:
		return fmt.Errorf("%s: resolved ping count %d exceeds the maximum of %d", t.Name, t.Pings, config.MaxPings)
	}
	return nil
}

// validLossSamples partitions samples into those whose Loss is consistent with pings
// (0..pings inclusive) and the count of those dropped for falling outside that range.
// Policy: drop the offending sample rather than failing the whole target — an RRD can
// hold years of history, and one bad round (a consolidation artifact, corruption, ...)
// shouldn't cost the rest of it, mirroring ExtractRRD's own per-row tolerance for a true
// gap. The caller is expected to warn when dropped > 0.
func validLossSamples(samples []smokeping.RRDSample, pings int) ([]smokeping.RRDSample, int) {
	out := make([]smokeping.RRDSample, 0, len(samples))
	dropped := 0
	for _, s := range samples {
		if s.Loss < 0 || s.Loss > pings {
			dropped++
			continue
		}
		out = append(out, s)
	}
	return out, dropped
}

// runHistory implements --history: resolve rrdtool and the data dir,
// reconcile config against it, then for each matched target extract its RRD
// history and backfill it into the DB's samples table (and, once done, the
// hourly/daily continuous aggregates over the imported range). Each target's
// rows are stored under the id of the configured target the hub shows it as,
// looked up in the YAML config at configPath (when given) and the DB config
// fragment (see historyDestinations); with neither defining any target, rows
// are keyed by SmokePing path, as before ids existed. A target with no single
// configured destination, whose resolved ping count doesn't validate
// (validTargetPings), or whose extract fails is logged and skipped (its RRD
// may be corrupt, missing an expected DS, etc. — no reason to abort a whole
// backfill over one bad target), and the run exits exitPartialFailure; a DB
// insert failure still aborts the whole run, since a partial/uncertain write
// state is worse than stopping.
func runHistory(dir, dataFlag, rrdtoolFlag, dsn, configPath string) int {
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "import: --history requires --dsn (or SMOKED_DSN)")
		return 2
	}
	rrdtoolBin := rrdtoolFlag
	if rrdtoolBin == "" {
		var err error
		rrdtoolBin, err = exec.LookPath("rrdtool")
		if err != nil {
			fmt.Fprintln(os.Stderr, "import: rrdtool not found on PATH; pass --rrdtool <path>")
			return 2
		}
	}

	targets, _, err := loadImportTargets(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "import: %v\n", err)
		return 1
	}
	dataDir, err := resolveDataDir(dir, dataFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "import: %v\n", err)
		return 1
	}
	rec, err := smokeping.Reconcile(targets, dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "import: %v\n", err)
		return 1
	}

	ctx := context.Background()
	pg, err := pgstore.New(ctx, dsn, 8, func(err error) {
		fmt.Fprintf(os.Stderr, "import: pgstore: %v\n", err)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "import: %v\n", err)
		return 1
	}
	defer pg.Close()

	// Long-range history (anything older than the raw-retention window) is only
	// queryable through the hourly/daily aggregates. Without them, importing raw
	// rows anyway and continuing (the old behavior) would silently produce a DB
	// where any history older than the eventual 30-day raw retention window can
	// NEVER be materialized into samples_daily: once -downsample is later enabled,
	// EnableDownsampling's one-time backfill only reaches back 30 days (matching
	// the raw retention it installs at the same time), and the retention policy
	// then deletes the raw rows out from under the older history before it's ever
	// aggregated — the "full consolidated history" this importer promises would be
	// silently lost (review Finding #3). So --history refuses to import anything at
	// all until aggregates already exist, forcing the correct operator order:
	// enable downsampling first, then import.
	hasCaggs, err := pg.AggregatesExist(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "import: checking continuous aggregates: %v\n", err)
		return 1
	}
	if !hasCaggs {
		fmt.Fprintln(os.Stderr, "import: continuous aggregates are not enabled on this database; start the "+
			"collector with `-downsample` (or otherwise enable downsampling) before importing history, so "+
			"history older than the 30-day raw retention is materialized into the daily aggregate. No rows "+
			"were imported.")
		return 1
	}

	dests, err := loadHistoryDestinations(ctx, dsn, configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "import: finding the configured targets to store history under: %v. No rows were imported.\n", err)
		return 1
	}
	if dests == nil {
		fmt.Println("note: no --config given and the database config defines no targets, so history is stored under")
		fmt.Println("      each target's SmokePing path. That is the id of a YAML-config target with no explicit `id`;")
		fmt.Println("      if these targets are in a YAML config, pass it with --config so explicit ids are honored.")
	} else {
		fmt.Printf("storing history under the ids of the matching targets in %s\n", strings.Join(dests.consulted, " and "))
	}

	now := time.Now()
	var totalRows int64
	var minTS, maxTS time.Time
	backfilled := 0
	failed := 0
	unresolved := 0
	for i, t := range rec.Matched {
		// Find the configured target this history belongs to before reading the RRD: rows
		// stored under any other key are invisible to every graph and API read (the hub
		// resolves a target to its stable id), so a target with no single destination is
		// reported and skipped rather than imported under its SmokePing path.
		key := t.Name
		if dests != nil {
			id, err := dests.resolve(t.Name)
			if err != nil {
				fmt.Fprintf(os.Stderr, "import: [%d/%d] %s: not imported: %v\n", i+1, len(rec.Matched), t.Name, err)
				unresolved++
				continue
			}
			key = id
		}
		// Validate BEFORE extracting/inserting: a target whose resolved ping count can't
		// be trusted (e.g. 0 from a missing Database file — Finding #5) must never reach
		// an insert. It's reported as a failed target and counted toward the run's
		// partial-failure exit code, and the other matched targets still import.
		if err := validTargetPings(t); err != nil {
			fmt.Fprintf(os.Stderr, "import: [%d/%d] %v\n", i+1, len(rec.Matched), err)
			failed++
			continue
		}
		rrdPath := filepath.Join(dataDir, t.Name+".rrd")
		samples, err := smokeping.ExtractRRD(rrdtoolBin, rrdPath, now)
		if err != nil {
			// A corrupt/unreadable RRD is a partial failure, not a silent skip: count it so the
			// summary and exit code reflect that this target's history was NOT imported, rather
			// than reporting 0 failed / exit 0 on an incomplete migration (CODE_REVIEW #4).
			fmt.Fprintf(os.Stderr, "import: [%d/%d] %s: extract failed, skipping: %v\n", i+1, len(rec.Matched), t.Name, err)
			failed++
			continue
		}
		samples, droppedLoss := validLossSamples(samples, t.Pings)
		if droppedLoss > 0 {
			fmt.Fprintf(os.Stderr, "import: [%d/%d] %s: dropped %d sample(s) with invalid loss (loss<0 or loss>pings=%d)\n",
				i+1, len(rec.Matched), t.Name, droppedLoss, t.Pings)
		}
		rows := make([]pgstore.ImportRow, len(samples))
		for j, s := range samples {
			rows[j] = pgstore.ImportRow{
				TS:            s.TS,
				Target:        key,
				Probe:         t.Probe,
				Host:          t.Host,
				Pings:         t.Pings,
				Loss:          s.Loss,
				MedianSeconds: s.Median,
			}
		}
		n, err := pg.ImportSamples(ctx, rows)
		if err != nil {
			fmt.Fprintf(os.Stderr, "import: [%d/%d] %s: insert failed: %v\n", i+1, len(rec.Matched), t.Name, err)
			return 1
		}
		totalRows += n
		backfilled++
		if key == t.Name {
			fmt.Fprintf(os.Stdout, "[%d/%d] %s: %d rows\n", i+1, len(rec.Matched), t.Name, n)
		} else {
			fmt.Fprintf(os.Stdout, "[%d/%d] %s (id %s): %d rows\n", i+1, len(rec.Matched), t.Name, key, n)
		}
		if len(samples) > 0 {
			if minTS.IsZero() || samples[0].TS.Before(minTS) {
				minTS = samples[0].TS
			}
			last := samples[len(samples)-1].TS
			if maxTS.IsZero() || last.After(maxTS) {
				maxTS = last
			}
		}
	}

	refreshed := false
	// partialRefresh: RefreshAggregates (via refreshWindowFor) caps `until`
	// at now()-1h, since continuous aggregates only refresh closed,
	// bucket-aligned ranges — a still-live SmokePing install being imported
	// mid-collection can have maxTS newer than that cap, in which case the
	// refresh genuinely does NOT cover the newest imported samples yet (they
	// land in the raw `samples` table either way — nothing is lost — but
	// won't show up via the hourly/daily aggregate views until the regular
	// background refresh policy catches up).
	partialRefresh := false
	if hasCaggs && !minTS.IsZero() && !maxTS.IsZero() {
		refreshFrom, refreshUntil := refreshWindowFor(minTS, maxTS, now)
		if err := pg.RefreshAggregates(ctx, refreshFrom, refreshUntil); err != nil {
			fmt.Fprintf(os.Stderr, "import: refresh aggregates: %v\n", err)
			return 1
		}
		refreshed = true
		partialRefresh = maxTS.After(refreshUntil)
	}

	printHistorySummary(os.Stdout, historyCounts{
		backfilled: backfilled, rows: totalRows, configOnly: len(rec.ConfigOnly), orphans: len(rec.Orphans),
		unresolved: unresolved, failed: failed,
	}, hasCaggs, refreshed, partialRefresh)
	if failed > 0 || unresolved > 0 {
		return exitPartialFailure
	}
	return 0
}

// minRefreshSpan is the minimum [from,until) width handed to
// RefreshAggregates. TimescaleDB's refresh_continuous_aggregate errors
// "refresh window too small" unless the window covers at least two buckets
// of the aggregate being refreshed (its own error hint), and RefreshAggregates
// refreshes both the hourly (1h bucket) and daily (1d bucket) continuous
// aggregates over the SAME window — so the binding constraint is the daily
// one, at least 2 days. 72h (3 days) gives a safety margin above that for
// whatever alignment adjustment TimescaleDB applies internally. Without this,
// a target with only a short or sparse RRD history (or any --history run
// whose Matched targets' combined sample range happens to be narrow) would
// have its samples import cleanly but the refresh step hard-fail.
const minRefreshSpan = 72 * time.Hour

// refreshWindowFor computes the window to hand RefreshAggregates for a
// --history run's combined [minTS,maxTS] extracted-sample range: `until` is
// capped at now()-1h — mirroring RefreshAggregates' own cap (continuous
// aggregates only refresh closed, bucket-aligned ranges, so the still-open
// last hour is left to the regular refresh policy) — and `from` is then
// pulled back from that (already-capped) `until`, never from the raw maxTS,
// so the guaranteed minRefreshSpan survives the cap rather than being
// silently eaten by it. Widening only backward (never forward past `until`)
// matters because forward padding into the future would just be clamped
// away by RefreshAggregates' own cap and defeat the guarantee.
func refreshWindowFor(minTS, maxTS, now time.Time) (time.Time, time.Time) {
	until := maxTS
	if ceiling := now.Add(-time.Hour); until.After(ceiling) {
		until = ceiling
	}
	from := minTS
	if until.Sub(from) < minRefreshSpan {
		from = until.Add(-minRefreshSpan)
	}
	return from, until
}

// historyCounts are the per-target outcomes of a --history run, for printHistorySummary.
type historyCounts struct {
	backfilled int
	rows       int64
	configOnly int
	orphans    int
	unresolved int // matched, but no single configured target to store the history under
	failed     int // matched, but invalid pings or an unreadable RRD
}

// printHistorySummary reports what --history did: how many matched targets
// it backfilled, the total rows actually inserted (0 on a re-run — see
// ImportSamples' ON CONFLICT DO NOTHING), how many config-only targets
// and orphan .rrds it left untouched (matching printReconciliationSummary's
// counts so --report's preview and --history's outcome read the same way),
// how many matched targets had no single configured target to store their
// history under (unresolved), and how many failed validation (an
// unresolvable ping count — Finding #5 — or an unreadable RRD); both were
// skipped with no rows written, and either being non-zero is why the run's
// exit code is exitPartialFailure rather than 0. The aggregate-refresh line
// is deliberately not a flat "done":
// partialRefresh means RefreshAggregates' own now()-1h cap left the newest
// imported samples (from a still-live SmokePing source) outside the
// refreshed range — raw data is never lost, but claiming an unqualified
// "done" there would overstate what's actually queryable via the
// hourly/daily views right now.
func printHistorySummary(w io.Writer, c historyCounts, hasCaggs, refreshed, partialRefresh bool) {
	fmt.Fprintf(w, "smokeping history: %d target(s) backfilled, %d row(s) inserted, %d config-only skipped, %d orphan(s) skipped, %d unresolved, %d failed\n",
		c.backfilled, c.rows, c.configOnly, c.orphans, c.unresolved, c.failed)
	if c.unresolved > 0 {
		fmt.Fprintf(w, "unresolved: %d target(s) have no single configured target to store their history under; nothing was imported for them (reasons above)\n", c.unresolved)
	}
	switch {
	case !hasCaggs:
		fmt.Fprintln(w, "aggregate refresh: skipped (no continuous aggregates — run `smoked -downsample` first)")
	case refreshed && partialRefresh:
		fmt.Fprintln(w, "aggregate refresh: done up through the last full hour; the most recent (<1h old) imported samples are stored but not yet reflected in the hourly/daily views — the regular background refresh policy will pick them up")
	case refreshed:
		fmt.Fprintln(w, "aggregate refresh: done")
	default:
		fmt.Fprintln(w, "aggregate refresh: skipped (no rows extracted)")
	}
}
