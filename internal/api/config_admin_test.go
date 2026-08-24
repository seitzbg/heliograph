package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// errConfigStore stands in for a backing-store failure in the ConfigYAML stub.
var errConfigStore = errors.New("store unavailable")

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

// The config read is open (a read-only view for anyone past the proxy's Basic Auth); a non-admin
// read is redacted (see TestGetConfigRedactsForNonAdminOnly), and applying a change stays admin-gated.
func TestConfigReadOpenWriteGated(t *testing.T) {
	_, mux, _ := configServer(t, func(json.RawMessage, int) error { return nil }, nil, 0)
	// GET without a cookie: open read -> 200.
	r := httptest.NewRequest("GET", "/api/admin/config", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET config (no cookie) = %d, want 200 (open read)", w.Code)
	}
	// PUT without a cookie: write stays gated -> 401.
	r = httptest.NewRequest("PUT", "/api/admin/config", strings.NewReader(`{"version":0,"doc":{}}`))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("PUT config (no cookie) = %d, want 401 (write gated)", w.Code)
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

// A non-admin config read must not disclose a credential embedded in a probe param; a logged-in
// admin gets the real, editable doc so a later save can't persist the mask over the secret (M11).
func TestGetConfigRedactsForNonAdminOnly(t *testing.T) {
	doc := json.RawMessage(`{"targets":{"children":{"x":{"probe":"HTTP","host":"h",` +
		`"params":{"urlformat":"https://u:p@%host%/?token=SECRET"}}}}}`)
	srv, _ := adminServer("hunter2")
	srv.ConfigGet = func() (json.RawMessage, int, error) { return doc, 1, nil }
	srv.ConfigApply = func(json.RawMessage, int) error { return nil }
	redactCalled := 0
	srv.ConfigRedact = func(d json.RawMessage) (json.RawMessage, error) {
		redactCalled++
		return json.RawMessage(`{"redacted":true}`), nil
	}
	mux := srv.Routes()
	cookie := login(t, mux, "hunter2")

	// No cookie: the redactor runs and the secret never reaches the body.
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/config", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("public GET = %d, want 200", w.Code)
	}
	if strings.Contains(w.Body.String(), "SECRET") {
		t.Fatalf("public read leaked the secret: %s", w.Body)
	}
	if !strings.Contains(w.Body.String(), "redacted") {
		t.Fatalf("public read was not redacted: %s", w.Body)
	}
	if redactCalled != 1 {
		t.Fatalf("redactor called %d times for the public read, want 1", redactCalled)
	}

	// With a valid admin cookie: the real doc (needed to edit) is served, the redactor is skipped.
	w = httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/admin/config", nil)
	r.AddCookie(cookie)
	mux.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), "SECRET") {
		t.Fatalf("admin read should see the real doc: %s", w.Body)
	}
	if redactCalled != 1 {
		t.Fatalf("redactor must not run for an admin read (called %d)", redactCalled)
	}
}

