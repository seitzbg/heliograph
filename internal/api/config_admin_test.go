package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// configServer extends adminServer with stub config closures.
func configServer(t *testing.T, apply func(json.RawMessage, int) error, doc json.RawMessage, ver int) (*Server, *http.ServeMux, *http.Cookie) {
	t.Helper()
	srv, _ := adminServer("hunter2")
	srv.ConfigGet = func() (json.RawMessage, int, error) { return doc, ver, nil }
	srv.ConfigApply = apply
	mux := srv.Routes()
	return srv, mux, login(t, mux, "hunter2")
}

func TestGetConfigReturnsVersionAndDoc(t *testing.T) {
	_, mux, cookie := configServer(t, func(json.RawMessage, int) error { return nil }, json.RawMessage(`{"targets":{"children":{}}}`), 3)
	r := httptest.NewRequest("GET", "/api/admin/config", nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("code=%d body=%s", w.Code, w.Body)
	}
	var got struct {
		Version int             `json:"version"`
		Doc     json.RawMessage `json:"doc"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != 3 || !strings.Contains(string(got.Doc), "children") {
		t.Fatalf("got %+v", got)
	}
}

func TestGetConfigUnauthorized(t *testing.T) {
	_, mux, _ := configServer(t, func(json.RawMessage, int) error { return nil }, nil, 0)
	r := httptest.NewRequest("GET", "/api/admin/config", nil) // no cookie
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
}

func TestGetConfigEmptyRowIsEmptyObject(t *testing.T) {
	_, mux, cookie := configServer(t, func(json.RawMessage, int) error { return nil }, nil, 0)
	r := httptest.NewRequest("GET", "/api/admin/config", nil)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var got struct {
		Version int             `json:"version"`
		Doc     json.RawMessage `json:"doc"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Version != 0 || strings.TrimSpace(string(got.Doc)) != "{}" {
		t.Fatalf("empty row want {version:0,doc:{}}, got %+v (%s)", got, got.Doc)
	}
}

func putConfig(t *testing.T, mux *http.ServeMux, cookie *http.Cookie, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("PUT", "/api/admin/config", strings.NewReader(body))
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestPutConfigValid(t *testing.T) {
	var gotDoc string
	var gotVer int
	_, mux, cookie := configServer(t, func(d json.RawMessage, v int) error { gotDoc = string(d); gotVer = v; return nil }, nil, 0)
	w := putConfig(t, mux, cookie, `{"version":2,"doc":{"targets":{"children":{"x":{"probe":"HTTP","host":"a"}}}}}`)
	if w.Code != 200 {
		t.Fatalf("want 200, got %d (%s)", w.Code, w.Body)
	}
	if gotVer != 2 || !strings.Contains(gotDoc, `"x"`) {
		t.Fatalf("ConfigApply got ver=%d doc=%s", gotVer, gotDoc)
	}
	var resp struct {
		Version int `json:"version"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Version != 3 {
		t.Fatalf("want new version 3, got %d", resp.Version)
	}
}

func TestPutConfigInvalidReturns400WithDetail(t *testing.T) {
	_, mux, cookie := configServer(t, func(json.RawMessage, int) error {
		return ErrConfigInvalid // handler wraps/reports its text
	}, nil, 0)
	w := putConfig(t, mux, cookie, `{"version":0,"doc":{"targets":{"children":{}}}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "invalid") {
		t.Fatalf("body should carry the validation detail, got %s", w.Body)
	}
}

func TestPutConfigConflictReturns409(t *testing.T) {
	_, mux, cookie := configServer(t, func(json.RawMessage, int) error { return ErrConfigConflict }, nil, 0)
	w := putConfig(t, mux, cookie, `{"version":1,"doc":{"targets":{"children":{}}}}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d", w.Code)
	}
}

func TestPutConfigMalformedReturns400AndDoesNotApply(t *testing.T) {
	called := false
	_, mux, cookie := configServer(t, func(json.RawMessage, int) error { called = true; return nil }, nil, 0)
	w := putConfig(t, mux, cookie, `not json`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
	if called {
		t.Fatal("ConfigApply must not be called on malformed input")
	}
}
