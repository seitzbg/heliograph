package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/seitzbg/heliograph/internal/model"
	"github.com/seitzbg/heliograph/internal/vantage"
)

// testCA builds a throwaway self-signed CA, DB-free, so AgentTLSConfig tests don't need
// Postgres — mirrors internal/vantage/ca.go's generateCA (self-signed ECDSA P-256 root).
func testCA(t *testing.T) *vantage.CA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate CA serial: %v", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "test federation CA"},
		NotBefore:             now,
		NotAfter:              now.AddDate(20, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return &vantage.CA{Cert: cert, Key: key, CertPEM: certPEM}
}

// TestAgentTLSConfigServerLeaf covers the shape of the *tls.Config AgentTLSConfig builds: mutual
// auth is required, the issued server leaf carries the requested hostnames as SANs (DNS name and
// IP handled separately per net.ParseIP), and the leaf verifies against the CA that signed it.
func TestAgentTLSConfigServerLeaf(t *testing.T) {
	ca := testCA(t)
	srv := &Server{}

	cfg, err := srv.AgentTLSConfig(ca, []string{"heliograph.bsd-unix.net", "66.23.200.148"})
	if err != nil {
		t.Fatalf("AgentTLSConfig: %v", err)
	}

	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("ClientAuth = %v, want RequireAndVerifyClientCert", cfg.ClientAuth)
	}
	if cfg.ClientCAs == nil {
		t.Fatal("ClientCAs is nil, want a pool containing the CA")
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("len(Certificates) = %d, want 1", len(cfg.Certificates))
	}

	leaf := cfg.Certificates[0].Leaf
	if leaf == nil {
		// Leaf may not always be populated by the assembly path; fall back to parsing the DER.
		parsed, err := x509.ParseCertificate(cfg.Certificates[0].Certificate[0])
		if err != nil {
			t.Fatalf("parse leaf DER: %v", err)
		}
		leaf = parsed
	}

	var gotDNS, gotIP bool
	for _, n := range leaf.DNSNames {
		if n == "heliograph.bsd-unix.net" {
			gotDNS = true
		}
	}
	for _, ip := range leaf.IPAddresses {
		if ip.String() == "66.23.200.148" {
			gotIP = true
		}
	}
	if !gotDNS {
		t.Fatalf("leaf.DNSNames = %v, want to contain heliograph.bsd-unix.net", leaf.DNSNames)
	}
	if !gotIP {
		t.Fatalf("leaf.IPAddresses = %v, want to contain 66.23.200.148", leaf.IPAddresses)
	}

	for _, eku := range leaf.ExtKeyUsage {
		if eku == x509.ExtKeyUsageServerAuth {
			goto hasServerAuth
		}
	}
	t.Fatalf("leaf.ExtKeyUsage = %v, want to contain ExtKeyUsageServerAuth", leaf.ExtKeyUsage)
hasServerAuth:

	// The leaf must verify against the CA that signed it.
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("leaf.Verify against CA: %v", err)
	}
	if err := leaf.CheckSignatureFrom(ca.Cert); err != nil {
		t.Fatalf("leaf.CheckSignatureFrom(ca): %v", err)
	}
}

// TestAgentTLSConfigMinVersion covers that the listener config floors the TLS version — an mTLS
// agent listener carrying vantage measurements must never silently negotiate down to a legacy,
// weaker protocol version.
func TestAgentTLSConfigMinVersion(t *testing.T) {
	ca := testCA(t)
	srv := &Server{}
	cfg, err := srv.AgentTLSConfig(ca, []string{"example.org"})
	if err != nil {
		t.Fatalf("AgentTLSConfig: %v", err)
	}
	if cfg.MinVersion < tls.VersionTLS12 {
		t.Fatalf("MinVersion = %v, want >= TLS 1.2", cfg.MinVersion)
	}
}

