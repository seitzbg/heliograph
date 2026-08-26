package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/seitzbg/heliograph/internal/alert"
	"github.com/seitzbg/heliograph/internal/api"
	"github.com/seitzbg/heliograph/internal/config"
	"github.com/seitzbg/heliograph/internal/configstore"
	"github.com/seitzbg/heliograph/internal/federation"
	"github.com/seitzbg/heliograph/internal/model"
	"github.com/seitzbg/heliograph/internal/probe"
	"github.com/seitzbg/heliograph/internal/sample"
	"github.com/seitzbg/heliograph/internal/scheduler"
	"github.com/seitzbg/heliograph/internal/store"

	_ "github.com/seitzbg/heliograph/internal/probe/tcpconnect" // register TCPConnect for the config
)

// warm-start must seed only the recent, cadence-contiguous, same-host/probe suffix — never
// stale or semantically-different history, which could fire a false alert at boot (#6).
// M4: a signed clock offset must never be fed to a latency matcher — alertRTT returns the median
// for an rtt round but NaN for an offset round (a +5s offset must not look like 5000ms latency).
func TestAlertRTTIgnoresOffset(t *testing.T) {
	rtt := scheduler.Outcome{Metric: probe.MetricRTT, Computed: sample.Compute(2, []float64{0.02, 0.02})}
	if v := alertRTT(rtt); v < 0.015 || v > 0.025 {
		t.Errorf("rtt round: alertRTT = %v, want ~0.02", v)
	}
	off := scheduler.Outcome{Metric: probe.MetricOffset, Computed: sample.Compute(2, []float64{5, 5})}
	if v := alertRTT(off); !math.IsNaN(v) {
		t.Errorf("offset round: alertRTT = %v, want NaN (must not trip a latency alert)", v)
	}
}

// M5: after a target changes measure, warm-start must not seed one metric's window from the other.
// recentContiguous truncates at the metric boundary, keeping only the current-metric suffix.
func TestRecentContiguousBreaksOnMetricChange(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	m := warmMeta{host: "h", probe: "NTP", step: time.Minute, metric: probe.MetricOffset}
	rnd := func(ago time.Duration, metric string) scheduler.Outcome {
		return scheduler.Outcome{Target: probe.Target{Name: "t", Host: "h"}, ProbeName: "NTP", Metric: metric, When: now.Add(-ago)}
	}
	hist := []scheduler.Outcome{
		rnd(3*time.Minute, probe.MetricRTT), rnd(2*time.Minute, probe.MetricRTT), // pre-switch
		rnd(time.Minute, probe.MetricOffset), rnd(0, probe.MetricOffset), // current
	}
	if got := recentContiguous(hist, m, now); len(got) != 2 {
		t.Errorf("metric change should truncate to the 2 current (offset) rounds, got %d", len(got))
	}
}

func TestRecentContiguous(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	m := warmMeta{host: "h", probe: "FPing", step: time.Minute}
	rnd := func(ago time.Duration, host, pk string) scheduler.Outcome {
		return scheduler.Outcome{Target: probe.Target{Name: "t", Host: host}, ProbeName: pk, When: now.Add(-ago)}
	}

	// Old breaching rounds followed by a recent contiguous block: only the recent block seeds.
	hist := []scheduler.Outcome{
		rnd(90*24*time.Hour, "h", "FPing"),
		rnd(90*24*time.Hour-time.Minute, "h", "FPing"),
		rnd(2*time.Minute, "h", "FPing"),
		rnd(1*time.Minute, "h", "FPing"),
		rnd(0, "h", "FPing"),
	}
	if got := recentContiguous(hist, m, now); len(got) != 3 {
		t.Errorf("expected the 3 recent contiguous rounds, got %d", len(got))
	}

	// Newest stored round is stale -> seed nothing (the target is dark).
	stale := []scheduler.Outcome{rnd(2*time.Hour, "h", "FPing"), rnd(2*time.Hour-time.Minute, "h", "FPing")}
	if got := recentContiguous(stale, m, now); got != nil {
		t.Errorf("stale newest round should seed nothing, got %d", len(got))
	}

	// Newest round is from a different host (name reused) -> seed nothing.
	mismatch := []scheduler.Outcome{rnd(time.Minute, "h", "FPing"), rnd(0, "other", "FPing")}
	if got := recentContiguous(mismatch, m, now); got != nil {
		t.Errorf("host mismatch on the newest round should seed nothing, got %d", len(got))
	}

	// A cadence gap truncates to the contiguous suffix after it.
	gapped := []scheduler.Outcome{rnd(10*time.Minute, "h", "FPing"), rnd(time.Minute, "h", "FPing"), rnd(0, "h", "FPing")}
	if got := recentContiguous(gapped, m, now); len(got) != 2 {
		t.Errorf("expected 2 rounds after the gap, got %d", len(got))
	}
}

