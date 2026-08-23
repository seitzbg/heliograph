package api

import (
	"sync"
	"time"
)

// remoteNTPStats holds the latest NTP clock stat — offset (seconds), stratum, and the graphed
// measure — reported by each remote vantage per target. The hub's own local NTP stat lives in the
// ntpprobe registry (wired as Server.NTPStat); this is its per-vantage equivalent for a target
// measured at a remote vantage, populated from RoundReports on ingest and read by /api/targets for
// a non-local vantage (CODE_REVIEW M2). It is deliberately keyed by (vantage, target) so a remote
// reading is never confused with the hub's local one, closing M3's per-vantage association.
//
// In-memory and best-effort like the local registry: it repopulates on the next round and is not
// persisted across a hub restart. The zero value is usable (the map is created on first set).
type remoteNTPStats struct {
	mu sync.RWMutex
	m  map[remoteNTPKey]remoteNTPVal
}

type remoteNTPKey struct{ vantage, target string }

type remoteNTPVal struct {
	offsetSec float64
	stratum   uint8
	measure   string
	key       string // the measurement identity (host+port+version) this reading came from; the
	// reader supplies the target's current identity and a stale reading whose key no longer matches
	// is refused, so a same-host reconfiguration can't mislabel the new endpoint (M3).
	ts time.Time // when the round was measured; an out-of-order older report can't roll it back (M3).
}

// set records (or overwrites) a vantage's latest clock stat for a target, tagged with the
// measurement identity (key) and time (ts) it was measured under. A write whose ts is older than the
// stored reading's is rejected, so a store-and-forward replay delivering an older round after a newer
// one can't roll the companion stat backward (CODE_REVIEW M3).
func (r *remoteNTPStats) set(vantage, target string, offsetSec float64, stratum uint8, measure, key string, ts time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = make(map[remoteNTPKey]remoteNTPVal)
	}
	k := remoteNTPKey{vantage, target}
	if cur, ok := r.m[k]; ok && ts.Before(cur.ts) {
		return
	}
	r.m[k] = remoteNTPVal{offsetSec, stratum, measure, key, ts}
}

// clear drops a vantage's clock stat for a target — used when a round no longer carries one
// (unsynchronized, unreachable, or an agent that does not report the stat), so a remote panel
// stops showing a value that is no longer being measured rather than a stale one. Like set it is
// freshness-gated: an older round's clear can't wipe a newer round's reading (M3).
func (r *remoteNTPStats) clear(vantage, target string, ts time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := remoteNTPKey{vantage, target}
	if cur, ok := r.m[k]; ok && ts.Before(cur.ts) {
		return
	}
	delete(r.m, k)
}

// lookupFor returns a per-target accessor bound to one vantage, matching Server.NTPStat's shape so
// latestDTO consumes either the hub's local registry or a remote vantage's store uniformly. It
// refuses a reading whose stored measurement identity differs from wantKey (the target's current
// endpoint), so a remote panel never shows a superseded server's offset after the target is
// repointed (M3). wantKey == "" skips the identity gate (parity with ntpprobe.LatestFor).
func (r *remoteNTPStats) lookupFor(vantage string) func(target, wantKey string) (float64, uint8, string, bool) {
	return func(target, wantKey string) (float64, uint8, string, bool) {
		r.mu.RLock()
		defer r.mu.RUnlock()
		v, ok := r.m[remoteNTPKey{vantage, target}]
		if !ok || (wantKey != "" && v.key != wantKey) {
			return 0, 0, "", false
		}
		return v.offsetSec, v.stratum, v.measure, true
	}
}
