package vantage

import (
	"context"
	"crypto/x509"
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
