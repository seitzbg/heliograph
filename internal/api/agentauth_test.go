package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/seitzbg/heliograph/internal/vantage"
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

// fakeVantageAdmin is a local stub of VantageAdmin for requireAgent tests: IsActive reports
// active only for the names in `active` (nil/false = unknown or revoked), or fails with `err`
// when set. Register/List/Revoke are unused no-ops — requireAgent only calls IsActive.
type fakeVantageAdmin struct {
	active map[string]bool
	err    error
}

func (f *fakeVantageAdmin) Register(context.Context, string) error { return nil }
func (f *fakeVantageAdmin) List(context.Context) ([]vantage.Info, error) {
	return nil, nil
}
func (f *fakeVantageAdmin) Revoke(context.Context, string) (bool, error) { return false, nil }
func (f *fakeVantageAdmin) IsActive(_ context.Context, name string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.active[name], nil
}

// certRequest returns a request carrying a synthetic verified peer certificate with the given
// CommonName, the way the mTLS listener's completed handshake would (a later task wires the
// listener itself; requireAgent only ever reads r.TLS.PeerCertificates).
func certRequest(cn string) *http.Request {
	r := httptest.NewRequest("GET", "/agent/v1/assignment", nil)
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{{Subject: pkix.Name{CommonName: cn}}}}
	return r
}

// TestRequireAgentActiveCNRuns covers the happy path: a client cert whose CN is an active
// vantage runs next with that CN stamped onto the context via vantageFrom.
func TestRequireAgentActiveCNRuns(t *testing.T) {
	srv := &Server{Vantages: &fakeVantageAdmin{active: map[string]bool{"nyc": true}}}
	var gotVantage string
	next := func(w http.ResponseWriter, r *http.Request) {
		gotVantage = vantageFrom(r)
		w.WriteHeader(http.StatusOK)
	}
	w := httptest.NewRecorder()
	srv.requireAgent(next)(w, certRequest("nyc"))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", w.Code)
	}
	if gotVantage != "nyc" {
		t.Fatalf("vantageFrom in next = %q, want nyc", gotVantage)
	}
}

// TestRequireAgentInactiveCNForbidden covers a CN that is unknown or revoked: 403, next not run.
func TestRequireAgentInactiveCNForbidden(t *testing.T) {
	srv := &Server{Vantages: &fakeVantageAdmin{active: map[string]bool{"nyc": true}}}
	called := false
	next := func(http.ResponseWriter, *http.Request) { called = true }
	w := httptest.NewRecorder()
	srv.requireAgent(next)(w, certRequest("ghost"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d want 403", w.Code)
	}
	if called {
		t.Fatal("next must not run for an inactive/unknown CN")
	}
}

// TestRequireAgentNoCertUnauthorized covers a request with no TLS state at all (no client cert
// presented): 401, next not run.
func TestRequireAgentNoCertUnauthorized(t *testing.T) {
	srv := &Server{Vantages: &fakeVantageAdmin{active: map[string]bool{"nyc": true}}}
	called := false
	next := func(http.ResponseWriter, *http.Request) { called = true }
	r := httptest.NewRequest("GET", "/agent/v1/assignment", nil) // r.TLS left nil
	w := httptest.NewRecorder()
	srv.requireAgent(next)(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", w.Code)
	}
	if called {
		t.Fatal("next must not run with no client cert")
	}
}

// TestRequireAgentEmptyPeerCertsUnauthorized covers a TLS connection state present but with no
// verified peer certificates (shouldn't happen once the mTLS listener enforces a client cert,
// but requireAgent must not panic indexing an empty slice).
func TestRequireAgentEmptyPeerCertsUnauthorized(t *testing.T) {
	srv := &Server{Vantages: &fakeVantageAdmin{active: map[string]bool{"nyc": true}}}
	called := false
	next := func(http.ResponseWriter, *http.Request) { called = true }
	r := httptest.NewRequest("GET", "/agent/v1/assignment", nil)
	r.TLS = &tls.ConnectionState{}
	w := httptest.NewRecorder()
	srv.requireAgent(next)(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", w.Code)
	}
	if called {
		t.Fatal("next must not run with empty PeerCertificates")
	}
}

// TestRequireAgentStoreErrorIsServiceUnavailable covers IsActive failing (e.g. DB down): the
// request must not be treated as authorized or as a definitive rejection.
func TestRequireAgentStoreErrorIsServiceUnavailable(t *testing.T) {
	srv := &Server{Vantages: &fakeVantageAdmin{err: errors.New("db down")}}
	called := false
	next := func(http.ResponseWriter, *http.Request) { called = true }
	w := httptest.NewRecorder()
	srv.requireAgent(next)(w, certRequest("nyc"))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d want 503", w.Code)
	}
	if called {
		t.Fatal("next must not run on a store error")
	}
}

// TestRequireAgentNilVantagesIsInternalError covers the defensive nil-store guard: production
// only wires the mTLS listener when srv.Vantages exists, but requireAgent must not panic if it
// is ever reached without one.
func TestRequireAgentNilVantagesIsInternalError(t *testing.T) {
	srv := &Server{}
	called := false
	next := func(http.ResponseWriter, *http.Request) { called = true }
	w := httptest.NewRecorder()
	srv.requireAgent(next)(w, certRequest("nyc"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", w.Code)
	}
	if called {
		t.Fatal("next must not run with a nil Vantages store")
	}
}
