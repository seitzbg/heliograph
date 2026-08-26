package api

import (
	"context"
	"net/http"
)

type agentCtxKey int

const vantageCtxKey agentCtxKey = 0

// vantageFrom returns the authenticated vantage name stored on the request context by the
// agent auth layer; "" if the request carries none. requireAgent (below) is what sets it, from
// the CN of the client's verified mTLS certificate; agentAssignment/agentResults read the
// identity through this same accessor.
func vantageFrom(r *http.Request) string {
	v, _ := r.Context().Value(vantageCtxKey).(string)
	return v
}

// requireAgent authorizes a federation agent by the CommonName of its mTLS client certificate.
// It does not perform the TLS handshake itself: the mTLS listener (wired separately, a later
// task) already requires a CA-signed client cert before a request ever reaches a handler here —
// this layer only authorizes the identity that cert presented, the same shape as requireAdmin
// authorizing an admin session. A CN must belong to a currently active (registered, not
// revoked) vantage; on success it is stamped onto the request context via vantageCtxKey exactly
// as the old Bearer-key auth did, so vantageFrom/agentAssignment/agentResults need no changes.
//
// SAFE ONLY behind a listener whose tls.Config sets ClientAuth: tls.RequireAndVerifyClientCert
// (the config AgentTLSConfig, in agentmtls.go, produces). A weaker ClientAuthType such as
// RequireAnyClientCert still populates r.TLS.PeerCertificates but never verifies the chain
// against the CA, so any self-signed cert bearing an arbitrary CN would sail through this check
// and impersonate any vantage.
func (srv *Server) requireAgent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, `{"error":"client certificate required"}`, http.StatusUnauthorized)
			return
		}
		// Defensive: production only wires the mTLS listener (and thus ever calls requireAgent)
		// once srv.Vantages exists, but don't let a misconfiguration panic a request handler.
		if srv.Vantages == nil {
			http.Error(w, `{"error":"vantage store unavailable"}`, http.StatusInternalServerError)
			return
		}
		cn := r.TLS.PeerCertificates[0].Subject.CommonName
		active, err := srv.Vantages.IsActive(r.Context(), cn)
		if err != nil {
			http.Error(w, `{"error":"store unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		if !active {
			http.Error(w, `{"error":"unknown or revoked vantage"}`, http.StatusForbidden)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), vantageCtxKey, cn)))
	}
}
