package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/seitzbg/heliograph/internal/vantage"
)

type fakeKeys struct {
	added   []string
	revoked []string
}

func (f *fakeKeys) Add(_ context.Context, name string) (string, error) {
	if name == "local" { // mirror the real store: the hub's own vantage is reserved
		return "", vantage.ErrReserved
	}
	f.added = append(f.added, name)
	return "smk_id_" + name, nil
}
func (f *fakeKeys) List(context.Context) ([]vantage.Info, error) {
	return []vantage.Info{{Name: "nyc"}}, nil
}
func (f *fakeKeys) Revoke(_ context.Context, name string) (bool, error) {
	f.revoked = append(f.revoked, name)
	return name == "nyc", nil
}

func adminServer(pass string) (*Server, *fakeKeys) {
	fk := &fakeKeys{}
	srv := New(nil, "")
	srv.Vantages = fk
	srv.AdminPassword = pass
	srv.AdminKey = []byte("test-key-test-key-test-key-test1")
	return srv, fk
}

func login(t *testing.T, mux *http.ServeMux, pass string) *http.Cookie {
	t.Helper()
	body := strings.NewReader(`{"password":"` + pass + `"}`)
	r := httptest.NewRequest("POST", "/api/admin/login", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("login code = %d, want 204", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "smoked_admin" {
			return c
		}
	}
	t.Fatal("no smoked_admin cookie set")
	return nil
}

func TestAddVantageRejectsInvalidNameAndSetsNoStore(t *testing.T) {
	srv, _ := adminServer("hunter2")
	mux := srv.Routes()
	cookie := login(t, mux, "hunter2")

	// invalid name -> 400 before the store is touched
	r := httptest.NewRequest("POST", "/api/admin/vantages", strings.NewReader(`{"name":"bad name"}`))
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid vantage name = %d, want 400", w.Code)
	}

	// valid name -> 200 with Cache-Control: no-store (the one-time key is in the body)
	r = httptest.NewRequest("POST", "/api/admin/vantages", strings.NewReader(`{"name":"nyc"}`))
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("valid add = %d, want 200", w.Code)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("add response Cache-Control = %q, want no-store", w.Header().Get("Cache-Control"))
	}

	// the login response is also no-store
	r = httptest.NewRequest("POST", "/api/admin/login", strings.NewReader(`{"password":"hunter2"}`))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("login response Cache-Control = %q, want no-store", w.Header().Get("Cache-Control"))
	}
}

// CODE_REVIEW L5: minting the reserved "local" vantage must return a clear client error (409),
// not a generic 503 "store unavailable" that misrepresents an operator input mistake as an outage.
func TestAddVantageReservedNameIsConflict(t *testing.T) {
	srv, fk := adminServer("hunter2")
	mux := srv.Routes()
	cookie := login(t, mux, "hunter2")
	r := httptest.NewRequest("POST", "/api/admin/vantages", strings.NewReader(`{"name":"local"}`))
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("reserved-name add = %d, want 409", w.Code)
	}
	if len(fk.added) != 0 {
		t.Fatalf("reserved name must not be recorded as added, got %v", fk.added)
	}
}

func TestAdminDisabledWhenNoPassword(t *testing.T) {
	srv, _ := adminServer("") // fail-closed
	mux := srv.Routes()
	for _, path := range []string{"/api/admin/login", "/api/admin/vantages"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s with admin disabled = %d, want 404", path, w.Code)
		}
	}
}

func TestAdminDisabledWhenNoSigningKey(t *testing.T) {
	srv, _ := adminServer("hunter2")
	srv.AdminKey = nil // password + store set, but no signing key -> must NOT register
	mux := srv.Routes()
	r := httptest.NewRequest("POST", "/api/admin/login", strings.NewReader(`{"password":"hunter2"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("login with no signing key = %d, want 404 (fail-closed)", w.Code)
	}
}

func TestAdminSessionAndLogout(t *testing.T) {
	srv, _ := adminServer("hunter2")
	mux := srv.Routes()

	// session probe without a cookie -> 401 (drives the top bar's "Log in" state)
	r := httptest.NewRequest("GET", "/api/admin/session", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("session (no cookie) = %d, want 401", w.Code)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("unauthorized session response Cache-Control = %q, want no-store", w.Header().Get("Cache-Control"))
	}

	cookie := login(t, mux, "hunter2")

	// session probe with a valid cookie -> 204 ("Admin · Log out" state)
	r = httptest.NewRequest("GET", "/api/admin/session", nil)
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("session (cookie) = %d, want 204", w.Code)
	}

	// logout -> 204 and a Set-Cookie that clears the session (HttpOnly cookie can't be
	// cleared from JS, so the client relies on this).
	r = httptest.NewRequest("POST", "/api/admin/logout", nil)
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("logout = %d, want 204", w.Code)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("logout response Cache-Control = %q, want no-store", w.Header().Get("Cache-Control"))
	}
	var cleared *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "smoked_admin" {
			cleared = c
		}
	}
	if cleared == nil || cleared.MaxAge >= 0 {
		t.Fatalf("logout must send a clearing smoked_admin cookie (MaxAge<0), got %+v", cleared)
	}
}

func TestAdminLoginAndCRUD(t *testing.T) {
	srv, fk := adminServer("hunter2")
	mux := srv.Routes()

	// wrong password
	r := httptest.NewRequest("POST", "/api/admin/login", strings.NewReader(`{"password":"nope"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad login = %d, want 401", w.Code)
	}

	// no cookie -> 401 on a protected endpoint
	r = httptest.NewRequest("GET", "/api/admin/vantages", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauth list = %d, want 401", w.Code)
	}

	cookie := login(t, mux, "hunter2")

	// list with cookie
	r = httptest.NewRequest("GET", "/api/admin/vantages", nil)
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200", w.Code)
	}

	// add
	r = httptest.NewRequest("POST", "/api/admin/vantages", strings.NewReader(`{"name":"lon"}`))
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("add = %d, want 200", w.Code)
	}
	var addResp map[string]any
	_ = json.NewDecoder(w.Body).Decode(&addResp)
	if addResp["key"] != "smk_id_lon" {
		t.Errorf("add key = %v, want smk_id_lon", addResp["key"])
	}
	if len(fk.added) != 1 || fk.added[0] != "lon" {
		t.Errorf("Add not called with lon: %v", fk.added)
	}

	// revoke via path value
	r = httptest.NewRequest("DELETE", "/api/admin/vantages/nyc", nil)
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("revoke = %d, want 200", w.Code)
	}
	if len(fk.revoked) != 1 || fk.revoked[0] != "nyc" {
		t.Errorf("Revoke not called with nyc: %v", fk.revoked)
	}
}

