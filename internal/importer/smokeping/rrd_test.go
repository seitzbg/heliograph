package smokeping

import (
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// mustRun runs an rrdtool subcommand and fails the test with its combined
// output on error — every RRD-building step in these tests goes through this
// so a malformed `create`/`update` invocation shows the RRD error text
// instead of an opaque exit status.
func mustRun(t *testing.T, bin string, args ...string) {
	t.Helper()
	out, err := exec.Command(bin, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", bin, args, err, out)
	}
}

// TestExtractRRD is the brief's smoke test: a tiny synthetic RRD with one
// 300s RRA, no gaps. It exercises the fetch + parse path end to end (the
// multi-tier stitching is exercised in the Task-4 e2e against the real
// fixture, which has SmokePing's actual RRA layout).
func TestExtractRRD(t *testing.T) {
	bin, err := exec.LookPath("rrdtool")
	if err != nil {
		t.Skip("rrdtool not on PATH")
	}
	dir := t.TempDir()
	rrd := filepath.Join(dir, "t.rrd")
	start := int64(1_600_000_000) // fixed
	mustRun(t, bin, "create", rrd, "--start", fmt.Sprint(start-1), "--step", "300",
		"DS:median:GAUGE:600:0:180", "DS:loss:GAUGE:600:0:20", "RRA:AVERAGE:0.5:1:100")
	for i := 0; i < 10; i++ {
		ts := start + int64(i)*300
		mustRun(t, bin, "update", rrd, fmt.Sprintf("%d:%f:%d", ts, 0.010+float64(i)*0.001, i%3))
	}
	got, err := ExtractRRD(bin, rrd, time.Unix(start+10*300, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no samples extracted")
	}
	// median in seconds ~0.01x, loss small ints, timestamps within range
	for _, s := range got {
		if s.Median <= 0 || s.Median > 1 {
			t.Errorf("median out of range: %v", s.Median)
		}
		if s.Loss < 0 || s.Loss > 20 {
			t.Errorf("loss out of range: %d", s.Loss)
		}
	}
}

// Audit M2: an install whose coarsest AVERAGE RRA retains more than the standard
// ~360 days (a common retention tweak) must have that older history imported, not
// silently truncated at the hard-coded 360-day coarsest tier. This RRD's coarsest
// (only) AVERAGE RRA holds ~400 days, with a real data cluster ~380 days before
// `last` — older than the 360-day tier lookback. ExtractRRD must reach it.
func TestExtractRRDBeyond360DayCoarseRRA(t *testing.T) {
	bin, err := exec.LookPath("rrdtool")
	if err != nil {
		t.Skip("rrdtool not on PATH")
	}
	dir := t.TempDir()
	rrd := filepath.Join(dir, "long.rrd")
	const step = int64(43200)    // 12h base step
	last := int64(1_728_000_000) // aligned to the 43200 grid
	old := last - 760*step       // exactly 380 days before last
	mustRun(t, bin, "create", rrd, "--start", fmt.Sprint(old-step), "--step", fmt.Sprint(step),
		"DS:median:GAUGE:86400:0:180", "DS:loss:GAUGE:86400:0:20",
		"RRA:AVERAGE:0.5:1:800") // 800 * 12h = 400 days of AVERAGE history
	// A cluster ~380d old and a cluster near `last`; the long middle stays NaN (a gap).
	for _, ts := range []int64{old, old + step, old + 2*step, last - 2*step, last - step, last} {
		mustRun(t, bin, "update", rrd, fmt.Sprintf("%d:%f:%d", ts, 0.025, 1))
	}

	got, err := ExtractRRD(bin, rrd, time.Unix(last, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no samples extracted from a 400-day RRD")
	}
	cutoff := last - 360*86400 // the 360-day boundary the buggy coarsest tier stopped at
	var beyond bool
	for _, s := range got {
		if s.TS.Unix() < cutoff {
			beyond = true
		}
	}
	if !beyond {
		var oldest int64 = last
		for _, s := range got {
			if s.TS.Unix() < oldest {
				oldest = s.TS.Unix()
			}
		}
		t.Fatalf("history older than 360d was dropped: oldest extracted sample is %ds before last (want a sample >%ds before last, at ~%ds)",
			last-oldest, last-cutoff, last-old)
	}
}

// TestExtractRRDGapVsTotalLoss covers the refinement over the brief's "skip
// NaN rows": a monitoring migration must be able to tell "nobody measured
// this round" (a true gap: the loss cell itself is unknown) apart from
// "measured, but every ping was lost" (loss is a finite count, median is
// unknown because there was nothing to average). Only the former should
// disappear from the extracted history; the latter must survive so the
// 100%-loss period still renders instead of reading as a hole.
//
// Update timestamps are chosen exactly on the RRA's 300s step grid (create's
// --start is one full step before the first update, and the step itself is a
// multiple of 300) so each raw update maps to exactly one consolidated row
// with no interpolation blending across a row boundary — otherwise a known
// value on one side of a boundary and an unknown on the other would smear
// into a partial average instead of the clean known/unknown split this test
// asserts on.
func TestExtractRRDGapVsTotalLoss(t *testing.T) {
	bin, err := exec.LookPath("rrdtool")
	if err != nil {
		t.Skip("rrdtool not on PATH")
	}
	dir := t.TempDir()
	rrd := filepath.Join(dir, "t.rrd")
	start := int64(1_600_000_200) // multiple of the 300s step -> aligned RRA buckets
	mustRun(t, bin, "create", rrd, "--start", fmt.Sprint(start-300), "--step", "300",
		"DS:median:GAUGE:600:0:180", "DS:loss:GAUGE:600:0:20", "RRA:AVERAGE:0.5:1:100")

	const (
		totalLossIdx = 5 // measured round, every ping lost: median U, loss = pings
		trueGapIdx   = 7 // nothing measured at all: median and loss both U
	)
	for i := 0; i < 10; i++ {
		ts := start + int64(i)*300
		switch i {
		case totalLossIdx:
			mustRun(t, bin, "update", rrd, fmt.Sprintf("%d:U:20", ts))
		case trueGapIdx:
			mustRun(t, bin, "update", rrd, fmt.Sprintf("%d:U:U", ts))
		default:
			mustRun(t, bin, "update", rrd, fmt.Sprintf("%d:%f:%d", ts, 0.010+float64(i)*0.001, i%3))
		}
	}

	got, err := ExtractRRD(bin, rrd, time.Unix(start+10*300, 0))
	if err != nil {
		t.Fatal(err)
	}

	byTS := map[int64]RRDSample{}
	for _, s := range got {
		byTS[s.TS.Unix()] = s
	}

	totalLossTS := start + totalLossIdx*300
	trueGapTS := start + trueGapIdx*300

	sample, ok := byTS[totalLossTS]
	if !ok {
		t.Fatalf("total-loss round at ts=%d missing from result, want it kept", totalLossTS)
	}
	if !math.IsNaN(sample.Median) {
		t.Errorf("total-loss round: Median = %v, want NaN", sample.Median)
	}
	if sample.Loss != 20 {
		t.Errorf("total-loss round: Loss = %d, want 20", sample.Loss)
	}

	if _, ok := byTS[trueGapTS]; ok {
		t.Errorf("true gap at ts=%d present in result, want it skipped", trueGapTS)
	}

	for _, s := range got {
		if s.TS.Unix() == totalLossTS || s.TS.Unix() == trueGapTS {
			continue
		}
		if math.IsNaN(s.Median) || s.Median <= 0 || s.Median > 1 {
			t.Errorf("normal row ts=%d: Median out of range: %v", s.TS.Unix(), s.Median)
		}
		if s.Loss < 0 || s.Loss > 2 {
			t.Errorf("normal row ts=%d: Loss out of range: %d", s.TS.Unix(), s.Loss)
		}
	}
}

// TestExtractRRDIncludesOldestRow guards a boundary bug caught during
// development: rrdtool's `fetch -s <t>` is exclusive of t (it returns only
// rows whose interval ends *after* t), but the brief specs the RRD's own
// span as the closed interval [first, last]. A tier whose look-back window
// clamps its start to exactly rrdtool `first` must still get that oldest row
// — passing `first` straight through as `-s` silently dropped it. Uses a
// deliberately tiny RRA (5 rows) so it fills completely and `first` lands on
// a real, non-gap sample rather than on unwritten (and so NaN-and-skipped-
// anyway) history, which would mask the bug.
func TestExtractRRDIncludesOldestRow(t *testing.T) {
	bin, err := exec.LookPath("rrdtool")
	if err != nil {
		t.Skip("rrdtool not on PATH")
	}
	dir := t.TempDir()
	rrd := filepath.Join(dir, "t.rrd")
	start := int64(1_600_000_200) // multiple of the 300s step -> aligned RRA buckets
	mustRun(t, bin, "create", rrd, "--start", fmt.Sprint(start-300), "--step", "300",
		"DS:median:GAUGE:600:0:180", "DS:loss:GAUGE:600:0:20", "RRA:AVERAGE:0.5:1:5")
	for i := 0; i < 5; i++ {
		ts := start + int64(i)*300
		mustRun(t, bin, "update", rrd, fmt.Sprintf("%d:%f:%d", ts, 0.010+float64(i)*0.001, i%3))
	}

	got, err := ExtractRRD(bin, rrd, time.Unix(start+5*300, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d samples, want 5 (the RRA is exactly full, no gaps)", len(got))
	}
	if got[0].TS.Unix() != start {
		t.Errorf("oldest row ts = %d, want %d (rrdtool `first`) — the fetch -s exclusivity boundary must not drop it", got[0].TS.Unix(), start)
	}
}

// TestExtractRRDTierBoundaryNoDuplicateFinestWins covers a second rrdtool
// boundary quirk, symmetric to the -s exclusivity one above: `rrdtool fetch
// -e <t>` is supposed to be inclusive of t, but when t lands exactly on that
// fetch's own step grid, rrdtool hands back ONE EXTRA row PAST -e (rounding
// "up" to guarantee a covering row even though -e already sat on a
// boundary). ExtractRRD's tier plan makes this land in exactly the wrong
// place: tier 1's lookback (3.5d = 302400s) is an exact multiple of tier 2's
// step (3600s), so tier 2's -e (= tier 1's start) regularly sits on the
// 3600s grid, and its overrun row — a full hour's coarse average — lands on
// a timestamp tier 1 already fetched at full 300s resolution. Without
// dedup, that timestamp appears twice in the result with two different
// Median values, and (sort.Slice being unstable) which one a caller sees is
// unspecified — a straight conflict on (target, vantage, ts) downstream.
//
// This needs two real RRAs (a single-RRA RRD, as in the other tests here,
// never exercises tier stitching at all) built at the real tier scale —
// rrdTiers isn't test-injectable, so the RRD has to actually span several
// days for tier 2 to engage. Update timestamps are exactly step-aligned (as
// in TestExtractRRDGapVsTotalLoss) so consolidation is deterministic, and
// the median alternates sharply between two values every 300s so a 3600s
// average is unmistakably different from either raw value — a value change
// this large can't be explained by consolidation rounding, only by the
// wrong tier's row winning.
func TestExtractRRDTierBoundaryNoDuplicateFinestWins(t *testing.T) {
	bin, err := exec.LookPath("rrdtool")
	if err != nil {
		t.Skip("rrdtool not on PATH")
	}
	dir := t.TempDir()
	rrd := filepath.Join(dir, "t.rrd")

	// last (L) is divisible by 3600 (and so by 300 too) so that tier 1's
	// start = L - 302400 (302400 = 84*3600) lands exactly on tier 2's 3600s
	// grid — the precondition for the -e overrun.
	const last = int64(1_600_002_000)
	const firstTS = last - 320400  // > 3.5 days before tier1's own start, so tier 2 actually runs
	const boundary = last - 302400 // tier 1's start == tier 2's end (-e)
	const dupTS = boundary + 3600  // where tier 2's overrun row lands — inside tier 1's own span

	mustRun(t, bin, "create", rrd, "--start", fmt.Sprint(firstTS-300), "--step", "300",
		"DS:median:GAUGE:600:0:180", "DS:loss:GAUGE:600:0:20",
		"RRA:AVERAGE:0.5:1:1200", // 300s tier
		"RRA:AVERAGE:0.5:12:150") // 3600s tier (12 PDPs/row = 3600s)

	// One bulk `update` with every 300s point from firstTS to last: median
	// alternates 0.01/0.5 by parity of the step index, loss cycles 0..2.
	args := []string{"update", rrd}
	var expectMedian float64
	for ts, i := firstTS, 0; ts <= last; ts, i = ts+300, i+1 {
		median := 0.01
		if i%2 != 0 {
			median = 0.5
		}
		if ts == dupTS {
			expectMedian = median // the true fine-grained value at the contested timestamp
		}
		args = append(args, fmt.Sprintf("%d:%f:%d", ts, median, i%3))
	}
	mustRun(t, bin, args...)

	got, err := ExtractRRD(bin, rrd, time.Unix(last+300, 0))
	if err != nil {
		t.Fatal(err)
	}

	seen := map[int64]int{}
	for _, s := range got {
		seen[s.TS.Unix()]++
	}
	for ts, n := range seen {
		if n > 1 {
			t.Errorf("timestamp %d appears %d times in result, want at most once (tier-boundary duplicate)", ts, n)
		}
	}

	dup, ok := seen[dupTS]
	if !ok || dup == 0 {
		t.Fatalf("expected overlap timestamp %d missing from result entirely", dupTS)
	}
	var dupSample RRDSample
	found := false
	for _, s := range got {
		if s.TS.Unix() == dupTS {
			dupSample = s
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected overlap timestamp %d missing from result", dupTS)
	}
	if math.Abs(dupSample.Median-expectMedian) > 1e-6 {
		t.Errorf("at overlap ts=%d: Median = %v, want %v (the finer 300s tier's value, not a coarse hourly average like ~0.255)",
			dupTS, dupSample.Median, expectMedian)
	}
}

// TestExtractRRDFetchesBeyondFinestRRA is the regression for a critical
// production bug: `first` used to come from bare `rrdtool first`, which
// returns RRA index 0's own first timestamp (SmokePing's finest,
// shortest-lived archive — ~3.5 days in the real layout) rather than the
// RRD's true earliest data. Since the tier loop's `end <= first` break
// condition then fired right after the finest tier finished, every coarser
// tier (hourly, 12-hourly) was silently never fetched: a --history import
// only ever pulled a target's most recent ~3.5 days, no matter how much
// older history its RRD actually held (proven against the real fixture:
// `rrdtool first OpenDNS1.rrd` vs `... --rraindex 1` differ by ~180 days,
// and the hourly RRA has real, non-NaN data across that whole span).
//
// This builds an RRD with a deliberately tiny 300s RRA (fineRows, holding
// only ~2.5h) and a much larger 3600s RRA (coarseRows, holding well over 4
// days), then pushes 4 days of 300s-interval data — far more than the fine
// RRA can retain (it wraps, keeping only the trailing ~2.5h) but well within
// the coarse RRA's window. The pushed span (4 days) is also deliberately
// chosen to exceed rrdTiers' hardcoded tier-1 lookback (3.5 days), so
// tier 1's own window doesn't just get clamped down to the whole span — the
// only way to reach the oldest pushed data is for tier 2 (the 3600s/hourly
// tier) to actually run.
func TestExtractRRDFetchesBeyondFinestRRA(t *testing.T) {
	bin, err := exec.LookPath("rrdtool")
	if err != nil {
		t.Skip("rrdtool not on PATH")
	}
	dir := t.TempDir()
	rrd := filepath.Join(dir, "t.rrd")

	const (
		step       = 300 // base PDP step, matches SmokePing's own
		fineRows   = 30  // 300s RRA: 30*300s = 2.5h window — wraps well before the pushed span ends
		coarseStep = 3600
		coarsePDP  = coarseStep / step // 12
		coarseRows = 110               // 110*3600s ≈ 4.58 days — comfortably covers the pushed span

		pushedSpanSeconds = 4 * 24 * 3600 // 4 days: > rrdTiers' 3.5-day tier-1 lookback
	)
	last := int64(1_700_000_000)
	last -= last % coarseStep // land on the coarse tier's own grid, as the other tier tests do
	pushedFirst := last - pushedSpanSeconds

	mustRun(t, bin, "create", rrd, "--start", fmt.Sprint(pushedFirst-step), "--step", fmt.Sprint(step),
		"DS:median:GAUGE:600:0:180", "DS:loss:GAUGE:600:0:20",
		fmt.Sprintf("RRA:AVERAGE:0.5:1:%d", fineRows),
		fmt.Sprintf("RRA:AVERAGE:0.5:%d:%d", coarsePDP, coarseRows))

	args := []string{"update", rrd}
	for ts := pushedFirst; ts <= last; ts += step {
		args = append(args, fmt.Sprintf("%d:0.02:0", ts))
	}
	mustRun(t, bin, args...)

	// Sanity check: confirm this RRD setup actually reproduces the bug's
	// precondition — bare `rrdtool first` must land well after the true
	// pushed start (proving the fine RRA wrapped), or the rest of this test
	// wouldn't be exercising anything.
	bareFirstOut, err := exec.Command(bin, "first", rrd).Output()
	if err != nil {
		t.Fatal(err)
	}
	bareFirst, err := strconv.ParseInt(strings.TrimSpace(string(bareFirstOut)), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if bareFirst < pushedFirst+24*3600 {
		t.Fatalf("test setup invalid: bare `rrdtool first` = %d is not comfortably after the true pushed start %d — fineRows didn't actually wrap", bareFirst, pushedFirst)
	}

	got, err := ExtractRRD(bin, rrd, time.Unix(last+step, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("no samples extracted")
	}

	oldestExtracted := got[0].TS.Unix() // sorted ascending
	if oldestExtracted > pushedFirst+2*coarseStep {
		t.Fatalf("oldest extracted sample ts=%d, want within ~%ds of the true pushed start %d — the coarser (hourly) tier was never fetched, reproducing the truncation bug (bare `rrdtool first` was %d)",
			oldestExtracted, 2*coarseStep, pushedFirst, bareFirst)
	}
}
