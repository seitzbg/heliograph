package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
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
// active only for the names in `active` (nil/false = unknown or revoked) regardless of serial, or
// fails with `err` when set. Register/List/Revoke are unused no-ops — requireAgent only calls
// IsActive. Serial binding is exercised against the real store in the mTLS tests below.
type fakeVantageAdmin struct {
	active map[string]bool
	err    error
}

func (f *fakeVantageAdmin) Register(context.Context, string) error { return nil }
func (f *fakeVantageAdmin) List(context.Context) ([]vantage.Info, error) {
	return nil, nil
}
func (f *fakeVantageAdmin) Revoke(context.Context, string) (bool, error) { return false, nil }
func (f *fakeVantageAdmin) IsActive(_ context.Context, name string, _ *big.Int) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.active[name], nil
}
func (f *fakeVantageAdmin) IssueClientCert(context.Context, string) (certPEM, keyPEM, caPEM []byte, err error) {
	return nil, nil, nil, nil
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

// mtlsAgentProbe stands up a real mTLS agent listener (AgentTLSConfig in front of requireAgent)
// backed by the TimescaleDB vantage store, and returns that store plus a function reporting the
// HTTP status a client presenting a given certificate gets. Each call opens a fresh connection,
// so every status reflects a full handshake against the registry's current state.
func mtlsAgentProbe(t *testing.T) (*vantage.Store, func(cert tls.Certificate) int) {
	t.Helper()
	dsn := os.Getenv("SMOKE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SMOKE_TEST_DSN to run the TimescaleDB integration test")
	}
	ctx := context.Background()
	vs, err := vantage.New(ctx, dsn)
	if err != nil {
		t.Fatalf("vantage.New: %v", err)
	}
	t.Cleanup(vs.Close)
	ca, err := vs.CA(ctx)
	if err != nil {
		t.Fatalf("CA: %v", err)
	}
	srv := &Server{Vantages: vs}
	cfg, err := srv.AgentTLSConfig(ca, []string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("AgentTLSConfig: %v", err)
	}
	ts := httptest.NewUnstartedServer(srv.requireAgent(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	ts.TLS = cfg
	ts.StartTLS()
	t.Cleanup(ts.Close)

	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	return vs, func(cert tls.Certificate) int {
		t.Helper()
		tr := &http.Transport{
			TLSClientConfig:   &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: roots},
			DisableKeepAlives: true,
		}
		defer tr.CloseIdleConnections()
		resp, err := (&http.Client{Transport: tr}).Get(ts.URL)
		if err != nil {
			t.Fatalf("GET over mTLS: %v", err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
}

// issueStoreCert mints a client certificate for name through the real store, as `vantage add`
// and the dashboard's Add/Regenerate do.
func issueStoreCert(t *testing.T, vs *vantage.Store, name string) tls.Certificate {
	t.Helper()
	certPEM, keyPEM, _, err := vs.IssueClientCert(context.Background(), name)
	if err != nil {
		t.Fatalf("IssueClientCert(%s): %v", name, err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	return cert
}

// registerForTest registers name, revoking any leftover row from an earlier run first and again
// when the test ends.
func registerForTest(t *testing.T, vs *vantage.Store, name string) {
	t.Helper()
	ctx := context.Background()
	if _, err := vs.Revoke(ctx, name); err != nil {
		t.Fatalf("pre-clean Revoke(%s): %v", name, err)
	}
	if err := vs.Register(ctx, name); err != nil {
		t.Fatalf("Register(%s): %v", name, err)
	}
	t.Cleanup(func() { _, _ = vs.Revoke(context.Background(), name) })
}

// TestRequireAgentRevokeThenReaddRetiresEarlierCerts is the regression test for revoked-name
// reuse: the documented way to retire a credential is to revoke the vantage and add it again.
// Before the fix, re-adding the name re-authorized every certificate ever issued for it, so the
// retired certificate got 200 again. A certificate issued before the revoke must stay rejected,
// and only the one issued by the re-add is accepted.
func TestRequireAgentRevokeThenReaddRetiresEarlierCerts(t *testing.T) {
	vs, status := mtlsAgentProbe(t)
	ctx := context.Background()
	const name = "test-revoke-readd"
	registerForTest(t, vs, name)

	old := issueStoreCert(t, vs, name)
	if got := status(old); got != http.StatusOK {
		t.Fatalf("issued certificate: status %d, want 200", got)
	}
	if _, err := vs.Revoke(ctx, name); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if got := status(old); got != http.StatusForbidden {
		t.Fatalf("certificate of a revoked vantage: status %d, want 403", got)
	}

	if err := vs.Register(ctx, name); err != nil {
		t.Fatalf("re-Register: %v", err)
	}
	fresh := issueStoreCert(t, vs, name)
	if got := status(old); got != http.StatusForbidden {
		t.Errorf("certificate issued before revoke, after re-add: status %d, want 403", got)
	}
	if got := status(fresh); got != http.StatusOK {
		t.Errorf("certificate issued by the re-add: status %d, want 200", got)
	}
}

// TestRequireAgentRegenerateKeepsEarlierCertsValid covers regenerate (a second IssueClientCert
// for a live name, as the dashboard's Regenerate and a repeated `vantage add` do): the new
// certificate works and the one already deployed keeps working.
func TestRequireAgentRegenerateKeepsEarlierCertsValid(t *testing.T) {
	vs, status := mtlsAgentProbe(t)
	const name = "test-regenerate"
	registerForTest(t, vs, name)

	first := issueStoreCert(t, vs, name)
	second := issueStoreCert(t, vs, name)
	if got := status(first); got != http.StatusOK {
		t.Errorf("certificate issued before regenerate: status %d, want 200", got)
	}
	if got := status(second); got != http.StatusOK {
		t.Errorf("regenerated certificate: status %d, want 200", got)
	}
}

// TestRequireAgentRejectsUnissuedCertForRegisteredName covers a certificate that chains to the
// hub's CA and names a registered vantage but was never issued by IssueClientCert for that
// registration: it must be refused, since only issued serials are authorized.
func TestRequireAgentRejectsUnissuedCertForRegisteredName(t *testing.T) {
	vs, status := mtlsAgentProbe(t)
	const name = "test-unissued"
	registerForTest(t, vs, name)

	ca, err := vs.CA(context.Background())
	if err != nil {
		t.Fatalf("CA: %v", err)
	}
	if got := status(issueTestClientCert(t, ca, name)); got != http.StatusForbidden {
		t.Errorf("CA-signed certificate never issued for the registration: status %d, want 403", got)
	}
}
