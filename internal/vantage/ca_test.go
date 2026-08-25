package vantage

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"testing"
)

func TestCA(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	ca1, err := s.CA(ctx)
	if err != nil {
		t.Fatalf("CA (first call): %v", err)
	}
	if !ca1.Cert.IsCA {
		t.Error("CA cert IsCA = false, want true")
	}
	if ca1.Cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("CA cert KeyUsage missing KeyUsageCertSign")
	}

	ca2, err := s.CA(ctx)
	if err != nil {
		t.Fatalf("CA (second call): %v", err)
	}

	if string(ca1.CertPEM) != string(ca2.CertPEM) {
		t.Error("CA() is not idempotent: got a different CertPEM on the second call")
	}
}

func TestIssueClientCert(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)

	certPEM, keyPEM, caPEM, err := s.IssueClientCert(ctx, "nyc")
	if err != nil {
		t.Fatalf("IssueClientCert: %v", err)
	}
	if len(keyPEM) == 0 {
		t.Error("IssueClientCert: keyPEM is empty")
	}

	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("IssueClientCert: certPEM is not valid PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse issued cert: %v", err)
	}

	if cert.Subject.CommonName != "nyc" {
		t.Errorf("Subject.CommonName = %q, want %q", cert.Subject.CommonName, "nyc")
	}
	wantEKU := false
	for _, eku := range cert.ExtKeyUsage {
		if eku == x509.ExtKeyUsageClientAuth {
			wantEKU = true
		}
	}
	if !wantEKU {
		t.Errorf("ExtKeyUsage = %v, want it to contain ExtKeyUsageClientAuth", cert.ExtKeyUsage)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("caPEM did not parse into a cert pool")
	}
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("cert.Verify against caPEM pool: %v", err)
	}
}
