package mcp

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestConfigGetYAML(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/admin/login" {
			http.SetCookie(w, &http.Cookie{Name: "smoked_admin", Value: "t", Path: "/api/admin"})
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path != "/api/admin/config.yaml" || r.URL.Query().Get("source") != "effective" {
			t.Errorf("wrong request: %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("targets:\n  Websites:\n"))
	}))
	body, _, err := fetchConfig(context.Background(), c, "effective", "yaml")
	if err != nil || !strings.Contains(body, "Websites") {
		t.Fatalf("fetchConfig: body=%q err=%v", body, err)
	}
}
