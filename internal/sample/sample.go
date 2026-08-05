// Package sample implements SmokePing's per-round result math: median, loss,
// and the "centered" sample array that makes the smoke bands render
// symmetrically around the median.
//
// This mirrors the semantics of lib/Smokeping/probes/base.pm::rrdupdate_string
// in the original SmokePing (see codemap 01/03). A probe's only job is to return
// the received round-trip times; this package derives everything stored.
package sample

import (
	"math"
	"sort"
)

// Computed is the storage-ready view of one measurement round for one target.
type Computed struct {
	Pings    int       // N: how many probes were expected this round
	Loss     int       // Pings - len(received); loss is simply missing samples
	Median   float64   // sorted median in seconds; NaN when no samples arrived
	Sorted   []float64 // received RTTs (seconds), ascending
	Centered []float64 // length == Pings; NaN in lost slots, samples centered around the median
}

// LossFraction returns loss as a fraction in [0,1].
func (c Computed) LossFraction() float64 {
	if c.Pings == 0 {
		return 0
	}
	return float64(c.Loss) / float64(c.Pings)
}

// Compute derives the storage-ready view from the raw received RTTs (seconds).
// rtts may be in any order and may contain fewer than pings entries (the
// difference is packet loss). It matches SmokePing's median selection
// (sorted[len/2]) and its loss-centering: floor(loss/2) unknown slots are
// prepended and the remainder appended, so lost samples sit symmetrically
// around the median.
func Compute(pings int, rtts []float64) Computed {
	if pings < 0 {
		pings = 0 // defensive: a negative N would panic as a slice capacity below
	}
	sorted := make([]float64, 0, len(rtts))
	for _, v := range rtts {
		if !math.IsNaN(v) {
			sorted = append(sorted, v)
		}
	}
	sort.Float64s(sorted)

	entries := len(sorted)
	loss := pings - entries
	if loss < 0 {
		loss = 0
	}

	median := math.NaN()
	if entries > 0 {
		median = sorted[entries/2]
	}

	centered := make([]float64, 0, pings)
	if entries > 0 {
		lead := loss / 2 // floor
		for i := 0; i < lead; i++ {
			centered = append(centered, math.NaN())
		}
		centered = append(centered, sorted...)
		for len(centered) < pings {
			centered = append(centered, math.NaN())
		}
	} else {
		for i := 0; i < pings; i++ {
			centered = append(centered, math.NaN())
		}
	}

	return Computed{
		Pings:    pings,
		Loss:     loss,
		Median:   median,
		Sorted:   sorted,
		Centered: centered,
	}
}
