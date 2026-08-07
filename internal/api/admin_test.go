package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"smokeping-modern/internal/vantage"
)

type fakeKeys struct {
	added   []string
	revoked []string
}

func (f *fakeKeys) Add(_ context.Context, name string) (string, error) {
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
