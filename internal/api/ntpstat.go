package api

import "sync"

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
}

// set records (or overwrites) a vantage's latest clock stat for a target.
func (r *remoteNTPStats) set(vantage, target string, offsetSec float64, stratum uint8, measure string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = make(map[remoteNTPKey]remoteNTPVal)
	}
	r.m[remoteNTPKey{vantage, target}] = remoteNTPVal{offsetSec, stratum, measure}
}

// clear drops a vantage's clock stat for a target — used when a round no longer carries one
// (unsynchronized, unreachable, or an agent that does not report the stat), so a remote panel
// stops showing a value that is no longer being measured rather than a stale one.
func (r *remoteNTPStats) clear(vantage, target string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, remoteNTPKey{vantage, target})
}

// lookupFor returns a per-target accessor bound to one vantage, matching Server.NTPStat's shape so
// latestDTO consumes either the hub's local registry or a remote vantage's store uniformly.
func (r *remoteNTPStats) lookupFor(vantage string) func(target string) (float64, uint8, string, bool) {
	return func(target string) (float64, uint8, string, bool) {
		r.mu.RLock()
		defer r.mu.RUnlock()
		v, ok := r.m[remoteNTPKey{vantage, target}]
		if !ok {
			return 0, 0, "", false
		}
		return v.offsetSec, v.stratum, v.measure, true
	}
}