// The hub builds local probe jobs only for targets assigned to its own vantage (local):
// a remote-only target (`vantages: [nyc]`) must NOT be probed here (it would be a false
// local measurement), while a target whose vantages include `local` must be.
func TestBuildRuntimeHonorsVantages(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte(`
database: { step: 30s, pings: 3 }
probes: { TCPConnect: {} }
targets:
  probe: TCPConnect
  children:
    here:   { host: 127.0.0.1, params: { port: "80" } }
    remote: { host: 127.0.0.1, params: { port: "80" }, vantages: [nyc] }
    both:   { host: 127.0.0.1, params: { port: "80" }, vantages: [local, nyc] }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	rt, err := buildRuntime(cfg, 3, 30*time.Second, time.Second, false, nil, nil)
	if err != nil {
		t.Fatalf("buildRuntime: %v", err)
	}
	got := map[string]bool{}
	for _, j := range rt.jobs {
		got[j.Target.Name] = true
	}
	if !got["here"] || !got["both"] {
		t.Errorf("expected local jobs for here+both (vantages include local), got %v", got)
	}
	if got["remote"] {
		t.Error("remote-only target (vantages: [nyc]) must not create a local probe job")
	}
	if len(rt.jobs) != 2 {
		t.Errorf("expected exactly 2 local jobs, got %d (%v)", len(rt.jobs), got)
	}
}

// The swappable runtime must retain the FULL post-inheritance monitor set (all
// vantages), not just the hub's local-filtered slice — the agent assignment endpoint
// (Task 4/5) computes a remote vantage's targets from it. The hub still builds local
// probe jobs only for its own vantage.
//
// Uses TCPConnect (no external binary dependency) rather than FPing so the test is
// portable to CI images without fping installed.
func TestRuntimeRetainsFullMonitorSet(t *testing.T) {
	dir := t.TempDir()
	cfg := "targets:\n" +
		"  probe: TCPConnect\n" +
		"  children:\n" +
		"    local-one: {host: 127.0.0.1}\n" +
		"    nyc-one:   {host: 1.1.1.1, vantages: [nyc]}\n"
	if err := os.WriteFile(filepath.Join(dir, "default.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	rt, err := buildRuntime(dir, 5, time.Second, time.Second, false, nil, nil)
	if err != nil {
		t.Fatalf("buildRuntime: %v", err)
	}
	// The hub builds a local job only for the local target...
	if got := len(rt.jobs); got != 1 {
		t.Fatalf("local jobs=%d want 1", got)
	}
	// ...but retains the full set so it can serve the nyc assignment.
	nyc := federation.AssignmentFor(rt.monitors, "nyc")
	if len(nyc) != 1 || nyc[0].Name != "nyc-one" {
		t.Fatalf("nyc assignment=%+v", nyc)
	}
}

func TestBuildRuntimeMergesDBFragment(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("targets:\n  children:\n    yaml-t: {probe: TCPConnect, host: 127.0.0.1, params: {port: \"80\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	getter := func() ([]byte, error) {
		return []byte(`{"targets":{"children":{"db-t":{"probe":"TCPConnect","host":"127.0.0.1","params":{"port":"80"}}}}}`), nil
	}
	rt, err := buildRuntime(cfgPath, 1, time.Second, time.Second, false, map[string]alert.Notifier{}, getter)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, m := range rt.monitors {
		names[m.Name] = true
	}
	if !names["yaml-t"] || !names["db-t"] {
		t.Fatalf("want both yaml-t and db-t in monitors, got %v", names)
	}
}

// TestBuildRuntimeSnapshotsEffectiveConfig checks that the runtime carries a JSON snapshot of the
// merged file+DB config (the Config tab's "effective" view). It must be the *merged* config and a
// valid, reloadable config — so we re-load it through the project's own loader (JSON is a YAML
// subset) and confirm both the YAML-defined and the DB-fragment target survive the round trip.
func TestBuildRuntimeSnapshotsEffectiveConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("targets:\n  children:\n    yaml-t: {probe: TCPConnect, host: 127.0.0.1, params: {port: \"80\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	getter := func() ([]byte, error) {
		return []byte(`{"targets":{"children":{"db-t":{"probe":"TCPConnect","host":"127.0.0.1","params":{"port":"80"}}}}}`), nil
	}
	rt, err := buildRuntime(cfgPath, 1, time.Second, time.Second, false, map[string]alert.Notifier{}, getter)
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.effectiveJSON) == 0 {
		t.Fatal("effectiveJSON snapshot is empty")
	}
	outPath := filepath.Join(dir, "effective.yaml")
	if err := os.WriteFile(outPath, rt.effectiveJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	reloaded, err := config.LoadPath(outPath)
	if err != nil {
		t.Fatalf("effective config snapshot did not re-load: %v\n---\n%s", err, rt.effectiveJSON)
	}
	mons, err := reloaded.Monitors()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, m := range mons {
		names[m.Name] = true
	}
	if !names["yaml-t"] || !names["db-t"] {
		t.Fatalf("effective config missing a merged target, got %v\n---\n%s", names, rt.effectiveJSON)
	}
}

// TestJSONToYAMLRendersConfig checks the shared render path: a config document stored as JSON comes
// back as clean YAML — lowercase keys mirroring the file form, durations as "60s" (not nanoseconds),
// and no dropped content.
func TestJSONToYAMLRendersConfig(t *testing.T) {
	y, err := jsonToYAML([]byte(`{"database":{"step":"60s","pings":3},"targets":{"children":{"cf":{"probe":"HTTP","host":"cloudflare.com"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	s := string(y)
	for _, want := range []string{"database:", "step: 60s", "pings: 3", "cf:", "probe: HTTP", "host: cloudflare.com"} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered YAML missing %q\n---\n%s", want, s)
		}
	}
	if strings.Contains(s, "60000000000") {
		t.Errorf("duration rendered as nanoseconds, not \"60s\"\n---\n%s", s)
	}
}

// TestJSONToYAMLStripsNilKeepsExplicitEmpty checks that an unset/inherited field (null) is dropped
// from the read-only view, while an explicit empty list ("cleared") survives — preserving the
// honest inherit-vs-explicitly-none distinction.
func TestJSONToYAMLStripsNilKeepsExplicitEmpty(t *testing.T) {
	y, err := jsonToYAML([]byte(`{"targets":{"children":{"a":{"probe":"Ping","host":"x","alerts":null,"vantages":[]}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	s := string(y)
	if strings.Contains(s, "alerts:") || strings.Contains(s, "null") {
		t.Errorf("nil field should be stripped from the view\n---\n%s", s)
	}
	if !strings.Contains(s, "vantages: []") {
		t.Errorf("explicit empty list should be kept as []\n---\n%s", s)
	}
}

func TestJSONToYAMLEmpty(t *testing.T) {
	for _, in := range []string{"", "  ", "null", "{}"} {
		y, err := jsonToYAML([]byte(in))
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if !strings.HasPrefix(string(y), "#") {
			t.Errorf("%q: want an empty-note comment, got %q", in, y)
		}
	}
}

func TestBuildRuntimeDBFragmentCollision(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("targets:\n  children:\n    dup: {probe: TCPConnect, host: 127.0.0.1, params: {port: \"80\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	getter := func() ([]byte, error) {
		return []byte(`{"targets":{"children":{"dup":{"probe":"TCPConnect","host":"127.0.0.1","params":{"port":"80"}}}}}`), nil
	}
	if _, err := buildRuntime(cfgPath, 1, time.Second, time.Second, false, map[string]alert.Notifier{}, getter); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("want duplicate-branch error, got %v", err)
	}
}

// AppendImport (internal/config) is deliberately context-free: it never sees the real
// default.yaml, so it can't know whether a leaf's probe/params/alerts resolve once inherited.
// buildRuntime is what actually composes the DB fragment with the base config (LoadPath +
// AppendDBFragment) and calls Monitors() — that's where a genuinely-invalid import (one that
// still doesn't resolve even with the base config's tree-wide defaults) must be caught. These
// two cases prove that boundary: AppendImport happily accepts both fragments (added=1, no
// error), and it's buildRuntime — the same composition the API's ConfigImport closure and
// applyConfig trigger — that rejects them.
func TestBuildRuntimeRejectsGenuinelyInvalidImportNotAppendImport(t *testing.T) {
	t.Run("no probe anywhere", func(t *testing.T) {
		dir := t.TempDir()
		// Base config sets no tree-wide probe at all, so a child that also sets none has
		// nothing to inherit.
		if err := os.WriteFile(filepath.Join(dir, "default.yaml"), []byte("targets:\n  children: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		frag := []byte("targets:\n  children:\n    orphan: {host: 127.0.0.1}\n")
		merged, added, _, err := config.AppendImport(nil, frag)
		if err != nil || added != 1 {
			t.Fatalf("AppendImport should accept a fragment relying on (missing) inherited probe: added=%d err=%v", added, err)
		}
		getter := func() ([]byte, error) { return merged, nil }
		if _, err := buildRuntime(dir, 1, time.Second, time.Second, false, nil, getter); err == nil || !strings.Contains(err.Error(), "no probe set") {
			t.Fatalf("buildRuntime should reject the composed config for having no probe anywhere, got %v", err)
		}
	})

	t.Run("undefined alert reference", func(t *testing.T) {
		dir := t.TempDir()
		base := "targets:\n  probe: TCPConnect\nalerts:\n  known: {type: loss, pattern: \">50%\"}\n"
		if err := os.WriteFile(filepath.Join(dir, "default.yaml"), []byte(base), 0o644); err != nil {
			t.Fatal(err)
		}
		frag := []byte("targets:\n  children:\n    orphan: {host: 127.0.0.1, alerts: [nope]}\n")
		merged, added, _, err := config.AppendImport(nil, frag)
		if err != nil || added != 1 {
			t.Fatalf("AppendImport should accept a fragment referencing an alert it can't see: added=%d err=%v", added, err)
		}
		getter := func() ([]byte, error) { return merged, nil }
		if _, err := buildRuntime(dir, 1, time.Second, time.Second, false, nil, getter); err == nil || !strings.Contains(err.Error(), `undefined alert "nope"`) {
			t.Fatalf("buildRuntime should reject the composed config for the undefined alert, got %v", err)
		}
	})

	t.Run("unknown probe kind", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "default.yaml"), []byte("targets:\n  children: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		frag := []byte("targets:\n  children:\n    orphan: {probe: NoSuchProbe, host: 127.0.0.1}\n")
		merged, added, _, err := config.AppendImport(nil, frag)
		if err != nil || added != 1 {
			t.Fatalf("AppendImport should accept an unknown-probe-kind fragment (schema-blind): added=%d err=%v", added, err)
		}
		getter := func() ([]byte, error) { return merged, nil }
		if _, err := buildRuntime(dir, 1, time.Second, time.Second, false, nil, getter); err == nil || !strings.Contains(err.Error(), "unknown probe kind") {
			t.Fatalf("buildRuntime should reject the composed config for the unknown probe kind, got %v", err)
		}
	})
}

type capNotify struct{ n int }

func (c *capNotify) Notify(alert.Event) { c.n++ }

// CODE_REVIEW L2: an alert `to` or target `alertee` recipient with no enabled notifier (a typo, or
// `webhook` without -webhook) must be surfaced up front, not stay invisible until the dropped
// notification. unresolvedRecipients returns exactly the unresolved names, deduped and sorted.
func TestUnresolvedRecipients(t *testing.T) {
	notifiers := map[string]alert.Notifier{"log": alert.LogNotifier{}}
	alertDefs := map[string]*alert.Alert{
		"loss": {Name: "loss", To: []string{"log", "webhook"}},        // webhook not enabled
		"slow": {Name: "slow", To: []string{"log", "pagerduty-typo"}}, // typo
	}
	alerteeByTarget := map[string][]string{
		"t1": {"log"},            // resolved
		"t2": {"webhook", "sms"}, // webhook (dup of the alert's) + sms (missing)
	}
	got := strings.Join(unresolvedRecipients(alertDefs, alerteeByTarget, notifiers), ",")
	if got != "pagerduty-typo,sms,webhook" {
		t.Fatalf("unresolvedRecipients = %q, want sorted/deduped \"pagerduty-typo,sms,webhook\"", got)
	}
	// A fully-resolved config reports nothing.
	if got := unresolvedRecipients(
		map[string]*alert.Alert{"a": {To: []string{"log"}}},
		map[string][]string{"t": {"log"}},
		notifiers,
	); len(got) != 0 {
		t.Fatalf("fully-resolved config must report nothing, got %v", got)
	}
}

// Bug D (CODE_REVIEW #4): a local round that finishes measuring under an obsolete target
// definition (a reload redefined the target between measure and eval) carries a stale
// fingerprint and must be dropped, not evaluated against the new alert identity.
func TestEvalDropsOutcomeWithStaleFingerprint(t *testing.T) {
	cap := &capNotify{}
	eng := alert.NewEngine(
		map[string]*alert.Alert{"loss": {Name: "loss", Matcher: alert.CheckLoss{L: 50, X: 1}, To: []string{"cap"}}},
		map[string]alert.Notifier{"cap": cap},
	)
	rt := &runtime{
		engine:         eng,
		alertsByTarget: map[string][]string{"t": {"loss"}},
		targetFP:       map[string]string{"t": "sha256:current"},
	}
	lost := scheduler.Outcome{Target: probe.Target{Name: "t"}, Computed: sample.Compute(1, nil)} // 100% loss

	stale := lost
	stale.Fingerprint = "sha256:old" // measured under an obsolete definition
	rt.eval([]scheduler.Outcome{stale})
	if cap.n != 0 {
		t.Fatalf("stale-fingerprint outcome must not be evaluated/alerted, got %d events", cap.n)
	}

	cur := lost
	cur.Fingerprint = "sha256:current"
	rt.eval([]scheduler.Outcome{cur})
	if cap.n != 1 {
		t.Fatalf("current-fingerprint outcome must fire, got %d events", cap.n)
	}
}

// CODE_REVIEW M2: a local round measured under an obsolete target identity (a reload redefined the
// target between measure and completion) must be dropped from STORAGE too, not only alerting — the
// remote ingest path already gates storage on the fingerprint before st.Add. storeLocal gates both,
// so a redefined target's in-flight round never lands in history/latest under the new definition.
func TestStoreLocalDropsObsoleteIdentityFromStorage(t *testing.T) {
	cap := &capNotify{}
	eng := alert.NewEngine(
		map[string]*alert.Alert{"loss": {Name: "loss", Matcher: alert.CheckLoss{L: 50, X: 1}, To: []string{"cap"}}},
		map[string]alert.Notifier{"cap": cap},
	)
	rt := &runtime{
		engine:         eng,
		alertsByTarget: map[string][]string{"t": {"loss"}},
		targetFP:       map[string]string{"t": "sha256:current"},
	}
	mem := store.NewMem(16)
	lost := scheduler.Outcome{Target: probe.Target{Name: "t"}, Computed: sample.Compute(1, nil)} // 100% loss

	// Measured under an obsolete definition: neither stored nor alerted.
	stale := lost
	stale.Fingerprint = "sha256:old"
	rt.storeLocal(mem, []scheduler.Outcome{stale})
	if keys, _ := mem.Keys(); len(keys) != 0 {
		t.Fatalf("obsolete-identity round must not be stored, store has keys %v", keys)
	}
	if cap.n != 0 {
		t.Fatalf("obsolete-identity round must not alert, got %d events", cap.n)
	}

	// Measured under the current definition: stored and alerted.
	cur := lost
	cur.Fingerprint = "sha256:current"
	rt.storeLocal(mem, []scheduler.Outcome{cur})
	if keys, _ := mem.Keys(); len(keys) != 1 || keys[0] != "t" {
		t.Fatalf("current-identity round must be stored, keys = %v", keys)
	}
	if cap.n != 1 {
		t.Fatalf("current-identity round must alert, got %d events", cap.n)
	}
}

// CODE_REVIEW M4: the remote ingest handler validates a round against a runtime SNAPSHOT and then
// persists it. A config reload landing in between could redefine (or remove) the target so the
// round lands under the new identity. commitRemote re-checks each outcome against the LIVE runtime
// under evalMu at write time, so a round validated under the old identity is dropped from BOTH
// storage and alerting once a reload has redefined/removed its target — the ingest-path analog of
// storeLocal. This models that reload-between-validation-and-write sequence deterministically: the
// outcome is stamped for the OLD identity (as the handler built it), and the live runtime it commits
// against is the reloaded one.
func TestCommitRemoteDropsReloadRedefinedTarget(t *testing.T) {
	cap := &capNotify{}
	newRT := func(fp string) *runtime {
		eng := alert.NewEngine(
			map[string]*alert.Alert{"loss": {Name: "loss", Matcher: alert.CheckLoss{L: 50, X: 1}, To: []string{"cap"}}},
			map[string]alert.Notifier{"cap": cap},
		)
		return &runtime{
			engine:         eng,
			alertsByTarget: map[string][]string{"t": {"loss"}},
			targetFP:       map[string]string{"t": fp},
		}
	}
	mem := store.NewMem(16)
	ctx := context.Background()
	lost := func(ts time.Time, fp string) scheduler.Outcome { // 100% loss, stamped fp + ts
		return scheduler.Outcome{Target: probe.Target{Name: "t"}, Computed: sample.Compute(1, nil), When: ts, Fingerprint: fp}
	}

	// A reload redefined "t" (new fingerprint) after the handler validated the round under the old
	// snapshot: the live runtime is now rtNew, and the round carries the OLD fingerprint.
	rtNew := newRT("sha256:new")
	stale := lost(time.Unix(1_700_000_000, 0), "sha256:old")
	if ins, err := rtNew.commitRemote(ctx, mem, []scheduler.Outcome{stale}); err != nil || len(ins) != 0 {
		t.Fatalf("reload-redefined round must be dropped, got inserted=%d err=%v", len(ins), err)
	}
	if keys, _ := mem.Keys(); len(keys) != 0 {
		t.Fatalf("reload-redefined round must not be stored, store has keys %v", keys)
	}
	if cap.n != 0 {
		t.Fatalf("reload-redefined round must not alert, got %d events", cap.n)
	}

	// A round measured under the CURRENT identity is stored and alerted.
	cur := lost(time.Unix(1_700_000_060, 0), "sha256:new")
	if ins, err := rtNew.commitRemote(ctx, mem, []scheduler.Outcome{cur}); err != nil || len(ins) != 1 {
		t.Fatalf("current-identity round must be stored, got inserted=%d err=%v", len(ins), err)
	}
	if keys, _ := mem.Keys(); len(keys) != 1 || keys[0] != "t" {
		t.Fatalf("current-identity round must be stored, keys = %v", keys)
	}
	if cap.n != 1 {
		t.Fatalf("current-identity round must alert, got %d events", cap.n)
	}

	// A pre-fingerprint agent is accepted leniently by the handler, but the handler stamps the
	// OLD snapshot's computed identity before calling commitRemote. A reload that redefines the
	// target must therefore drop it too; empty on the wire no longer means empty at this boundary.
	fromLegacyAgent := lost(time.Unix(1_700_000_090, 0), "sha256:old")
	if accepted, err := rtNew.commitRemote(ctx, mem, []scheduler.Outcome{fromLegacyAgent}); err != nil || len(accepted) != 0 {
		t.Fatalf("snapshot-stamped legacy-agent round must be dropped after redefine, accepted=%d err=%v", len(accepted), err)
	}

	// A round for a target a reload REMOVED entirely (absent from targetFP) is dropped by the
	// membership gate before the store write. An EMPTY-fingerprint round exercises that gate in
	// isolation: fingerprintStale never flags an empty fingerprint, so only the membership check can
	// drop it — the distinct timestamp rules out dedup as the reason it isn't stored.
	removedRT := &runtime{engine: rtNew.engine, alertsByTarget: map[string][]string{}, targetFP: map[string]string{}}
	emptyFP := lost(time.Unix(1_700_000_120, 0), "")
	if ins, err := removedRT.commitRemote(ctx, mem, []scheduler.Outcome{emptyFP}); err != nil || len(ins) != 0 {
		t.Fatalf("empty-fp round for a removed target must be dropped by the membership gate, got inserted=%d err=%v", len(ins), err)
	}
	// And a fingerprint-stamped round for the removed target is likewise dropped.
	fresh := lost(time.Unix(1_700_000_180, 0), "sha256:new")
	if ins, err := removedRT.commitRemote(ctx, mem, []scheduler.Outcome{fresh}); err != nil || len(ins) != 0 {
		t.Fatalf("round for a removed target must be dropped, got inserted=%d err=%v", len(ins), err)
	}
	if keys, _ := mem.Keys(); len(keys) != 1 {
		t.Fatalf("removed-target rounds must not be stored, keys = %v", keys)
	}
}

// Bug C (CODE_REVIEW #4): attaching an alert to a previously-unalerted target on reload must
// seed its window from durable history, so an already-breaching target fires immediately
// instead of waiting X fresh rounds. swapRuntime runs the seed before the swap.
func TestSwapRuntimeSeedsNewlyAlertedTarget(t *testing.T) {
	st := store.NewMem(16)
	now := time.Now()
	st.Add([]scheduler.Outcome{{ // one recent breaching round, local vantage
		Target: probe.Target{Name: "t", Host: "h"}, ProbeName: "FPing",
		Computed: sample.Compute(1, nil), When: now.Add(-30 * time.Second),
	}})
	alertDef := map[string]*alert.Alert{"loss": {Name: "loss", Matcher: alert.CheckLoss{L: 50, X: 2}, To: []string{"cap"}}}
	cap := &capNotify{}

	mon := model.Monitor{Name: "t", ID: "t", ProbeKind: "FPing", Host: "h", Pings: 1, Step: time.Minute, Vantages: []string{store.DefaultVantage}}
	old := &runtime{ // target present but NOT alerted yet
		engine:   alert.NewEngine(alertDef, map[string]alert.Notifier{"cap": cap}),
		monitors: []model.Monitor{mon}, alertsByTarget: map[string][]string{}, targetFP: map[string]string{"t": "fp"},
	}
	var current atomic.Pointer[runtime]
	current.Store(old)
	var mu sync.Mutex

	monAlerted := mon
	monAlerted.Alerts = []string{"loss"}
	nrt := &runtime{ // target now carries a 2-sample loss alert
		engine:   alert.NewEngine(alertDef, map[string]alert.Notifier{"cap": cap}),
		monitors: []model.Monitor{monAlerted}, alertsByTarget: map[string][]string{"t": {"loss"}}, targetFP: map[string]string{"t": "fp"},
	}
	seed := func(r *runtime) { warmStartAlerts(context.Background(), r.engine, r.monitors, r.metricByName, st, now) }
	swapRuntime(&current, &mu, nrt, seed)

	// Window seeded with the 1 prior breaching round; one more now meets X=2 → fires immediately.
	current.Load().eval([]scheduler.Outcome{{
		Target: probe.Target{Name: "t", Host: "h"}, ProbeName: "FPing",
		Computed: sample.Compute(1, nil), When: now, Fingerprint: "fp",
	}})
	if cap.n != 1 {
		t.Fatalf("newly-alerted target should fire immediately from seeded history, got %d events", cap.n)
	}
}

// sameTargetIdentity underpins Bug A: only a target whose measurement fingerprint is unchanged
// keeps its inherited window/firing state across a reload. It compares the runtimes' targetFP,
// so any identity-changing field (host, probe, params, pings, probe-config) flips it.
func TestSameTargetIdentity(t *testing.T) {
	old := &runtime{targetFP: map[string]string{
		"keep": "fp1", "rehost": "fpA", "reparam": "fp2", "gone": "fpG",
	}}
	nrt := &runtime{targetFP: map[string]string{
		"keep": "fp1", "rehost": "fpB", "reparam": "fp3", "new": "fpN",
	}}
	same := sameTargetIdentity(old, nrt)
	for name, want := range map[string]bool{"keep": true, "rehost": false, "reparam": false, "new": false} {
		if same[name] != want {
			t.Errorf("sameTargetIdentity[%q] = %v, want %v", name, same[name], want)
		}
	}
	if _, ok := same["gone"]; ok {
		t.Error("removed target must not appear in sameTargetIdentity")
	}
}

func TestSwapRuntimeInstallsAndInherits(t *testing.T) {
	var current atomic.Pointer[runtime]
	var mu sync.Mutex
	old := &runtime{jobs: nil} // nil engine → inheritance guarded off
	current.Store(old)
	nrt := &runtime{}
	swapRuntime(&current, &mu, nrt, nil)
	if current.Load() != nrt {
		t.Fatal("swapRuntime did not install the new runtime")
	}
}

func TestApplyConfigValidatesPersistsSwaps(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run applyConfig test")
	}
	ctx := context.Background()
	cs, err := configstore.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	var current atomic.Pointer[runtime]
	var mu sync.Mutex
	current.Store(&runtime{})

	built := &runtime{jobs: []scheduler.Job{{}}} // sentinel distinct runtime
	goodBuild := func(func() ([]byte, error)) (*runtime, error) { return built, nil }
	badBuild := func(func() ([]byte, error)) (*runtime, error) { return nil, errors.New("boom") }

	// invalid → ErrConfigInvalid, NOT persisted, NOT swapped
	_, verBefore, _ := cs.Get(ctx)
	if err := applyConfig(cs, &current, &mu, badBuild, json.RawMessage(`{}`), verBefore, nil); !errors.Is(err, api.ErrConfigInvalid) {
		t.Fatalf("want ErrConfigInvalid, got %v", err)
	}
	if _, verAfter, _ := cs.Get(ctx); verAfter != verBefore {
		t.Fatalf("invalid doc must not persist: %d -> %d", verBefore, verAfter)
	}
	if current.Load() == built {
		t.Fatal("invalid doc must not swap")
	}

	// valid → persists (version bumps) + swaps
	if err := applyConfig(cs, &current, &mu, goodBuild, json.RawMessage(`{"targets":{"children":{}}}`), verBefore, nil); err != nil {
		t.Fatalf("valid apply: %v", err)
	}
	if _, verAfter, _ := cs.Get(ctx); verAfter != verBefore+1 {
		t.Fatalf("valid doc should bump version %d -> %d", verBefore, verAfter)
	}
	if current.Load() != built {
		t.Fatal("valid doc should swap in the built runtime")
	}

	// stale version → ErrConfigConflict
	if err := applyConfig(cs, &current, &mu, goodBuild, json.RawMessage(`{}`), verBefore, nil); !errors.Is(err, api.ErrConfigConflict) {
		t.Fatalf("want ErrConfigConflict on stale version, got %v", err)
	}
}

func TestValidateRuntimeFlags(t *testing.T) {
	cases := []struct {
		name          string
		pings         int
		step, timeout time.Duration
		wantErr       bool
	}{
		{"valid", 10, 5 * time.Second, 4 * time.Second, false},
		{"pings zero", 0, 5 * time.Second, 4 * time.Second, true},
		{"pings too many", 1 << 20, 5 * time.Second, 4 * time.Second, true},
		{"step too small", 10, time.Millisecond, 4 * time.Second, true},
		{"timeout zero", 10, 5 * time.Second, 0, true},
		{"timeout negative", 10, 5 * time.Second, -time.Second, true},
	}
	for _, c := range cases {
		if err := validateRuntimeFlags(c.pings, c.step, c.timeout); (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v wantErr=%v", c.name, err, c.wantErr)
		}
	}
}

// TestValidateAgentFlags covers Task 11 fix round 1 Finding 1 (-agent-addr without -dsn) and the
// follow-up hardening fix: -agent-addr without -serve must also be rejected, since the listener it
// enables is wired entirely inside the -serve block (nested inside -dsn) in main and would
// otherwise silently never start.
func TestValidateAgentFlags(t *testing.T) {
	cases := []struct {
		name           string
		agentAddr, dsn string
		serve          bool
		wantErr        bool
	}{
		{"neither set", "", "", false, false},
		{"dsn only", "", "postgres://x", false, false},
		{"dsn and serve, no agent-addr", "", "postgres://x", true, false},
		{"all three set", ":8443", "postgres://x", true, false},
		{"agent-addr and dsn without serve", ":8443", "postgres://x", false, true},
		{"agent-addr and serve without dsn", ":8443", "", true, true},
		{"agent-addr alone", ":8443", "", false, true},
	}
	for _, c := range cases {
		if err := validateAgentFlags(c.agentAddr, c.dsn, c.serve); (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v wantErr=%v", c.name, err, c.wantErr)
		}
	}
}

// TestApplyMuSerializesRuntimeSwaps models CODE_REVIEW #1: a slow "SIGHUP" build (A)
// racing an "API apply" (B) that starts later. When both hold applyMu across their whole
// build+swap, A cannot swap a stale runtime AFTER B: B blocks on the lock until A finishes,
// then swaps last, so the later replacement is the live one. Without applyMu around the
// build, A (finishing its build last) would clobber B — the bug.
func TestApplyMuSerializesRuntimeSwaps(t *testing.T) {
	var current atomic.Pointer[runtime]
	var evalMu, applyMu sync.Mutex
	current.Store(&runtime{})
	rtA, rtB := &runtime{}, &runtime{} // distinct runtimes; B (later apply) must win
	reload := func(buildDelay time.Duration, nrt *runtime) {
		applyMu.Lock()
		defer applyMu.Unlock()
		time.Sleep(buildDelay) // simulate build time while holding the lock
		swapRuntime(&current, &evalMu, nrt, nil)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); reload(80*time.Millisecond, rtA) }() // A grabs the lock first, slow build
	time.Sleep(15 * time.Millisecond)
	go func() { defer wg.Done(); reload(0, rtB) }() // B blocks on the lock, swaps after A
	wg.Wait()
	if current.Load() != rtB {
		t.Fatal("the later runtime replacement (B) must be live; a stale swap clobbered it")
	}
}

// TestConfigImportCmd exercises `smoked config import <file>` end to end against a real
// database: a first import adds the file's targets, and a re-import of the identical file
// is a no-op (idempotent) that leaves the stored version unchanged.
func TestConfigImportCmd(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run config import test")
	}
	ctx := context.Background()
	cs, err := configstore.New(ctx, dsn) // also ensures the config_fragment table exists
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	// clean slate: config_fragment is a shared row across this package's DB-gated tests,
	// so reset it deterministically rather than relying on whatever a prior test left
	// behind. configstore has no delete method, so use a raw connection (mirrors
	// configstore_test.go's reset, which reaches its own unexported pool from inside
	// the package).
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `DELETE FROM config_fragment WHERE id = 1`); err != nil {
		conn.Close(ctx)
		t.Fatal(err)
	}
	conn.Close(ctx)
	if _, _, e := cs.Get(ctx); e != nil {
		t.Fatal(e)
	}
	// write a config file
	dir := t.TempDir()
	f := filepath.Join(dir, "in.yaml")
	os.WriteFile(f, []byte("targets:\n  children:\n    imp-a: {probe: TCPConnect, host: 127.0.0.1, params: {port: \"5432\"}}\n"), 0o644)
	// first import adds it
	if rc := configCmd([]string{"import", f, "-dsn", dsn}); rc != 0 {
		t.Fatalf("import rc=%d", rc)
	}
	doc, ver, _ := cs.Get(ctx)
	if ver < 1 || !strings.Contains(string(doc), "imp-a") {
		t.Fatalf("import not persisted: v%d %s", ver, doc)
	}
	// re-import same file -> no change (idempotent), version unchanged
	if rc := configCmd([]string{"import", f, "-dsn", dsn}); rc != 0 {
		t.Fatalf("re-import rc=%d", rc)
	}
	_, ver2, _ := cs.Get(ctx)
	if ver2 != ver {
		t.Fatalf("idempotent re-import bumped version %d -> %d", ver, ver2)
	}
}

// resetConfigFragmentRowMain mirrors resetConfigFragmentRow (import_test.go) using this
// file's already-imported pgx.Connect, so `smoked config import`'s DB-gated tests can reset
// the shared config_fragment row (id=1) between runs without depending on load order across
// the two test files' helpers.
func resetConfigFragmentRowMain(t *testing.T, dsn string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `DELETE FROM config_fragment WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
}

// `smoked config import` merges via config.AppendImport, which (Finding #6) is deliberately
// context-free — it never sees the real base config, so it can't tell whether a leaf's
// probe/params/alerts resolve once inherited. The optional -config DIR effective-validates the
// merged fragment against that base config before persisting (same composition buildRuntime
// performs), giving an operator an immediate reject instead of discovering the break only at
// the daemon's next reload.

// -config given, fragment relies on the base's tree-wide probe: accepted and persisted.
func TestConfigImportCmdConfigFlagAcceptsInheritedFragment(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run config import -config test")
	}
	ctx := context.Background()
	cs, err := configstore.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	resetConfigFragmentRowMain(t, dsn)
	if _, _, e := cs.Get(ctx); e != nil {
		t.Fatal(e)
	}

	baseDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(baseDir, "default.yaml"), []byte("targets:\n  probe: TCPConnect\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "in.yaml")
	// no probe on the target: relies entirely on baseDir's tree-wide probe.
	if err := os.WriteFile(f, []byte("targets:\n  children:\n    ci-inh: {host: 127.0.0.1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rc := configCmd([]string{"import", f, "-dsn", dsn, "-config", baseDir}); rc != 0 {
		t.Fatalf("config import -config on an inheriting fragment: rc=%d, want 0", rc)
	}
	doc, ver, _ := cs.Get(ctx)
	if ver < 1 || !strings.Contains(string(doc), "ci-inh") {
		t.Fatalf("inherited fragment not persisted: v%d %s", ver, doc)
	}
}

// -config given, fragment has no probe anywhere (base sets no tree-wide probe either):
// rejected, non-zero exit, nothing persisted.
func TestConfigImportCmdConfigFlagRejectsInvalid(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run config import -config test")
	}
	ctx := context.Background()
	cs, err := configstore.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	resetConfigFragmentRowMain(t, dsn)
	_, verBefore, e := cs.Get(ctx)
	if e != nil {
		t.Fatal(e)
	}

	baseDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(baseDir, "default.yaml"), []byte("targets:\n  children: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "in.yaml")
	if err := os.WriteFile(f, []byte("targets:\n  children:\n    ci-bad: {host: 127.0.0.1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rc := configCmd([]string{"import", f, "-dsn", dsn, "-config", baseDir})
	if rc == 0 {
		t.Fatal("config import -config should reject a genuinely-invalid fragment, got rc=0")
	}
	doc, verAfter, _ := cs.Get(ctx)
	if verAfter != verBefore || strings.Contains(string(doc), "ci-bad") {
		t.Fatalf("rejected fragment must not persist: v%d -> v%d, doc=%s", verBefore, verAfter, doc)
	}
}

// -config omitted: only AppendImport's context-free checks run, so an inheriting fragment is
// accepted and persisted; a note explains it's validated at the daemon's next reload instead.
func TestConfigImportCmdWithoutConfigFlagAcceptsInheritedFragment(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run config import test")
	}
	ctx := context.Background()
	cs, err := configstore.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	resetConfigFragmentRowMain(t, dsn)
	if _, _, e := cs.Get(ctx); e != nil {
		t.Fatal(e)
	}

	dir := t.TempDir()
	f := filepath.Join(dir, "in.yaml")
	if err := os.WriteFile(f, []byte("targets:\n  children:\n    ci-noconf: {host: 127.0.0.1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var rc int
	out := captureStdout(t, func() { rc = configCmd([]string{"import", f, "-dsn", dsn}) })
	if rc != 0 {
		t.Fatalf("config import without -config should accept a context-free-valid fragment: rc=%d", rc)
	}
	if !strings.Contains(out, "next reload") {
		t.Fatalf("expected a note about daemon-reload validation, got:\n%s", out)
	}
	doc, ver, _ := cs.Get(ctx)
	if ver < 1 || !strings.Contains(string(doc), "ci-noconf") {
		t.Fatalf("fragment not persisted: v%d %s", ver, doc)
	}
}

// TestConfigImportMergeThenApply exercises, against a real database, the exact sequence the
// API's ConfigImport closure (cmd/smoked/main.go) runs: cfgStore.Get -> config.AppendImport ->
// (only if added>0) applyConfig's validate/persist/swap. The closure itself is unexported and
// built inline inside main(), so this drives its three primitives directly — the same approach
// TestApplyConfigValidatesPersistsSwaps takes for ConfigApply's applyConfig call — to prove:
// an import that adds targets persists and swaps in the built runtime; a no-op re-import (added
// == 0) does neither; and a conflicting import (a differing duplicate target) is rejected as
// api.ErrConfigInvalid with the store and live runtime both left untouched.
func TestConfigImportMergeThenApply(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run config import merge/apply test")
	}
	ctx := context.Background()
	cs, err := configstore.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `DELETE FROM config_fragment WHERE id = 1`); err != nil {
		conn.Close(ctx)
		t.Fatal(err)
	}
	conn.Close(ctx)

	var current atomic.Pointer[runtime]
	var evalMu sync.Mutex
	base := &runtime{}
	current.Store(base)
	built := &runtime{jobs: []scheduler.Job{{}}} // sentinel distinct runtime
	build := func(func() ([]byte, error)) (*runtime, error) { return built, nil }

	bodyA := []byte("targets:\n  children:\n    imp-x: {probe: TCPConnect, host: 127.0.0.1, params: {port: \"5432\"}}\n")

	// added > 0 -> merge persists (version bumps) and swaps in the built runtime.
	doc0, ver0, gerr := cs.Get(ctx)
	if gerr != nil {
		t.Fatal(gerr)
	}
	merged, added, unchanged, ierr := config.AppendImport(doc0, bodyA)
	if ierr != nil || added != 1 || unchanged != 0 {
		t.Fatalf("first import: added=%d unchanged=%d err=%v", added, unchanged, ierr)
	}
	if err := applyConfig(cs, &current, &evalMu, build, merged, ver0, nil); err != nil {
		t.Fatalf("apply after import: %v", err)
	}
	doc1, ver1, _ := cs.Get(ctx)
	if ver1 != ver0+1 || !strings.Contains(string(doc1), "imp-x") {
		t.Fatalf("import not persisted: v%d %s", ver1, doc1)
	}
	if current.Load() != built {
		t.Fatal("import with added>0 should swap in the built runtime")
	}

	// added == 0 (identical re-import) -> the ConfigImport closure returns early and never
	// calls applyConfig, so nothing is persisted or swapped again.
	merged2, added2, unchanged2, ierr2 := config.AppendImport(doc1, bodyA)
	if ierr2 != nil || added2 != 0 || unchanged2 != 1 {
		t.Fatalf("re-import: added=%d unchanged=%d err=%v", added2, unchanged2, ierr2)
	}
	_ = merged2 // per the closure, add==0 short-circuits before ever reaching applyConfig
	if _, ver2, _ := cs.Get(ctx); ver2 != ver1 {
		t.Fatalf("no-op re-import must not bump version: %d -> %d", ver1, ver2)
	}

	// a conflicting duplicate (same key, different settings) is rejected by AppendImport
	// itself -> the closure wraps it as api.ErrConfigInvalid, applyConfig is never reached,
	// so the store and live runtime are both untouched.
	bodyConflict := []byte("targets:\n  children:\n    imp-x: {probe: TCPConnect, host: 10.0.0.9, params: {port: \"5432\"}}\n")
	_, _, _, cerr := config.AppendImport(doc1, bodyConflict)
	if cerr == nil {
		t.Fatal("conflicting duplicate target should error")
	}
	wrapped := fmt.Errorf("%w: %v", api.ErrConfigInvalid, cerr) // exactly the wrapping main.go's ConfigImport closure applies
	if !errors.Is(wrapped, api.ErrConfigInvalid) {
		t.Fatalf("wrapped conflict should still be api.ErrConfigInvalid, got %v", wrapped)
	}
	if _, ver3, _ := cs.Get(ctx); ver3 != ver1 {
		t.Fatalf("rejected conflict must not persist: %d -> %d", ver1, ver3)
	}
	if current.Load() != built {
		t.Fatal("rejected conflict must not swap the runtime")
	}
}

// TestVantageAddEmitsOnboardingBundle exercises `smoked vantage add` end to end against a real
// database. Default form renders a ready-to-run agent.yaml (Task 7's RenderAgentYAML) to stdout;
// -json emits the raw PEMs as JSON; -out writes a tar.gz onboarding bundle to a file instead of
// stdout.
func TestVantageAddEmitsOnboardingBundle(t *testing.T) {
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run vantage add test")
	}

	var rc int
	out := captureStdout(t, func() { rc = vantageCmd([]string{"add", "nyc", "-dsn", dsn}) })
	if rc != 0 {
		t.Fatalf("vantage add rc=%d", rc)
	}
	// yaml.v3 only quotes a scalar when required (ambiguous with a number/bool/etc.); a plain
	// name like "nyc" is emitted bare, per internal/vantage/bundle_test.go's own roundtrip test.
	for _, want := range []string{"vantage: nyc", "BEGIN CERTIFICATE", "BEGIN EC PRIVATE KEY"} {
		if !strings.Contains(out, want) {
			t.Errorf("agent.yaml output missing %q\n---\n%s", want, out)
		}
	}

	// -json emits the raw PEMs instead of agent.yaml.
	var rcJSON int
	outJSON := captureStdout(t, func() { rcJSON = vantageCmd([]string{"add", "nyc2", "-dsn", dsn, "-json"}) })
	if rcJSON != 0 {
		t.Fatalf("vantage add -json rc=%d", rcJSON)
	}
	var doc struct {
		Vantage    string `json:"vantage"`
		Hub        string `json:"hub"`
		ClientCert string `json:"client_cert"`
		ClientKey  string `json:"client_key"`
		CACert     string `json:"ca_cert"`
	}
	if err := json.Unmarshal([]byte(outJSON), &doc); err != nil {
		t.Fatalf("vantage add -json: invalid JSON: %v\n---\n%s", err, outJSON)
	}
	if doc.Vantage != "nyc2" {
		t.Errorf("json vantage = %q, want nyc2", doc.Vantage)
	}
	if !strings.Contains(doc.ClientCert, "BEGIN CERTIFICATE") {
		t.Errorf("json client_cert missing BEGIN CERTIFICATE:\n%s", doc.ClientCert)
	}

	// -out writes a tar.gz bundle to a file; stdout stays empty.
	dir := t.TempDir()
	outPath := filepath.Join(dir, "b.tar.gz")
	var rcOut int
	outOut := captureStdout(t, func() {
		rcOut = vantageCmd([]string{"add", "nyc3", "-dsn", dsn, "-out", outPath})
	})
	if rcOut != 0 {
		t.Fatalf("vantage add -out rc=%d", rcOut)
	}
	if strings.TrimSpace(outOut) != "" {
		t.Errorf("vantage add -out should leave stdout empty, got %q", outOut)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("bundle file not written: %v", err)
	}
	if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
		t.Fatalf("bundle file is not gzip (first bytes %x)", data[:min(2, len(data))])
	}
}
