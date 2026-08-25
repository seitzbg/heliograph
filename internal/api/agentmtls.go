package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"time"

	"github.com/seitzbg/heliograph/internal/vantage"
)

// AgentTLSConfig builds the *tls.Config for the mTLS agent listener (wired separately, a later
// task): a fresh server leaf certificate issued from the hub's federation CA — SANs from
// hostnames, EKU serverAuth — paired with mutual authentication that requires every connecting
// client to present a certificate verified against that same CA. Together with requireAgent
// (agentauth.go), this is what lets only a vantage holding a CA-issued client cert
// (vantage.Store.IssueClientCert) ever reach an agent route.
//
// The leaf is signed the same way IssueClientCert signs a client cert (internal/vantage/ca.go):
// ECDSA P-256 key, random 128-bit serial, x509.CreateCertificate against ca.Cert/ca.Key. The
// difference is ExtKeyUsageServerAuth in place of ExtKeyUsageClientAuth, and DNSNames/IPAddresses
// populated from hostnames instead of a client CN.
func (srv *Server) AgentTLSConfig(ca *vantage.CA, hostnames []string) (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("api: agent TLS config: generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("api: agent TLS config: generate serial: %w", err)
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "heliograph agent listener"},
		NotBefore:    now,
		NotAfter:     now.AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hostnames {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return nil, fmt.Errorf("api: agent TLS config: create server certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("api: agent TLS config: parse issued server certificate: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM) {
		return nil, fmt.Errorf("api: agent TLS config: append CA cert to client pool: invalid CA PEM")
	}

	return &tls.Config{
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{der},
			PrivateKey:  key,
			Leaf:        leaf,
		}},
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  pool,
		MinVersion: tls.VersionTLS12,
	}, nil
}

// AgentMux returns the mux for the mTLS agent listener (started by a later task, using the
// *tls.Config from AgentTLSConfig): only the two federation agent routes, each wrapped in
// requireAgent so a caller must present a client certificate that verifies against the hub's CA
// and belongs to a currently active, non-revoked vantage. Deliberately separate from Routes()
// (the plain-HTTP dashboard/API mux, which no longer registers these — see the comment in
// api.go's Routes) so an agent route is never reachable without mTLS.
func (srv *Server) AgentMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agent/v1/assignment", srv.requireAgent(srv.agentAssignment))
	mux.HandleFunc("POST /agent/v1/results", srv.requireAgent(srv.agentResults))
	return mux
}
