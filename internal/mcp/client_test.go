package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func newTestClient(t *testing.T, h http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewClient(Config{BaseURL: srv.URL, BasicUser: "u", BasicPass: "p", AdminPass: "secret"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c, srv
}

func TestGetJSONSendsBasicAuth(t *testing.T) {
	var gotUser, gotPass string
	var ok bool
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, ok = r.BasicAuth()
		_ = json.NewEncoder(w).Encode(map[string]any{"targets": []any{}})
	}))
	var out struct {
		Targets []any `json:"targets"`
	}
	if err := c.getJSON(context.Background(), "/api/targets", nil, &out); err != nil {
		t.Fatalf("getJSON: %v", err)
	}
	if !ok || gotUser != "u" || gotPass != "p" {
		t.Fatalf("basic auth not sent: ok=%v user=%q pass=%q", ok, gotUser, gotPass)
	}
}

func TestPutConfigLogsInAndReusesCookie(t *testing.T) {
	logins := 0
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/admin/login":
			logins++
			http.SetCookie(w, &http.Cookie{Name: "smoked_admin", Value: "tok", Path: "/api/admin"})
			w.WriteHeader(http.StatusNoContent)
		case "/api/admin/config":
			if _, err := r.Cookie("smoked_admin"); err != nil {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"version": 6})
		}
	}))
	if v, err := c.putConfig(context.Background(), json.RawMessage(`{"targets":{}}`), 5); err != nil || v != 6 {
		t.Fatalf("putConfig #1: v=%d err=%v", v, err)
	}
	if _, err := c.putConfig(context.Background(), json.RawMessage(`{"targets":{}}`), 6); err != nil {
		t.Fatalf("putConfig #2: %v", err)
	}
	if logins != 1 {
		t.Fatalf("expected exactly one login, got %d", logins)
	}
}

func TestPutConfigMapsStatusToSentinels(t *testing.T) {
	cases := []struct {
		code int
		body string
		want error
	}{
		{http.StatusBadRequest, `{"error":"probes.http: bad param"}`, ErrConfigInvalid},
		{http.StatusConflict, `{"error":"version conflict"}`, ErrConfigConflict},
	}
	for _, tc := range cases {
		c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/admin/login" {
				http.SetCookie(w, &http.Cookie{Name: "smoked_admin", Value: "tok", Path: "/api/admin"})
				w.WriteHeader(http.StatusNoContent)
				return
			}
			http.Error(w, tc.body, tc.code)
		}))
		_, err := c.putConfig(context.Background(), json.RawMessage(`{"targets":{}}`), 1)
		if err == nil || !errors.Is(err, tc.want) {
			t.Fatalf("code %d: got err %v, want wrap of %v", tc.code, err, tc.want)
		}
	}
}

// TestAdminGETRetriesOnExpiredSession proves getBytes/getConfigDoc recover from an
// admin session cookie the server no longer honors: the client is already marked
// logged in (so getBytes's preemptive login is a no-op) and its cookie jar holds a
// stale token, so the first GET must 401, and do() must re-login once and retry with
// the fresh cookie rather than treating the retry guard (built for the PUT-with-body
// case) as unconditionally false for a body-less GET.
func TestAdminGETRetriesOnExpiredSession(t *testing.T) {
	var logins int
	var currentToken string
	c, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/admin/login":
			logins++
			currentToken = fmt.Sprintf("tok%d", logins)
			http.SetCookie(w, &http.Cookie{Name: "smoked_admin", Value: currentToken, Path: "/api/admin"})
			w.WriteHeader(http.StatusNoContent)
		case "/api/admin/config":
			ck, err := r.Cookie("smoked_admin")
			if err != nil || ck.Value != currentToken {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"version": 3, "doc": json.RawMessage(`{"targets":{}}`)})
		default:
			http.NotFound(w, r)
		}
	}))

	// Simulate a previously-established session that the server has since expired:
	// mark the client logged in (skips getBytes's preemptive login) and seed the
	// jar with a token the server will reject.
	c.loggedIn = true
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	c.http.Jar.SetCookies(u, []*http.Cookie{{Name: "smoked_admin", Value: "stale", Path: "/api/admin"}})

	_, version, err := c.getConfigDoc(context.Background(), "")
	if err != nil {
		t.Fatalf("getConfigDoc: %v", err)
	}
	if version != 3 {
		t.Fatalf("version = %d, want 3", version)
	}
	if logins != 1 {
		t.Fatalf("expected exactly one re-login, got %d", logins)
	}
}