func TestListVantagesTargetCount(t *testing.T) {
	srv, _ := adminServer("hunter2")
	srv.TargetVantages = func() map[string][]string {
		return map[string][]string{"a": {"nyc", "local"}, "b": {"nyc"}}
	}
	mux := srv.Routes()
	cookie := login(t, mux, "hunter2")

	r := httptest.NewRequest("GET", "/api/admin/vantages", nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200", w.Code)
	}
	var resp struct {
		Vantages []map[string]any `json:"vantages"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Vantages) != 1 {
		t.Fatalf("want 1 vantage row, got %d", len(resp.Vantages))
	}
	if got := resp.Vantages[0]["targets"]; got != float64(2) { // JSON numbers decode to float64
		t.Errorf("nyc targets = %v, want 2", got)
	}
}

func TestListVantagesOmitsCountWithoutTargetVantages(t *testing.T) {
	srv, _ := adminServer("hunter2") // TargetVantages left nil
	mux := srv.Routes()
	cookie := login(t, mux, "hunter2")

	r := httptest.NewRequest("GET", "/api/admin/vantages", nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var resp struct {
		Vantages []map[string]any `json:"vantages"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := resp.Vantages[0]["targets"]; present {
		t.Errorf("targets field present with nil TargetVantages; want omitted")
	}
}

func TestListVantagesDeduplicatesVantagesPerTarget(t *testing.T) {
	srv, _ := adminServer("hunter2")
	srv.TargetVantages = func() map[string][]string {
		return map[string][]string{"a": {"nyc", "nyc"}, "b": {"nyc"}}
	}
	mux := srv.Routes()
	cookie := login(t, mux, "hunter2")

	r := httptest.NewRequest("GET", "/api/admin/vantages", nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200", w.Code)
	}
	var resp struct {
		Vantages []map[string]any `json:"vantages"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Vantages) != 1 {
		t.Fatalf("want 1 vantage row, got %d", len(resp.Vantages))
	}
	if got := resp.Vantages[0]["targets"]; got != float64(2) {
		t.Errorf("nyc targets = %v, want 2 (deduped per target)", got)
	}
}

func TestListVantagesZeroCountWhenVantageNotAssigned(t *testing.T) {
	srv, _ := adminServer("hunter2")
	srv.TargetVantages = func() map[string][]string {
		return map[string][]string{"a": {"local"}}
	}
	mux := srv.Routes()
	cookie := login(t, mux, "hunter2")

	r := httptest.NewRequest("GET", "/api/admin/vantages", nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d, want 200", w.Code)
	}
	var resp struct {
		Vantages []map[string]any `json:"vantages"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Vantages) != 1 {
		t.Fatalf("want 1 vantage row, got %d", len(resp.Vantages))
	}
	if got := resp.Vantages[0]["targets"]; got != float64(0) {
		t.Errorf("nyc targets = %v, want 0 (present, not omitted)", got)
	}
	if _, present := resp.Vantages[0]["targets"]; !present {
		t.Errorf("targets field absent when TargetVantages is set; want present with value 0")
	}
}

func TestLoginCookieHonorsSessionTTL(t *testing.T) {
	// Default (unset TTL) -> 12h on both the cookie Max-Age and the signed token.
	srv, _ := adminServer("hunter2")
	c := login(t, srv.Routes(), "hunter2")
	if c.MaxAge != int((12 * time.Hour).Seconds()) {
		t.Errorf("default cookie MaxAge = %d, want %d (12h)", c.MaxAge, int((12 * time.Hour).Seconds()))
	}
	if !verifySession(srv.AdminKey, c.Value, time.Now().Add(12*time.Hour-time.Minute)) {
		t.Error("default token should still be valid just under 12h")
	}
	if verifySession(srv.AdminKey, c.Value, time.Now().Add(12*time.Hour+time.Minute)) {
		t.Error("default token should be expired just over 12h")
	}

	// A configured TTL drives both the Max-Age and the token expiry.
	srv2, _ := adminServer("hunter2")
	srv2.AdminSessionTTL = 24 * time.Hour
	c2 := login(t, srv2.Routes(), "hunter2")
	if c2.MaxAge != int((24 * time.Hour).Seconds()) {
		t.Errorf("configured cookie MaxAge = %d, want %d (24h)", c2.MaxAge, int((24 * time.Hour).Seconds()))
	}
	if !verifySession(srv2.AdminKey, c2.Value, time.Now().Add(24*time.Hour-time.Minute)) {
		t.Error("configured token should still be valid just under 24h")
	}
	if verifySession(srv2.AdminKey, c2.Value, time.Now().Add(24*time.Hour+time.Minute)) {
		t.Error("configured token should be expired just over 24h")
	}
}
