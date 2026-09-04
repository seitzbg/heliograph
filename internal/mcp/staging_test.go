package mcp

import (
	"encoding/json"
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

func keysOf(m map[string]json.RawMessage) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}
