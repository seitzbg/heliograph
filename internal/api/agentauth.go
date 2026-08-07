package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
)

// VantageAuth verifies an agent's presented API key and returns the vantage it
// authenticates as. *vantage.Store satisfies it; api keeps it an interface so the
// handlers test with a fake and need no live DB.
type VantageAuth interface {
	Verify(ctx context.Context, presented string) (name string, ok bool, err error)
}

type agentCtxKey int

const vantageCtxKey agentCtxKey = 0

// vantageFrom returns the authenticated vantage name requireAgent stored on the
// request context; "" if the request did not pass through requireAgent.
func vantageFrom(r *http.Request) string {
	v, _ := r.Context().Value(vantageCtxKey).(string)
	return v
}

func bearerToken(h string) string {
	const p = "Bearer "
	if len(h) >= len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}

// requireAgent authenticates an agent request by its Bearer API key, storing the
// resolved vantage on the context for the handler. 401 for any absent/malformed/
// unknown/revoked key (no oracle); 503 if the key store is unreachable.
func (srv *Server) requireAgent(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := bearerToken(r.Header.Get("Authorization"))
		if key == "" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		name, ok, err := srv.VantageAuth.Verify(r.Context(), key)
		if err != nil {
			slog.Error("agent auth: verify failed", "err", err)
			http.Error(w, `{"error":"auth unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		if !ok {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), vantageCtxKey, name)))
	}
}
