package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
)

func TestApplyPutsAndResets(t *testing.T) {
	var puts int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/admin/login":
			http.SetCookie(w, &http.Cookie{Name: "smoked_admin", Value: "t", Path: "/api/admin"})
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/api/admin/config":
			_ = json.NewEncoder(w).Encode(map[string]any{"version": 4, "doc": json.RawMessage(`{"targets":{"children":{}}}`)})
		case r.Method == http.MethodPut && r.URL.Path == "/api/admin/config":
			atomic.AddInt32(&puts, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"version": 5})
		}
	}))
	st := newStaging()
	_ = st.ensure(context.Background(), c)
	_ = stageAddTarget(st, addTargetIn{GroupPath: "S", Name: "x", Host: "example.com", Probe: "Ping"})
	v, err := applyStaged(context.Background(), c, st)
	if err != nil || v != 5 {
		t.Fatalf("applyStaged: v=%d err=%v", v, err)
	}
	if atomic.LoadInt32(&puts) != 1 || st.active {
		t.Fatalf("expected one PUT and reset buffer; puts=%d active=%v", puts, st.active)
	}
}

// The safety contract: a locally-invalid staged change never reaches the network.
func TestInvalidStageNeverPuts(t *testing.T) {
	var puts int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			atomic.AddInt32(&puts, 1)
		}
		if r.URL.Path == "/api/admin/login" {
			http.SetCookie(w, &http.Cookie{Name: "smoked_admin", Value: "t", Path: "/api/admin"})
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"version": 1, "doc": json.RawMessage(`{"targets":{"children":{}}}`)})
	}))
	st := newStaging()
	_ = st.ensure(context.Background(), c)
	err := stageAddTarget(st, addTargetIn{GroupPath: "S", Name: "x", Host: "h", Probe: "Ping", Params: map[string]string{"bogus_param": "1"}})
	if err == nil {
		t.Fatal("expected local validation error")
	}
	if atomic.LoadInt32(&puts) != 0 {
		t.Fatalf("invalid stage must not PUT; puts=%d", puts)
	}
}
