// Package ntpprobe is a native SNTP probe (no external `ntpdate`/`chronyc`) that
// measures how long an NTP server takes to answer a client request, and — as a
// side channel — the server's clock offset and stratum. Registered as "NTP".
//
// In the default `measure: rtt` mode the round-trip time of each SNTPv4 exchange is
// the sample fed to the normal sample+loss pipeline, so an NTP target draws an
// ordinary smoke graph. In `measure: offset` mode the signed clock offset is the
// sample instead, drawing a zero-baselined signed graph through the same pipeline.
// Stratum, and the companion latest offset/stratum stat the dashboard shows beside
// median/loss, are not latencies and stay out of that pipeline entirely — they are
// recorded per target in a small latest-value registry. See latestReg below.
package ntpprobe

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/seitzbg/heliograph/internal/probe"
)

// NTP timestamps count seconds since 1900-01-01; Unix time counts from 1970. The
// gap is 70 years including 17 leap days = 2208988800 seconds.
const ntpUnixEpochDelta = 2208988800

// packetSize is the fixed SNTP/NTP packet length (RFC 4330/5905): 48 bytes, no
// authentication/extension fields.
const packetSize = 48

type ntpProbe struct {
	port     string
	version  uint8         // NTP version written into the request (3 or 4)
	measure  string        // "rtt" (graph query round-trip) or "offset" (graph clock offset)
	interval time.Duration // min gap between the N queries in a round (anti-burst)
}

func init() {
	probe.Register("NTP", "NTP query", map[string]probe.VarSpec{
		"port":        {Doc: "NTP server port", Default: "123", Scope: probe.TargetVar, Kind: probe.KindPort},
		"version":     {Doc: "NTP protocol version to send", Default: "4", Scope: probe.ProbeVar, Enum: []string{"3", "4"}},
		"measure":     {Doc: "value graphed as the smoke series: rtt (query round-trip) or offset (clock offset, signed)", Default: "rtt", Scope: probe.TargetVar, Enum: []string{"rtt", "offset"}},
		"interval_ms": {Doc: "milliseconds between the N queries in a round (paces the burst; RFC 4330 / NTP Pool ToS)", Default: "100", Scope: probe.ProbeVar, Kind: probe.KindPositiveInt},
	}, func(cfg map[string]string) (probe.Probe, error) {
		p := &ntpProbe{port: "123", version: 4, measure: "rtt", interval: 100 * time.Millisecond}
		if v := cfg["port"]; v != "" {
			p.port = v
		}
		if cfg["version"] == "3" {
			p.version = 3
		}
		if cfg["measure"] == "offset" {
			p.measure = "offset"
		}
		if v := cfg["interval_ms"]; v != "" {
			if ms, err := strconv.Atoi(v); err == nil {
				p.interval = time.Duration(ms) * time.Millisecond
			}
		}
		return p, nil
	})
}

func (p *ntpProbe) Name() string { return "NTP" }

func (p *ntpProbe) Measure(ctx context.Context, t probe.Target, pings int) (probe.Result, error) {
	server := net.JoinHostPort(t.Host, t.Param("port", p.port))
	version := p.version
	if v := t.Param("version", ""); v == "3" {
		version = 3
	} else if v == "4" {
		version = 4
	}

	measure := t.Param("measure", p.measure)
	wantOffset := measure == "offset"

	var samples []float64
	// Fair per-ping share of the round budget so an unresponsive server is queried all `pings`
	// times instead of the first request eating the whole round (correct loss). The inter-query
	// delay is reserved from the budget before dividing (it sleeps outside the per-attempt
	// context), so a large interval_ms can't starve the attempts — see PerPingBudgetWithDelay.
	perPing, interDelay := probe.PerPingBudgetWithDelay(ctx, pings, p.interval)
	// A round that ends without a single synchronized reply — every ping lost/unsynchronized/KoD,
	// or the round cancelled — means any previously recorded offset/stratum is stale. Clear it on
	// every exit path so a server that goes unreachable or loses sync stops showing a "good clock"
	// stat (M3). A synchronized ping sets this and updates the stat via latestReg.set below.
	key := statKey(t.Host, t.Param("port", p.port), version)
	syncedThisRound := false
	defer func() {
		if !syncedThisRound {
			latestReg.clear(t.Key(), time.Now())
		}
	}()
	for i := 0; i < pings; i++ {
		if err := ctx.Err(); err != nil {
			return probe.Result{Samples: samples, Kind: measure}, err
		}
		actx, cancel := probe.AttemptContext(ctx, perPing)
		rtt, offset, hasOffset, stratum, ok := p.exchange(actx, server, version)
		cancel()
		if ok {
			if stratum == 0 {
				// Kiss-o'-Death: the server is explicitly telling the client to stop (RFC 4330
				// §8). Back off — send no more this round — and don't count it as a usable sample;
				// the missing samples read as loss. The deferred clear drops any stale stat.
				break
			}
			// Offset is meaningful only from a synchronized server (stratum 1..15) with usable,
			// origin-verified timestamps; stratum 16+ (unsynchronized) is reachable but carries
			// no usable clock offset.
			synced := hasOffset && stratum >= 1 && stratum <= 15
			if wantOffset {
				// Graph the clock offset. Only a synchronized reply yields one; anything else
				// (unsynchronized, unverifiable timestamps) is a lost sample this round.
				if synced {
					samples = append(samples, offset.Seconds())
				}
			} else {
				samples = append(samples, rtt.Seconds())
			}
			if synced {
				latestReg.set(t.Key(), offset.Seconds(), stratum, measure, key, time.Now())
				syncedThisRound = true
			}
		}
		// Pace the round: honor the inter-query delay whether or not the reply was usable, so a
		// responsive server is not hit with a back-to-back burst. Skipped after the final ping
		// (and after a KoD, which breaks above).
		if interDelay > 0 && i < pings-1 {
			select {
			case <-ctx.Done():
				return probe.Result{Samples: samples, Kind: measure}, ctx.Err()
			case <-time.After(interDelay):
			}
		}
	}
	return probe.Result{Samples: samples, Kind: measure}, nil
}

