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
	first, err := rrdEpoch(rrdtoolBin, rrdPath, "first")
	if err != nil {
		return nil, err
	}
	last, err := rrdEpoch(rrdtoolBin, rrdPath, "last")
	if err != nil {
		return nil, err
	}
	if nowUnix := now.Unix(); last > nowUnix {
		last = nowUnix
	}
	if last <= first {
		return nil, nil
	}

	var out []RRDSample
	// end is the exclusive-lower-bound-turned-inclusive-upper-bound carried
	// from the previous (finer) tier: that tier already claimed everything
	// in (its own start, end], so this tier's window stops at end.
	end := last
	for _, tier := range rrdTiers {
		if end <= first {
			break
		}
		start := last - tier.Lookback
		if start < first {
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
		out = append(out, samples...)
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
