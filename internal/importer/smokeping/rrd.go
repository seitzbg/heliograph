package smokeping

import (
	"bufio"
	"bytes"
	"fmt"
	"math"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// RRDSample is one historical measurement extracted from a SmokePing .rrd:
// TS is the sample's UTC timestamp, Median the round-trip time in seconds,
// and Loss the number of pings lost that round. Median may be math.NaN() —
// see ExtractRRD's doc comment for when that happens and why it is kept
// rather than dropped.
type RRDSample struct {
	TS     time.Time
	Median float64
	Loss   int
}

// rrdTier is one step of ExtractRRD's finest-first stitching plan: Step is
// the RRA resolution (seconds) to fetch at, Lookback how far back (seconds)
// from the RRD's last timestamp that RRA is expected to still hold data.
type rrdTier struct {
	Step, Lookback int64
}

// rrdTiers is SmokePing's standard three-RRA layout, finest first: ~3.5 days
// at the base 300s step, ~180 days at 3600s (1h), ~360 days at 43200s (12h).
// ExtractRRD walks these in order and, for each, fetches only the slice of
// its lookback window not already covered by a finer tier, so the tiers
// partition the RRD's history into disjoint spans and no sample is fetched
// (or counted) twice.
var rrdTiers = []rrdTier{
	{Step: 300, Lookback: int64(3.5 * 24 * 3600)},
	{Step: 3600, Lookback: 180 * 24 * 3600},
	{Step: 43200, Lookback: 360 * 24 * 3600},
}

// ExtractRRD reads a SmokePing .rrd's full AVERAGE-consolidated history by
// shelling out to `rrdtool fetch` once per RRA tier (rrdTiers, finest
// first), each call restricted to the slice of that tier's look-back window
// a finer tier didn't already cover, and all spans clamped to the RRD's
// actual [first, last] (read via `rrdtool first`/`rrdtool last`). Concretely,
// with L = the RRD's last timestamp: tier 1 covers (L-3.5d, L], tier 2
// covers (L-180d, L-3.5d], tier 3 covers (first, L-180d] — each the
// half-open window the previous tier's start didn't already claim. now
// defensively caps L at now.Unix() (never trust an RRD timestamp beyond the
// caller's notion of "now") and keeps the result reproducible in tests
// without ExtractRRD itself calling time.Now().
//
// A row is skipped only when its loss cell is non-finite (NaN/-nan/U in
// rrdtool's fetch output): that means no round was measured at all — a true
// gap. A row whose loss is a finite count but whose median is non-finite is
// a round that *was* measured but lost every ping; it is kept, with Median
// left as math.NaN(). Downstream storage (pgstore's ImportSamples via
// nanToNil) maps a NaN median to SQL NULL, so a 100%-loss period still shows
// on the graph instead of silently reading as a gap in history.
//
// Results are sorted by TS ascending. An RRD with no first/last data (e.g.
// freshly created, never updated) yields (nil, nil), not an error.
func ExtractRRD(rrdtoolBin, rrdPath string, now time.Time) ([]RRDSample, error) {
	last, err := rrdEpoch(rrdtoolBin, rrdPath, "last")
	if err != nil {
		return nil, err
	}
	if nowUnix := now.Unix(); last > nowUnix {
		last = nowUnix
	}
	// first is deliberately NOT `rrdtool first <rrdPath>`: that bare command
	// returns RRA index 0's own first timestamp — SmokePing's finest, 300s
	// archive, sized to hold only ~3.5 days — not the RRD's true earliest
	// data. Using it here silently truncated every RRD's imported history to
	// its most recent ~3.5 days: with first ≈ last-3.5d, the tier loop below
	// processes tier 1 (300s) and then immediately hits `end <= first` and
	// breaks, so tier 2 (hourly, 180d) and tier 3 (12h, 360d) were never
	// fetched. rrdOldestSeconds instead derives first from the RRD's actual
	// RRA layout (the true oldest AVERAGE-consolidated data available).
	oldest, err := rrdOldestSeconds(rrdtoolBin, rrdPath)
	if err != nil {
		return nil, err
	}
	first := last - oldest
	if last <= first {
		return nil, nil
	}

	var out []RRDSample
	// seen guards against a second rrdtool boundary quirk, symmetric to the
	// -s one handled by fetchStart below: when a tier's -e lands exactly on
	// that tier's own step grid, rrdtool fetch returns ONE EXTRA row past -e
	// (rounding "up" to guarantee at least one covering row, even though -e
	// already sat on a boundary). Because every tier's Lookback is an exact
	// multiple of the next tier's Step (302400 = 84*3600, 15552000 =
	// 360*43200), that overrun row's timestamp regularly lands back inside
	// the finer tier's own (already-fetched) span — e.g. tier 2 (3600s) can
	// hand back a hopelessly coarse hourly average timestamped one hour past
	// its nominal end, exactly the instant tier 1 (300s) already fetched at
	// full resolution. Rather than chase this with more boundary arithmetic
	// (fragile, and there is no guarantee it's the only such quirk), tiers
	// are processed finest-first (rrdTiers' own order) and any sample whose
	// timestamp a finer tier already produced is dropped: the finer,
	// higher-resolution value always wins.
	seen := make(map[int64]bool)
	// end is the exclusive-lower-bound-turned-inclusive-upper-bound carried
	// from the previous (finer) tier: that tier already claimed everything
	// in (its own start, end], so this tier's window stops at end.
	end := last
	for i, tier := range rrdTiers {
		if end <= first {
			break
		}
		start := last - tier.Lookback
		if start < first {
			start = first
		}
		// The coarsest (last) tier owns ALL remaining history down to the RRD's
		// true oldest data (first), even when that predates its nominal lookback:
		// an install whose coarsest AVERAGE RRA retains more than the standard
		// ~360 days would otherwise have everything older than the lookback
		// silently dropped (audit M2). rrdtool fetch selects the RRA that spans
		// the widened window, so the extra span is read at the best resolution
		// still available for it.
		if i == len(rrdTiers)-1 {
			start = first
		}
		if start >= end {
			continue // this tier's whole window was already claimed by a finer tier
		}
		// fetchStart is what actually goes on the wire as `-s`. rrdtool fetch's
		// -s is exclusive (it returns rows whose interval ends strictly after
		// -s), which is exactly what's wanted at an inter-tier boundary — those
		// are spec'd open on the low end, e.g. tier 1 is (last-3.5d, last]. But
		// the RRD's own first is spec'd as a *closed* lower bound ([first,
		// last]), so when start lands exactly on first (clamped there, or
		// coincidentally equal), back the wire value off by one second or the
		// RRD's single oldest row — real data — would be silently dropped.
		fetchStart := start
		if start == first {
			fetchStart = first - 1
		}
		samples, err := fetchTier(rrdtoolBin, rrdPath, tier.Step, fetchStart, end)
		if err != nil {
			return nil, err
		}
		for _, s := range samples {
			ts := s.TS.Unix()
			if seen[ts] {
				continue // a finer tier already produced this instant at higher resolution
			}
			seen[ts] = true
			out = append(out, s)
		}
		end = start
	}

	sort.Slice(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	return out, nil
}

// rrdEpoch shells out to `rrdtool first <rrdPath>` or `rrdtool last <rrdPath>`
// (which is which selected by cmd) and parses its single-integer stdout.
func rrdEpoch(rrdtoolBin, rrdPath, cmd string) (int64, error) {
	out, err := exec.Command(rrdtoolBin, cmd, rrdPath).Output()
	if err != nil {
		return 0, fmt.Errorf("smokeping: rrdtool %s %s: %w", cmd, rrdPath, exitErr(err))
	}
	epoch, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("smokeping: rrdtool %s %s: unparsable output %q: %w", cmd, rrdPath, out, err)
	}
	return epoch, nil
}

// rrdOldestSeconds returns how far back before the RRD's last update its
// truly oldest AVERAGE-consolidated data can exist: the largest coverage
// (rows * pdp_per_row * step) among the RRD's AVERAGE RRAs — the archive
// ExtractRRD's coarsest tier expects to hold the oldest history. This shells
// out to `rrdtool info` once and derives the bound from the RRA layout,
// rather than an extra `rrdtool first --rraindex <n>` call for whichever RRA
// wins: safe even if these numbers don't line up exactly with rrdTiers'
// hardcoded windows, since a fetch that reaches back further than an RRA
// actually holds data for just returns unknown/NaN rows, which the true-gap
// skip in parseFetch already discards. Only AVERAGE RRAs count (MIN/MAX
// archives, even if longer-lived on some non-standard install, are never
// queried by fetchTier).
func rrdOldestSeconds(rrdtoolBin, rrdPath string) (int64, error) {
	out, err := exec.Command(rrdtoolBin, "info", rrdPath).Output()
	if err != nil {
		return 0, fmt.Errorf("smokeping: rrdtool info %s: %w", rrdPath, exitErr(err))
	}
	return parseRRDMaxCoverage(out)
}

// rraLayout accumulates the fields of one `rra[N].*` block from `rrdtool
// info` output as they're seen — the fields aren't necessarily adjacent
// (cdp_prep entries interleave), so this is filled incrementally.
type rraLayout struct {
	cf        string
	rows, pdp int64
}

// parseRRDMaxCoverage parses `rrdtool info` output and returns the largest
// (rows * pdp_per_row * step) among its AVERAGE RRAs. Lines are shaped
// `key = value` (value optionally double-quoted), e.g. `step = 300`,
// `rra[1].cf = "AVERAGE"`, `rra[1].rows = 4320`, `rra[1].pdp_per_row = 12` —
// order-independent and freely interleaved with fields this doesn't care
// about (xff, cdp_prep[*].*, ...), so every line is parsed into `step` or
// keyed into the RRA index found in its `rra[N].` prefix before computing
// coverage, rather than assuming any particular field order.
func parseRRDMaxCoverage(out []byte) (int64, error) {
	var step int64
	rras := map[int]*rraLayout{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		key, val, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"`)
		if key == "step" {
			step, _ = strconv.ParseInt(val, 10, 64)
			continue
		}
		idx, field, ok := parseRRAKey(key)
		if !ok {
			continue
		}
		r, ok := rras[idx]
		if !ok {
			r = &rraLayout{}
			rras[idx] = r
		}
		switch field {
		case "cf":
			r.cf = val
		case "rows":
			r.rows, _ = strconv.ParseInt(val, 10, 64)
		case "pdp_per_row":
			r.pdp, _ = strconv.ParseInt(val, 10, 64)
		}
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("smokeping: read rrdtool info: %w", err)
	}
	if step <= 0 {
		return 0, fmt.Errorf("smokeping: rrdtool info: no step found")
	}
	var maxCoverage int64
	for _, r := range rras {
		if r.cf != "AVERAGE" {
			continue
		}
		if cov := r.rows * r.pdp * step; cov > maxCoverage {
			maxCoverage = cov
		}
	}
	if maxCoverage <= 0 {
		return 0, fmt.Errorf("smokeping: rrdtool info: no AVERAGE RRA found")
	}
	return maxCoverage, nil
}

// parseRRAKey splits an `rrdtool info` key like `rra[1].pdp_per_row` (or the
// deeper `rra[1].cdp_prep[0].value`, which simply won't match any field this
// package cares about) into its RRA index and field name. Keys with no
// `rra[N].` prefix (`step`, `filename`, ...) return ok=false.
func parseRRAKey(key string) (idx int, field string, ok bool) {
	rest, ok := strings.CutPrefix(key, "rra[")
	if !ok {
		return 0, "", false
	}
	end := strings.IndexByte(rest, ']')
	if end < 0 {
		return 0, "", false
	}
	idx, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0, "", false
	}
	field, ok = strings.CutPrefix(rest[end+1:], ".")
	if !ok {
		return 0, "", false
	}
	return idx, field, true
}