// The YAML view mirrors it: a non-admin read asks ConfigYAML to redact, an admin read does not.
func TestGetConfigYAMLRedactsForNonAdminOnly(t *testing.T) {
	srv, _ := adminServer("hunter2")
	srv.ConfigGet = func() (json.RawMessage, int, error) { return nil, 0, nil }
	srv.ConfigApply = func(json.RawMessage, int) error { return nil }
	var lastRedact bool
	srv.ConfigYAML = func(_ string, redact bool) ([]byte, error) { lastRedact = redact; return []byte("targets: {}\n"), nil }
	mux := srv.Routes()
	cookie := login(t, mux, "hunter2")

	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/admin/config.yaml", nil))
	if !lastRedact {
		t.Fatal("public YAML read must request redaction")
	}
	r := httptest.NewRequest("GET", "/api/admin/config.yaml", nil)
	r.AddCookie(cookie)
	mux.ServeHTTP(httptest.NewRecorder(), r)
	if lastRedact {
		t.Fatal("admin YAML read must not redact")
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

func TestPutConfigNullDocRejected(t *testing.T) {
	called := false
	_, mux, cookie := configServer(t, func(json.RawMessage, int) error { called = true; return nil }, nil, 0)
	w := putConfig(t, mux, cookie, `{"version":0,"doc":null}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d (%s)", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "doc required") {
		t.Fatalf("body should report doc required, got %s", w.Body)
	}
	if called {
		t.Fatal("ConfigApply must not be called on a null doc")
	}
}

// TestGetConfigEffectiveSource checks the read-only effective tree source: it returns the merged
// config JSON with readonly:true (and no version), is an open read, and 503s when unwired.
func TestGetConfigEffectiveSource(t *testing.T) {
	srv, _ := adminServer("hunter2")
	srv.ConfigGet = func() (json.RawMessage, int, error) { return json.RawMessage(`{"targets":{"children":{}}}`), 4, nil }
	srv.ConfigApply = func(json.RawMessage, int) error { return nil }
	srv.ConfigEffective = func() (json.RawMessage, error) {
		return json.RawMessage(`{"targets":{"children":{"eff-t":{"probe":"HTTP","host":"x"}}}}`), nil
	}
	mux := srv.Routes()

	// Open read (no cookie) returns the effective doc, readonly, no version.
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/config?source=effective", nil))
	if w.Code != 200 {
		t.Fatalf("effective source = %d, want 200\n%s", w.Code, w.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["readonly"] != true {
		t.Errorf("want readonly:true, got %v", got["readonly"])
	}
	if _, hasVer := got["version"]; hasVer {
		t.Errorf("effective source must not carry an editable version, got %v", got["version"])
	}
	if !strings.Contains(w.Body.String(), "eff-t") {
		t.Errorf("want the effective doc, got %s", w.Body)
	}

	// Default (no source) still returns the editable DB fragment with a version.
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/config", nil))
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["version"] != float64(4) {
		t.Errorf("db source want version 4, got %v", got["version"])
	}
}

// The effective source merges file-defined targets, which can also carry a credential-bearing probe
// param — a non-admin read of it must be redacted too, not just the DB source (CODE_REVIEW M11).
func TestGetConfigEffectiveRedactsForNonAdmin(t *testing.T) {
	srv, _ := adminServer("hunter2")
	srv.ConfigGet = func() (json.RawMessage, int, error) { return nil, 0, nil }
	srv.ConfigApply = func(json.RawMessage, int) error { return nil }
	srv.ConfigEffective = func() (json.RawMessage, error) {
		return json.RawMessage(`{"targets":{"children":{"eff":{"probe":"HTTP","host":"x",` +
			`"params":{"urlformat":"https://u:p@%host%/?token=SECRET"}}}}}`), nil
	}
	redactCalled := 0
	srv.ConfigRedact = func(d json.RawMessage) (json.RawMessage, error) {
		redactCalled++
		return json.RawMessage(`{"redacted":true}`), nil
	}
	mux := srv.Routes()
	cookie := login(t, mux, "hunter2")

	// No cookie: the effective doc is redacted before it reaches the reader.
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/config?source=effective", nil))
	if strings.Contains(w.Body.String(), "SECRET") {
		t.Fatalf("public effective read leaked the secret: %s", w.Body)
	}
	if redactCalled != 1 {
		t.Fatalf("effective redactor called %d times for public read, want 1", redactCalled)
	}

	// Admin cookie: the real effective doc is served (redactor skipped).
	w = httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/admin/config?source=effective", nil)
	r.AddCookie(cookie)
	mux.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), "SECRET") {
		t.Fatalf("admin effective read should see the real doc: %s", w.Body)
	}
	if redactCalled != 1 {
		t.Fatalf("redactor must not run for an admin effective read (called %d)", redactCalled)
	}
}

func TestGetConfigEffectiveUnwiredReturns503(t *testing.T) {
	// ConfigGet/Apply set (route registered) but ConfigEffective nil.
	_, mux, _ := configServer(t, func(json.RawMessage, int) error { return nil }, nil, 0)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/config?source=effective", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("effective source without hook = %d, want 503", w.Code)
	}
}

// yamlConfigServer wires a stub ConfigYAML that echoes the requested source, so the
// handler's source parsing and open-read behavior can be exercised without the collector.
func yamlConfigServer(t *testing.T, fn func(source string, redact bool) ([]byte, error)) (*http.ServeMux, *http.Cookie) {
	t.Helper()
	srv, _ := adminServer("hunter2")
	srv.ConfigGet = func() (json.RawMessage, int, error) { return nil, 0, nil }
	srv.ConfigApply = func(json.RawMessage, int) error { return nil }
	srv.ConfigYAML = fn
	mux := srv.Routes()
	return mux, login(t, mux, "hunter2")
}

func TestGetConfigYAMLSources(t *testing.T) {
	mux, _ := yamlConfigServer(t, func(source string, _ bool) ([]byte, error) {
		return []byte("source: " + source + "\n"), nil
	})
	for _, tc := range []struct{ query, want string }{
		{"", "source: db"},                         // default is db
		{"?source=db", "source: db"},               // explicit db
		{"?source=effective", "source: effective"}, // effective
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/config.yaml"+tc.query, nil))
		if w.Code != 200 {
			t.Fatalf("%q: code=%d body=%s", tc.query, w.Code, w.Body)
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("%q: Content-Type=%q, want text/plain", tc.query, ct)
		}
		if !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("%q: body=%q, want to contain %q", tc.query, w.Body, tc.want)
		}
	}
}

func TestGetConfigYAMLBadSource(t *testing.T) {
	called := false
	mux, _ := yamlConfigServer(t, func(string, bool) ([]byte, error) { called = true; return nil, nil })
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/config.yaml?source=bogus", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad source = %d, want 400", w.Code)
	}
	if called {
		t.Fatal("ConfigYAML must not be called for an invalid source")
	}
}

// The YAML view is an open read like GET /api/admin/config — no admin cookie required.
func TestGetConfigYAMLReadOpen(t *testing.T) {
	mux, _ := yamlConfigServer(t, func(string, bool) ([]byte, error) { return []byte("targets: {}\n"), nil })
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/config.yaml", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET config.yaml (no cookie) = %d, want 200 (open read)", w.Code)
	}
}

func TestGetConfigYAMLErrorReturns503(t *testing.T) {
	mux, _ := yamlConfigServer(t, func(string, bool) ([]byte, error) { return nil, errConfigStore })
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/config.yaml", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("ConfigYAML error = %d, want 503", w.Code)
	}
}

// TestConfigYAMLRouteAbsentWithoutHook confirms the route is not registered when the hook
// is nil, so a build without config CRUD doesn't expose a half-wired endpoint.
func TestConfigYAMLRouteAbsentWithoutHook(t *testing.T) {
	_, mux, _ := configServer(t, func(json.RawMessage, int) error { return nil }, nil, 0) // no ConfigYAML
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/config.yaml", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("config.yaml without hook = %d, want 404", w.Code)
	}
}

func TestImportConfig(t *testing.T) {
	srv, _ := adminServer("hunter2")
	srv.ConfigGet = func() (json.RawMessage, int, error) { return nil, 0, nil }
	srv.ConfigApply = func(json.RawMessage, int) error { return nil }
	var gotBody string
	srv.ConfigImport = func(b []byte) (int, int, int, error) { gotBody = string(b); return 2, 1, 5, nil }
	mux := srv.Routes()
	cookie := login(t, mux, "hunter2")
	r := httptest.NewRequest("POST", "/api/admin/config/import", strings.NewReader("targets:\n  children:\n    a: {probe: HTTP, host: a}\n"))
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(gotBody, "children") {
		t.Fatalf("code=%d body-seen=%q resp=%s", w.Code, gotBody, w.Body)
	}
	var resp struct{ Added, Unchanged, Version int }
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Added != 2 || resp.Unchanged != 1 || resp.Version != 5 {
		t.Fatalf("resp=%+v", resp)
	}
}

func TestImportConfigConflict400(t *testing.T) {
	srv, _ := adminServer("hunter2")
	srv.ConfigGet = func() (json.RawMessage, int, error) { return nil, 0, nil }
	srv.ConfigApply = func(json.RawMessage, int) error { return nil }
	srv.ConfigImport = func([]byte) (int, int, int, error) { return 0, 0, 0, ErrConfigInvalid }
	mux := srv.Routes()
	cookie := login(t, mux, "hunter2")
	r := httptest.NewRequest("POST", "/api/admin/config/import", strings.NewReader("x"))
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}
