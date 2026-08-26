package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/seitzbg/heliograph/internal/agentwire"
)

func TestPullAssignment200And304(t *testing.T) {
	const cv = "sha256:v1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The client authenticates via mTLS, not a header — no Authorization is ever sent.
		if r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected Authorization header: %q (client no longer sends one)", r.Header.Get("Authorization"))
		}
		if r.Header.Get("If-None-Match") == cv {
			w.Header().Set("ETag", cv)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", cv)
		_ = json.NewEncoder(w).Encode(agentwire.Assignment{
			Vantage: "nyc", ConfigVersion: cv,
			Targets: []agentwire.AssignmentTarget{{Name: "cf", Probe: "TCPConnect", Host: "127.0.0.1", StepMs: 1000, Pings: 1}},
		})
	}))
	defer srv.Close()
	c := NewClient(srv.URL, nil, 5*time.Second)

	asg, changed, err := c.PullAssignment(context.Background(), "")
	if err != nil || !changed || asg.ConfigVersion != cv || len(asg.Targets) != 1 {
		t.Fatalf("first pull: changed=%v err=%v asg=%+v", changed, err, asg)
	}
	_, changed, err = c.PullAssignment(context.Background(), cv)
	if err != nil || changed {
		t.Fatalf("304 pull: changed=%v err=%v (want changed=false, nil)", changed, err)
	}
}

// TestPullAssignmentNon200IsError covers a generic non-200/304 status (e.g. a hub-side
// rejection unrelated to TLS) still surfaces as an error — the mTLS-specific auth-failure
// cases are covered separately below (TestPullAssignmentMTLS).
func TestPullAssignmentNon200IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer srv.Close()
	c := NewClient(srv.URL, nil, 5*time.Second)
	if _, _, err := c.PullAssignment(context.Background(), ""); err == nil {
		t.Fatal("expected error on 401")
	}
}

// genTestCA builds a throwaway self-signed CA, mirroring internal/api/agentmtls_test.go's
// testCA helper (self-signed ECDSA P-256 root) — duplicated here rather than imported so
// internal/agent (the agent binary side) doesn't take a dependency on internal/vantage (the
// hub-side CA store) just for a test helper.
func genTestCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
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
	return cert, key
}

// issueTestLeaf signs a leaf cert from ca/caKey for the given EKU (serverAuth or
// clientAuth), mirroring vantage.Store's issuance shape (ECDSA P-256, CN + optional IP
// SANs) but DB-free.
func issueTestLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, cn string, eku x509.ExtKeyUsage, ips []net.IP) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate leaf serial: %v", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    now,
		NotAfter:     now.AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{eku},
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("build tls.Certificate: %v", err)
	}
	return cert
}

// TestPullAssignmentMTLS covers the actual authentication mechanism end to end: an
// httptest TLS server requiring (and verifying) a client certificate, exactly like the
// hub's real mTLS agent listener (internal/api.AgentTLSConfig). A client presenting a
// cert issued by the trusted CA succeeds; a client presenting no cert, or a cert issued by
// an untrusted CA, fails the handshake.
func TestPullAssignmentMTLS(t *testing.T) {
	ca, caKey := genTestCA(t)
	caPool := x509.NewCertPool()
	caPool.AddCert(ca)

	serverLeaf := issueTestLeaf(t, ca, caKey, "hub", x509.ExtKeyUsageServerAuth, []net.IP{net.ParseIP("127.0.0.1")})

	const cv = "sha256:v1"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", cv)
		_ = json.NewEncoder(w).Encode(agentwire.Assignment{
			Vantage: "nyc", ConfigVersion: cv,
			Targets: []agentwire.AssignmentTarget{{Name: "cf", Probe: "TCPConnect", Host: "127.0.0.1", StepMs: 1000, Pings: 1}},
		})
	})
	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverLeaf},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
	}
	srv.StartTLS()
	defer srv.Close()

	t.Run("valid client cert succeeds", func(t *testing.T) {
		clientLeaf := issueTestLeaf(t, ca, caKey, "nyc", x509.ExtKeyUsageClientAuth, nil)
		c := NewClient(srv.URL, &tls.Config{
			Certificates: []tls.Certificate{clientLeaf},
			RootCAs:      caPool,
		}, 5*time.Second)

		asg, changed, err := c.PullAssignment(context.Background(), "")
		if err != nil || !changed || asg.ConfigVersion != cv || len(asg.Targets) != 1 {
			t.Fatalf("pull with valid client cert: changed=%v err=%v asg=%+v", changed, err, asg)
		}
	})

	t.Run("no client cert fails handshake", func(t *testing.T) {
		c := NewClient(srv.URL, &tls.Config{RootCAs: caPool}, 5*time.Second)
		if _, _, err := c.PullAssignment(context.Background(), ""); err == nil {
			t.Fatal("expected a TLS handshake error with no client cert presented")
		}
	})

	t.Run("client cert from an untrusted CA fails handshake", func(t *testing.T) {
		otherCA, otherKey := genTestCA(t)
		clientLeaf := issueTestLeaf(t, otherCA, otherKey, "nyc", x509.ExtKeyUsageClientAuth, nil)
		c := NewClient(srv.URL, &tls.Config{
			Certificates: []tls.Certificate{clientLeaf},
			RootCAs:      caPool,
		}, 5*time.Second)
		if _, _, err := c.PullAssignment(context.Background(), ""); err == nil {
			t.Fatal("expected a TLS handshake error with a client cert from an untrusted CA")
		}
	})
}

