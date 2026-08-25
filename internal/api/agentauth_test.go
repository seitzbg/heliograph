package api

import (
	"net/http/httptest"
	"testing"
)

// TestVantageFromWithoutContext covers a request whose context never had the vantage identity
// attached (e.g. one that never passed through an auth layer): vantageFrom must return "" and
// must not panic on the failed type assertion.
func TestVantageFromWithoutContext(t *testing.T) {
	r := httptest.NewRequest("GET", "/agent/v1/assignment", nil)
	if v := vantageFrom(r); v != "" {
		t.Fatalf("vantageFrom(no-auth request) = %q, want \"\"", v)
	}
}

// TestVantageFromReadsContext covers the happy path: vantageFrom reads back whatever an auth
// layer stamped on the context via vantageCtxKey (requestAs, in agent_test.go, does this for
// every other agent-handler test).
func TestVantageFromReadsContext(t *testing.T) {
	r := requestAs(httptest.NewRequest("GET", "/agent/v1/assignment", nil), "nyc")
	if v := vantageFrom(r); v != "nyc" {
		t.Fatalf("vantageFrom = %q, want nyc", v)
	}
}
