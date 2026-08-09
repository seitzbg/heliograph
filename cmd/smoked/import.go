// smoked import smokeping <dir> reads a legacy SmokePing config directory
// (Targets, Probes, Database) and turns it into a modern target-tree
// fragment: YAML to stdout/--out by default for review, or merged straight
// into the DB config fragment with --apply. This is slice A of the
// importer — RRD history backfill and a --report/--history mode are slice B,
// not implemented here.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"

	"smokeping-modern/internal/config"
	"smokeping-modern/internal/configstore"
	"smokeping-modern/internal/importer/smokeping"
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

// importCmd implements `smoked import smokeping <dir> [--out FILE] [--apply] [--dsn DSN]`.
func importCmd(args []string) int {
	const usage = "usage: smoked import smokeping <dir> [--out FILE] [--apply] [--dsn DSN]"
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
	if err := fs.Parse(rest[1:]); err != nil {
		return 2
	}

	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		return string(b)
	}
	root, sum, err := smokeping.Parse(read("Targets"), read("Probes"), read("Database"))
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
		if code := applyFragment(*dsn, root); code != 0 {
			return code
		}
	}
	return 0
}

// applyFragment merges root's targets into the DB config fragment, mirroring configCmd's
// `config import` flow: read the current fragment, append (idempotent — an unchanged
// re-import reports 0 added), write it back under optimistic concurrency.
func applyFragment(dsn string, root *config.Node) int {
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
		fmt.Printf("nothing to import (%d unchanged)\n", unchanged)
		return 0
	}
	if err := cs.Set(ctx, merged, version); err != nil {
		if errors.Is(err, configstore.ErrConflict) {
			fmt.Fprintf(os.Stderr, "import: %v (re-run to retry)\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "import: %v\n", err)
		}
		return 1
	}
	fmt.Printf("imported %d targets → database config v%d (%d unchanged)\n", added, version+1, unchanged)
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