// fetchTier runs `rrdtool fetch <rrdPath> AVERAGE -r <step> -s <start> -e <end>`
// and parses its text output into samples. rrdtool's own semantics for -s/-e
// make this half-open on start and closed on end (the first row returned is
// the one whose consolidated interval ends after start; the last covers
// end), which is exactly the disjoint-span partitioning ExtractRRD relies on
// to stitch tiers without double-counting a row.
func fetchTier(rrdtoolBin, rrdPath string, step, start, end int64) ([]RRDSample, error) {
	out, err := exec.Command(rrdtoolBin, "fetch", rrdPath, "AVERAGE",
		"-r", strconv.FormatInt(step, 10),
		"-s", strconv.FormatInt(start, 10),
		"-e", strconv.FormatInt(end, 10),
	).Output()
	if err != nil {
		return nil, fmt.Errorf("smokeping: rrdtool fetch %s -r %d -s %d -e %d: %w", rrdPath, step, start, end, exitErr(err))
	}
	return parseFetch(out)
}

// exitErr enriches err with the subprocess's stderr text when err is an
// *exec.ExitError, so a failure reads as rrdtool's own diagnostic (e.g. "no
// such file") rather than just "exit status 1".
func exitErr(err error) error {
	if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
	}
	return err
}

// parseFetch parses `rrdtool fetch`'s text output: a header line naming the
// DSes (used to locate the "median" and "loss" columns — order is not
// assumed), a blank line, then one data line per consolidated row shaped
// "           <epoch>: <v1> <v2> ...". A fetch that returns only unknown
// rows yields (nil, nil), not an error — that's a legitimate (if boring)
// result, not a parse failure.
func parseFetch(out []byte) ([]RRDSample, error) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var cols []string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		cols = strings.Fields(line)
		break
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("smokeping: read rrdtool fetch header: %w", err)
	}
	medianIdx, lossIdx := -1, -1
	for i, c := range cols {
		switch c {
		case "median":
			medianIdx = i
		case "loss":
			lossIdx = i
		}
	}
	if medianIdx < 0 || lossIdx < 0 {
		return nil, fmt.Errorf("smokeping: rrdtool fetch header has no median/loss DS (got %q)", strings.Join(cols, " "))
	}

	var samples []RRDSample
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		epochStr, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue // defensive: not a data line we recognize
		}
		epoch, err := strconv.ParseInt(strings.TrimSpace(epochStr), 10, 64)
		if err != nil {
			continue
		}
		vals := strings.Fields(rest)
		if lossIdx >= len(vals) || medianIdx >= len(vals) {
			continue
		}
		loss := parseRRDFloat(vals[lossIdx])
		if math.IsNaN(loss) {
			continue // true gap: no round was measured at all — skip
		}
		median := parseRRDFloat(vals[medianIdx]) // may legitimately be NaN: total-loss round, kept
		samples = append(samples, RRDSample{
			TS:     time.Unix(epoch, 0).UTC(),
			Median: median,
			Loss:   int(math.Round(loss)),
		})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("smokeping: read rrdtool fetch data: %w", err)
	}
	return samples, nil
}

// parseRRDFloat parses one rrdtool fetch cell. rrdtool prints an unknown
// value as "-nan", "nan", or "NaN" (the sign varies by libc); Go's
// strconv.ParseFloat rejects the signed forms outright ("-nan" is not valid
// Go float syntax), so "nan" is matched case-insensitively as a substring
// first rather than deferring to ParseFloat's own (narrower) NaN handling.
// Anything else that fails to parse is also treated as NaN rather than
// erroring the whole fetch over one malformed cell.
func parseRRDFloat(s string) float64 {
	s = strings.TrimSpace(s)
	if strings.Contains(strings.ToLower(s), "nan") || strings.EqualFold(s, "U") {
		return math.NaN()
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return math.NaN()
	}
	return v
}
