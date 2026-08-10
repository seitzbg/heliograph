package smokeping

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Reconciliation is the result of matching a flattened target list (Targets)
// against the .rrd files actually present under a SmokePing data directory.
// Config is the source of truth: Reconcile never invents an ImportTarget
// from an orphan .rrd, and an orphan is never imported — it is only reported
// so an operator can decide (e.g. a target renamed or removed from Targets
// after data collection started).
type Reconciliation struct {
	Matched    []ImportTarget // target has a dataDir/<Name>.rrd
	ConfigOnly []ImportTarget // target has no matching .rrd under dataDir
	Orphans    []string       // dataDir/*.rrd relpaths (slash-joined, .rrd stripped) with no matching target; sorted
}

// Reconcile classifies each target by whether dataDir/<Name>.rrd exists
// (Matched vs ConfigOnly), then walks dataDir for every *.rrd file and
// reports any whose stripped relative path isn't one of the targets' Names
// as an Orphan.
func Reconcile(targets []ImportTarget, dataDir string) (Reconciliation, error) {
	var rec Reconciliation
	names := make(map[string]bool, len(targets))
	for _, t := range targets {
		names[t.Name] = true
		if _, err := os.Stat(filepath.Join(dataDir, t.Name+".rrd")); err == nil {
			rec.Matched = append(rec.Matched, t)
		} else {
			rec.ConfigOnly = append(rec.ConfigOnly, t)
		}
	}

	err := filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".rrd") {
			return nil
		}
		rel, err := filepath.Rel(dataDir, path)
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(filepath.ToSlash(rel), ".rrd")
		if !names[name] {
			rec.Orphans = append(rec.Orphans, name)
		}
		return nil
	})
	if err != nil {
		return Reconciliation{}, err
	}
	sort.Strings(rec.Orphans)
	return rec, nil
}