func TestPushResults(t *testing.T) {
	var got agentwire.ResultsRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(agentwire.ResultsResponse{Accepted: len(got.Results), Dropped: 0})
	}))
	defer srv.Close()
	c := NewClient(srv.URL, nil, 5*time.Second)
	resp, err := c.PushResults(context.Background(), []agentwire.RoundReport{{Target: "cf", TS: "2026-08-07T00:00:00Z", Pings: 1, RTTs: []float64{0.01}}})
	if err != nil || resp.Accepted != 1 {
		t.Fatalf("push: resp=%+v err=%v", resp, err)
	}
	if len(got.Results) != 1 || got.Results[0].Target != "cf" {
		t.Fatalf("hub received %+v", got.Results)
	}
}

// CODE_REVIEW M1: a malformed or inconsistent 2xx from the hub (an empty 200, a 204, HTML from
// a proxy, truncated/garbage JSON, or counters that don't account for the batch) must be a
// TRANSIENT error so the flush loop retains the buffered rounds — never a silent success, which
// would make sendBatch reclaim rounds the hub never acknowledged storing (irreversible loss).
// Only a well-formed 200 whose accepted+dropped exactly covers the batch is a success.
func TestPushResultsRejectsMalformedSuccess(t *testing.T) {
	rounds := []agentwire.RoundReport{{Target: "a"}, {Target: "b"}} // batch of 2
	cases := []struct {
		name    string
		handler http.HandlerFunc
		wantErr bool
	}{
		{"valid 200 all accepted", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(agentwire.ResultsResponse{Accepted: 2, Dropped: 0})
		}, false},
		{"valid 200 split accept/drop", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(agentwire.ResultsResponse{Accepted: 1, Dropped: 1})
		}, false},
		{"204 no body", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }, true},
		{"200 empty body", func(w http.ResponseWriter, r *http.Request) {}, true},
		{"200 malformed json", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("{not json")) }, true},
		{"200 html from a proxy", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html>ok</html>")) }, true},
		{"200 trailing data", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"accepted":2,"dropped":0}{"x":1}`))
		}, true},
		{"200 negative counter", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(agentwire.ResultsResponse{Accepted: -1, Dropped: 3})
		}, true},
		{"200 sum too small", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(agentwire.ResultsResponse{Accepted: 1, Dropped: 0})
		}, true},
		{"200 sum too large", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(agentwire.ResultsResponse{Accepted: 2, Dropped: 1})
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			c := NewClient(srv.URL, nil, 5*time.Second)
			_, err := c.PushResults(context.Background(), rounds)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want error so the batch is retained, got nil (silent commit)")
				}
				// Must be transient, not a permanent drop — the rounds should be retried, not lost.
				var pe *pushError
				if errors.As(err, &pe) && pe.permanent() {
					t.Errorf("a malformed success must be transient (retained), got permanent drop: %v", err)
				}
			} else if err != nil {
				t.Fatalf("want success, got err=%v", err)
			}
		})
	}
}

func TestPushResultsNon2xxErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	c := NewClient(srv.URL, nil, 5*time.Second)
	if _, err := c.PushResults(context.Background(), []agentwire.RoundReport{{Target: "cf"}}); err == nil {
		t.Fatal("expected error on 503 so the caller retries")
	}
}

// CODE_REVIEW #2: PushResults must flag a permanent (4xx the hub will never accept) rejection
// distinctly from a transient (5xx) one, so the flush loop drops vs retries.
func TestPushResultsPermanentVsTransient(t *testing.T) {
	newC := func(code int) *Client {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
		t.Cleanup(srv.Close)
		return NewClient(srv.URL, nil, 5*time.Second)
	}
	perm := func(code int, want bool) {
		_, err := newC(code).PushResults(context.Background(), []agentwire.RoundReport{{Target: "cf"}})
		var pe *pushError
		if !errors.As(err, &pe) {
			t.Fatalf("status %d: expected *pushError, got %v", code, err)
		}
		if pe.permanent() != want {
			t.Errorf("status %d: permanent()=%v want %v", code, pe.permanent(), want)
		}
	}
	perm(http.StatusRequestEntityTooLarge, true) // 413 oversize -> permanent
	perm(http.StatusBadRequest, true)            // 400 malformed -> permanent
	perm(http.StatusServiceUnavailable, false)   // 503 -> transient (retry)
	perm(http.StatusUnauthorized, false)         // 401 -> transient (key may be re-added)
}

// CODE_REVIEW round-9 #1: a 413 must be distinguishable from a 400 specifically as a SIZE
// condition — the agent's flush splits an oversized batch to isolate the unsendable
// round(s), but must drop a malformed (400) batch wholesale, since splitting a batch the
// hub's decoder outright rejected can't make any sub-shape of it more acceptable.
func TestPushErrorOversize(t *testing.T) {
	newC := func(code int) *Client {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
		t.Cleanup(srv.Close)
		return NewClient(srv.URL, nil, 5*time.Second)
	}
	oversize := func(code int, want bool) {
		_, err := newC(code).PushResults(context.Background(), []agentwire.RoundReport{{Target: "cf"}})
		var pe *pushError
		if !errors.As(err, &pe) {
			t.Fatalf("status %d: expected *pushError, got %v", code, err)
		}
		if pe.oversize() != want {
			t.Errorf("status %d: oversize()=%v want %v", code, pe.oversize(), want)
		}
	}
	oversize(http.StatusRequestEntityTooLarge, true) // 413 -> oversize (splittable)
	oversize(http.StatusBadRequest, false)           // 400 -> permanent but NOT oversize (drop whole)
	oversize(http.StatusServiceUnavailable, false)   // 503 -> not even permanent
}
