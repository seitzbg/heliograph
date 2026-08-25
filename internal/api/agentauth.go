package api

import (
	"net/http"
)

type agentCtxKey int

const vantageCtxKey agentCtxKey = 0

// vantageFrom returns the authenticated vantage name stored on the request context by the
// agent auth layer; "" if the request carries none. The Bearer-key auth that used to set this
// (requireAgent, removed with the old key-based federation path) is superseded by mTLS auth on
// a dedicated listener (a later task); agentAssignment/agentResults read the identity through
// this same accessor either way, so they need no changes when that lands.
func vantageFrom(r *http.Request) string {
	v, _ := r.Context().Value(vantageCtxKey).(string)
	return v
}