// exchange performs one SNTP client/server round trip. It returns the round-trip time,
// the estimated clock offset, the server stratum, and ok=false for any transport error or
// malformed/ non-server reply (counted as a lost ping by the caller).
func (p *ntpProbe) exchange(ctx context.Context, server string, version uint8) (rtt, offset time.Duration, hasOffset bool, stratum uint8, ok bool) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", server)
	if err != nil {
		return 0, 0, false, 0, false
	}
	defer conn.Close()
	if dl, hasDL := ctx.Deadline(); hasDL {
		_ = conn.SetDeadline(dl)
	}

	req := make([]byte, packetSize)
	// LI (0) | VN (version) | Mode (3 = client): version occupies bits 3-5, mode bits 0-2.
	req[0] = byte(version<<3) | 0x03

	t1 := time.Now()
	// Stamp the request's transmit timestamp (bytes 40..47). A compliant server copies it back as
	// its originate timestamp (bytes 24..31); checking that echo ties the reply to THIS request
	// and rejects stray/replayed/spoofed packets (RFC 5905 client checks). Its byte value is the
	// association nonce — the offset math below uses t1 as a local time.Time, independent of this.
	putNTPTime(req[40:48], t1)
	if _, err := conn.Write(req); err != nil {
		return 0, 0, false, 0, false
	}
	resp := make([]byte, packetSize)
	n, err := conn.Read(resp)
	t4 := time.Now()
	if err != nil || n < packetSize {
		return 0, 0, false, 0, false
	}
	// Reject anything that is not a server reply (mode 4). A mode-4 packet with a bogus
	// origin is still accepted for RTT — a reachability probe cares that the server answered.
	if mode := resp[0] & 0x07; mode != 4 {
		return 0, 0, false, 0, false
	}
	stratum = resp[1]
	// Leap indicator = the top two bits. LI=3 is the alarm condition: the server's own clock is
	// not synchronized, so its timestamps are not a trustworthy time source even at a low stratum.
	leapAlarm := resp[0]>>6 == 3

	// Receive timestamp (t2) at bytes 32..39, transmit timestamp (t3) at 40..47.
	t2 := ntpTime(resp[32:40])
	t3 := ntpTime(resp[40:48])

	rawRTT := t4.Sub(t1)
	// The NTP round-trip excludes the server's processing time (t3-t2). Fall back to the raw
	// round trip if the server left the timestamps empty or they are nonsensical (t3 < t2, or a
	// server-think value larger than the whole round trip), so RTT is never negative or absurd.
	rtt = rawRTT
	if !t2.IsZero() && !t3.IsZero() && !t3.Before(t2) {
		if think := t3.Sub(t2); think >= 0 && think <= rawRTT {
			rtt = rawRTT - think
		}
	}
	// offset = ((t2 - t1) + (t3 - t4)) / 2  (RFC 4330). Publish it (hasOffset) only for a reply
	// we can trust as a genuine clock reading: the origin must echo our transmit timestamp, the
	// server must not be in the leap alarm state, and the server timestamps must be present and
	// sanely ordered (t3 >= t2). A zeroed, reversed, alarmed, or unverifiable reply still yields an
	// RTT reachability sample but no clock offset.
	originOK := bytes.Equal(resp[24:32], req[40:48])
	if originOK && !leapAlarm && !t2.IsZero() && !t3.IsZero() && !t3.Before(t2) {
		offset = (t2.Sub(t1) + t3.Sub(t4)) / 2
		hasOffset = true
	}
	return rtt, offset, hasOffset, stratum, true
}

// putNTPTime writes t as an 8-byte NTP timestamp (seconds since 1900 + a 32-bit fraction), the
// inverse of ntpTime. Used to stamp the request's transmit timestamp so the server echoes it
// back as the originate timestamp for the client origin check in exchange.
func putNTPTime(b []byte, t time.Time) {
	secs := uint32(t.Unix() + ntpUnixEpochDelta)
	frac := uint32((int64(t.Nanosecond()) << 32) / 1e9)
	binary.BigEndian.PutUint32(b[0:4], secs)
	binary.BigEndian.PutUint32(b[4:8], frac)
}

