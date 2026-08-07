package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeAuth struct {
	name string
	ok   bool
	err  error
}

func (f fakeAuth) Verify(context.Context, string) (string, bool, error) { return f.name, f.ok, f.err }

func TestRequireAgent(t *testing.T) {
	dummy := func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(vantageFrom(r)))
	}
	cases := []struct {
		name       string
		auth       fakeAuth
		header     string
		wantStatus int
		wantBody   string
	}{
		{"valid", fakeAuth{name: "nyc", ok: true}, "Bearer smk_x_y", 200, "nyc"},
		{"missing header", fakeAuth{ok: true}, "", 401, ""},
		{"not bearer", fakeAuth{ok: true}, "Basic abc", 401, ""},
		{"bad key", fakeAuth{ok: false}, "Bearer smk_x_y", 401, ""},
		{"verify error", fakeAuth{err: errors.New("db down")}, "Bearer smk_x_y", 503, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &Server{VantageAuth: tc.auth}
			r := httptest.NewRequest("GET", "/agent/v1/assignment", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			w := httptest.NewRecorder()
			srv.requireAgent(dummy)(w, r)
			if w.Code != tc.wantStatus {
				t.Fatalf("status=%d want %d", w.Code, tc.wantStatus)
			}
			if tc.wantBody != "" && w.Body.String() != tc.wantBody {
				t.Fatalf("body=%q want %q", w.Body.String(), tc.wantBody)
			}
		})
	}
}
