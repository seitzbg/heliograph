package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
)

func TestValidateDocRejectsUnknownProbeParam(t *testing.T) {
	// The brief's scenario used probe kind "icmp", but internal/probe has no such
	// registered kind (native ICMP echo is registered as "Ping" — see
	// internal/probe/pingprobe/ping.go's probe.Register("Ping", ...)). Substituted per
	// task-7-brief.md's own allowance ("if icmp is not a registered probe kind ... we'll
	// pick a probe/param that does"): "Ping" is the closest analog (native ICMP, no
	// external binary) and its schema (packetsize/interval_ms/timeout_ms/mode) rejects an
	// unknown target param exactly like the brief intended for icmp.
	bad := json.RawMessage(`{"targets":{"children":{"x":{"host":"1.1.1.1","probe":"Ping","params":{"not_a_real_param":"z"}}}}}`)
	if err := validateDoc(bad); err == nil {
		t.Fatal("expected validation error for unknown Ping param")
	}
	good := json.RawMessage(`{"targets":{"children":{"x":{"host":"1.1.1.1","probe":"Ping"}}}}`)
	if err := validateDoc(good); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestFlattenAndDiff(t *testing.T) {
	base := json.RawMessage(`{"targets":{"children":{"g":{"children":{"a":{"host":"1.1.1.1","probe":"Ping"}}}}}}`)
	work := json.RawMessage(`{"targets":{"children":{"g":{"children":{"b":{"host":"2.2.2.2","probe":"Ping"}}}}}}`)
	fb, err := flatten(base)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fb["g/a"]; !ok {
		t.Fatalf("flatten missing g/a: %v keys", keysOf(fb))
	}
	added, removed, _, err := diffDocs(base, work)
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 1 || added[0] != "g/b" || len(removed) != 1 || removed[0] != "g/a" {
		t.Fatalf("diff wrong: added=%v removed=%v", added, removed)
	}
}

// TestStagingConcurrentAddsAllLand proves staging mutations are atomic under the go-sdk's
// concurrent tool dispatch (jsonrpc2.Async): N goroutines each stage a distinct target
// against one shared staging buffer, and every single one must land in the final working
// doc. A read-modify-write across two separate lock cycles (st.working() then, later,
// st.setDoc()) loses updates here — the last writer to call setDoc clobbers every
// intervening goroutine's addition with the stale doc it read before they committed. Run
// under -race: this doesn't race on any individual field (each access is mutex-guarded) —
// it deterministically loses updates, which is exactly the class of bug a lock-per-whole-
// operation (staging.mutate) fixes and a data race detector alone would not catch.
func TestStagingConcurrentAddsAllLand(t *testing.T) {
	c, st := stagedClient(t, `{"targets":{"children":{}}}`)
	if err := st.ensure(context.Background(), c); err != nil {
		t.Fatal(err)
	}

	const n = 50
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = stageAddTarget(st, addTargetIn{
				GroupPath: "G",
				Name:      fmt.Sprintf("t%02d", i),
				Host:      fmt.Sprintf("10.0.0.%d", i),
				Probe:     "Ping",
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("stageAddTarget(%d): %v", i, err)
		}
	}

	f, err := flatten(st.working())
	if err != nil {
		t.Fatal(err)
	}
	if len(f) != n {
		t.Fatalf("expected %d targets to have landed, got %d: %v", n, len(f), keysOf(f))
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}