// ntpTime decodes an 8-byte NTP timestamp (seconds since 1900 + a 32-bit fraction) into a
// wall-clock time. A zero timestamp (server left the field empty) decodes to the zero Time.
func ntpTime(b []byte) time.Time {
	secs := binary.BigEndian.Uint32(b[0:4])
	frac := binary.BigEndian.Uint32(b[4:8])
	if secs == 0 && frac == 0 {
		return time.Time{}
	}
	nanos := (int64(frac) * 1e9) >> 32
	return time.Unix(int64(secs)-ntpUnixEpochDelta, nanos)
}

// --- latest-offset registry -------------------------------------------------
//
// This holds the companion latest offset/stratum STAT the dashboard shows beside median/loss —
// distinct from the graphed series. In `measure: offset` mode the offset is also fed through the
// sample pipeline as the graphed series (see Measure); the latest-stat copy kept here, and the
// stratum, never ride that pipeline. The API reads this via LatestFor. Package-level (not
// per-instance) so it survives a config reload that rebuilds the probe instance, and so a single
// accessor wired once in main stays valid.

type ntpLatest struct {
	offsetSec float64
	stratum   uint8
	measure   string // the target's graphed metric ("rtt" or "offset"); lets the UI hide the
	// redundant offset stat on a panel that already graphs offset.
	key string // the measurement identity this reading came from (host+port+version — see statKey).
	// A reader supplies the target's CURRENT identity; the accessor refuses a reading whose key no
	// longer matches, so a same-host reconfiguration (a new port/version) or a slow in-flight probe
	// of the previous endpoint never mislabels the new configuration (CODE_REVIEW M3).
	ts time.Time // when this reading was measured; an older write/clear can't roll it back (M3).
}

var latestReg = &registry{m: map[string]ntpLatest{}}

type registry struct {
	mu sync.RWMutex
	m  map[string]ntpLatest
}

// statKey is the measurement-identity token a clock stat is bound to: the exact endpoint and
// protocol version a round measured. The API rebuilds it (via StatKey) from the current target
// config; a mismatch means the reading belongs to a superseded configuration and is refused (M3).
func statKey(host, port string, version uint8) string {
	return net.JoinHostPort(host, port) + "|v" + strconv.Itoa(int(version))
}

// StatKey builds the clock-stat identity token from a target's NTP settings, so a reader (the API)
// can match the token the probe stored. targetParams are the target's per-node overrides; probeCfg
// is the tree-wide probes.NTP config. Resolution mirrors the probe's own: target param, then probe
// config, then the schema default (port 123, version 4).
func StatKey(host string, targetParams, probeCfg map[string]string) string {
	port := "123"
	if v := probeCfg["port"]; v != "" {
		port = v
	}
	if v := targetParams["port"]; v != "" {
		port = v
	}
	version := uint8(4)
	if probeCfg["version"] == "3" {
		version = 3
	}
	if v := targetParams["version"]; v == "3" {
		version = 3
	} else if v == "4" {
		version = 4
	}
	return statKey(host, port, version)
}

// set records a reading, tagged with its measurement identity (key) and time (ts). A write whose ts
// is older than the stored reading's is rejected, so an out-of-order or slow-completing probe can't
// roll the stat backward (CODE_REVIEW M3).
func (r *registry) set(target string, offsetSec float64, stratum uint8, measure, key string, ts time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.m[target]; ok && ts.Before(cur.ts) {
		return
	}
	r.m[target] = ntpLatest{offsetSec: offsetSec, stratum: stratum, measure: measure, key: key, ts: ts}
}

// clear drops a target's latest offset/stratum so a server that has gone unsynchronized (or a
// removed target) stops showing a stale "good clock" reading. Like set it is freshness-gated: an
// older round's clear can't wipe a newer round's reading (M3). A no-op if nothing was recorded.
func (r *registry) clear(target string, ts time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.m[target]; ok && ts.Before(cur.ts) {
		return
	}
	delete(r.m, target)
}

// LatestFor returns the most recent clock offset (seconds), stratum, and graphed metric recorded for
// an NTP target, or ok=false if none has been measured yet OR the stored reading's measurement
// identity differs from wantKey. Binding the stat to the identity closes M3: after a target keeps
// its stable id but is repointed at a different NTP server/port/version, the old endpoint's
// offset/stratum is not attributed to the new one — the reader passes the target's current StatKey,
// so a mismatch reads as "no stat" until a fresh round for the current endpoint lands. As a special
// case wantKey == "" skips the identity gate: the measuring agent reads back the value its own probe
// just wrote this round (no config drift is possible, and it may lack the config to rebuild the key).
// Wired into the API as Server.NTPStat.
func LatestFor(target, wantKey string) (offsetSec float64, stratum uint8, measure string, ok bool) {
	latestReg.mu.RLock()
	v, ok := latestReg.m[target]
	latestReg.mu.RUnlock()
	if !ok || (wantKey != "" && v.key != wantKey) {
		return 0, 0, "", false
	}
	return v.offsetSec, v.stratum, v.measure, true
}
