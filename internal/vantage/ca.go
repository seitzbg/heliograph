package vantage

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
)

// CA is the hub's federation certificate authority: a self-signed ECDSA P-256
// root used (by later tasks) to sign per-vantage client certificates for
// mTLS. It is generated once and persisted in the vantage_ca table, so every
// hub process shares the same root regardless of which process bootstraps it.
type CA struct {
	Cert    *x509.Certificate
	Key     *ecdsa.PrivateKey
	CertPEM []byte
}

// CA returns the hub's federation CA, generating and persisting it on the
// first call. It is idempotent and safe under concurrent bootstrap: a race
// between two callers is resolved by the DB (INSERT ... ON CONFLICT DO
// NOTHING) followed by a re-select, so every caller ends up with the row
// that actually won the race.
func (s *Store) CA(ctx context.Context) (*CA, error) {
	ca, err := s.selectCA(ctx)
	if err == nil {
		return ca, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	certPEM, keyPEM, err := generateCA()
	if err != nil {
		return nil, fmt.Errorf("vantage: generate CA: %w", err)
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO vantage_ca (id, cert_pem, key_pem) VALUES (1, $1, $2) ON CONFLICT (id) DO NOTHING`,
		certPEM, keyPEM); err != nil {
		return nil, fmt.Errorf("vantage: persist CA: %w", err)
	}

	// Re-select rather than trust the pair just generated: another process
	// may have won the insert race, and every caller must converge on the
	// one row that actually persisted.
	return s.selectCA(ctx)
}

func (s *Store) selectCA(ctx context.Context) (*CA, error) {
	var certPEM, keyPEM []byte
	err := s.pool.QueryRow(ctx, `SELECT cert_pem, key_pem FROM vantage_ca WHERE id=1`).Scan(&certPEM, &keyPEM)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("vantage: select CA: %w", err)
	}
	return parseCA(certPEM, keyPEM)
}

// generateCA creates a fresh self-signed ECDSA P-256 CA (20y validity) and
// returns its certificate and key, each PEM-encoded and ready to persist.
func generateCA() (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "heliograph federation CA"},
		NotBefore:             now,
		NotAfter:              now.AddDate(20, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// parseCA parses a persisted (cert_pem, key_pem) pair back into a CA.
func parseCA(certPEM, keyPEM []byte) (*CA, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, fmt.Errorf("vantage: CA cert_pem is not valid PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("vantage: parse CA cert: %w", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("vantage: CA key_pem is not valid PEM")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("vantage: parse CA key: %w", err)
	}

	return &CA{Cert: cert, Key: key, CertPEM: certPEM}, nil
}