// TestAgentMuxRoutesAssignment covers that AgentMux wires GET /agent/v1/assignment through
// requireAgent: with no client cert presented, the route is REACHED (401 from requireAgent) not
// 404 — proving the route exists rather than re-testing requireAgent's auth logic (Task 5 already
// covers that in agentauth_test.go).
func TestAgentMuxRoutesAssignment(t *testing.T) {
	srv := &Server{Vantages: &fakeVantageAdmin{active: map[string]bool{"nyc": true}}}
	mux := srv.AgentMux()

	r := httptest.NewRequest("GET", "/agent/v1/assignment", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code == http.StatusNotFound {
		t.Fatalf("GET /agent/v1/assignment not routed (404); want it reachable (401 with no client cert)")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (requireAgent rejecting a request with no client cert)", w.Code)
	}
}

// TestAgentMuxRoutesResults covers that AgentMux wires POST /agent/v1/results through
// requireAgent, same shape as TestAgentMuxRoutesAssignment.
func TestAgentMuxRoutesResults(t *testing.T) {
	srv := &Server{Vantages: &fakeVantageAdmin{active: map[string]bool{"nyc": true}}}
	mux := srv.AgentMux()

	r := httptest.NewRequest("POST", "/agent/v1/results", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code == http.StatusNotFound {
		t.Fatalf("POST /agent/v1/results not routed (404); want it reachable (401 with no client cert)")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (requireAgent rejecting a request with no client cert)", w.Code)
	}
}

// TestAgentMuxUnknownRouteNotFound covers that AgentMux serves ONLY the two agent routes: an
// unrelated path must not be routed at all (404), unlike the known routes which are reached but
// rejected by requireAgent.
func TestAgentMuxUnknownRouteNotFound(t *testing.T) {
	srv := &Server{Vantages: &fakeVantageAdmin{active: map[string]bool{"nyc": true}}}
	mux := srv.AgentMux()

	r := httptest.NewRequest("GET", "/api/version", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a route AgentMux must not serve", w.Code)
	}
}

// TestAgentMuxWrongMethodNotAllowed covers that the routes are method-specific: POSTing to the
// assignment path (a GET-only route) must not reach requireAgent/agentAssignment.
func TestAgentMuxWrongMethodNotAllowed(t *testing.T) {
	srv := &Server{Vantages: &fakeVantageAdmin{active: map[string]bool{"nyc": true}}}
	mux := srv.AgentMux()

	r := httptest.NewRequest("POST", "/agent/v1/assignment", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405 for POST to a GET-only route", w.Code)
	}
}

// issueTestClientCert signs a client leaf (CN=cn, EKU clientAuth) from ca, mirroring
// vantage.Store.IssueClientCert's issuance shape (internal/vantage/ca.go) but DB-free, and
// returns it as a tls.Certificate ready to present as a client cert.
func issueTestClientCert(t *testing.T, ca *vantage.CA, cn string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate client serial: %v", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    now,
		NotAfter:     now.AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		t.Fatalf("create client cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// TestAgentMuxRoundTrip is a stronger, real mTLS round trip: an httptest server listening with
// AgentTLSConfig's *tls.Config in front of AgentMux, and a client presenting a CA-issued client
// cert (mirroring vantage.Store.IssueClientCert's issuance shape). Confirms the pieces this task
// produces actually compose into a working mTLS request all the way through requireAgent to
// agentAssignment, not just their independent shapes.
func TestAgentMuxRoundTrip(t *testing.T) {
	ca := testCA(t)
	srv := &Server{
		Vantages: &fakeVantageAdmin{active: map[string]bool{"nyc": true}},
		Assignment: func(v string) ([]model.Monitor, map[string]map[string]string, string) {
			return nil, nil, "sha256:v1"
		},
	}

	cfg, err := srv.AgentTLSConfig(ca, []string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("AgentTLSConfig: %v", err)
	}

	ts := httptest.NewUnstartedServer(srv.AgentMux())
	ts.TLS = cfg
	ts.StartTLS()
	defer ts.Close()

	clientCert := issueTestClientCert(t, ca, "nyc")
	rootPool := x509.NewCertPool()
	rootPool.AddCert(ca.Cert)
	client := ts.Client()
	client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      rootPool,
	}

	resp, err := client.Get(ts.URL + "/agent/v1/assignment")
	if err != nil {
		t.Fatalf("GET /agent/v1/assignment over mTLS: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for an active vantage's CA-issued client cert", resp.StatusCode)
	}
}
