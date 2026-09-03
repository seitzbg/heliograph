package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/seitzbg/heliograph/internal/vantage"
)

type fakeKeys struct {
	registered []string
	revoked    []string
	issued     []string
}

func (f *fakeKeys) Register(_ context.Context, name string) error {
	if name == "local" { // mirror the real store: the hub's own vantage is reserved
		return vantage.ErrReserved
	}
	f.registered = append(f.registered, name)
	return nil
}
func (f *fakeKeys) List(context.Context) ([]vantage.Info, error) {
	return []vantage.Info{{Name: "nyc"}}, nil
}
func (f *fakeKeys) Revoke(_ context.Context, name string) (bool, error) {
	f.revoked = append(f.revoked, name)
	return name == "nyc", nil
}
func (f *fakeKeys) IsActive(_ context.Context, name string) (bool, error) {
	return name == "nyc", nil // mirrors List(), which only knows about "nyc"
}

// fakeCertPEM/fakeKeyPEM/fakeCAPEM are deterministic stand-ins for a minted mTLS identity —
// recognizable markers, not real PEM, so tests can assert on their presence without paying for
// real key generation.
var (
	fakeCertPEM = []byte("-----BEGIN CERTIFICATE-----\nFAKECERT\n-----END CERTIFICATE-----\n")
	fakeKeyPEM  = []byte("-----BEGIN EC PRIVATE KEY-----\nFAKEKEY\n-----END EC PRIVATE KEY-----\n")
	fakeCAPEM   = []byte("-----BEGIN CERTIFICATE-----\nFAKECA\n-----END CERTIFICATE-----\n")
)

func (f *fakeKeys) IssueClientCert(_ context.Context, name string) (certPEM, keyPEM, caPEM []byte, err error) {
	f.issued = append(f.issued, name)
	return fakeCertPEM, fakeKeyPEM, fakeCAPEM, nil
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

// markSecure marks r as having arrived over the TLS-terminating proxy (X-Forwarded-Proto: https) —
// the transport addVantage requires before it will mint a vantage's private key (see
// TestAddVantageRequiresHTTPS). Production requests always carry this header (nginx/Caddy set it),
// so any test exercising a mint that must get PAST the transport check sets it here.
func markSecure(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https") }

func TestAddVantageRejectsInvalidName(t *testing.T) {
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

	// valid name -> 200 (registers the name AND mints its mTLS client identity in the same
	// response — see TestAddVantageReturnsBundle / TestAddVantageReturnsJSONWithPEMFields for the
	// mint-time PEM payload itself)
	r = httptest.NewRequest("POST", "/api/admin/vantages", strings.NewReader(`{"name":"nyc"}`))
	r.AddCookie(cookie)
	markSecure(r)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("valid add = %d, want 200", w.Code)
	}

	// the login response is still no-store (the session cookie/token is a secret)
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
	markSecure(r) // reach the reserved-name check; the transport gate runs first
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("reserved-name add = %d, want 409", w.Code)
	}
	if len(fk.registered) != 0 {
		t.Fatalf("reserved name must not be recorded as registered, got %v", fk.registered)
	}
	if len(fk.issued) != 0 {
		t.Fatalf("a rejected register must not mint a client cert, got %v", fk.issued)
	}
}

// TestAddVantageReturnsBundle covers the gzip-bundle path (Accept: application/gzip): a
// successful mint streams a downloadable tar.gz — headers set before the body, gzip magic bytes,
// and (unpacked) an agent.yaml carrying the minted PEM material.
func TestAddVantageReturnsBundle(t *testing.T) {
	srv, fk := adminServer("hunter2")
	srv.AgentHubURL = "https://hub.example.test:8443"
	mux := srv.Routes()
	cookie := login(t, mux, "hunter2")

	r := httptest.NewRequest("POST", "/api/admin/vantages", strings.NewReader(`{"name":"nyc"}`))
	r.AddCookie(cookie)
	r.Header.Set("Accept", "application/gzip")
	markSecure(r)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("bundle add = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/gzip" {
		t.Errorf("Content-Type = %q, want application/gzip", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "nyc-vantage.tar.gz") {
		t.Errorf("Content-Disposition = %q, want it to contain nyc-vantage.tar.gz", cd)
	}
	body := w.Body.Bytes()
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		t.Fatalf("body does not start with gzip magic bytes: %x", body[:min(len(body), 8)])
	}
	if len(fk.issued) != 1 || fk.issued[0] != "nyc" {
		t.Fatalf("IssueClientCert not called with nyc: %v", fk.issued)
	}

	// Unpack and confirm agent.yaml made it into the archive with the minted PEM material.
	gr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	tr := tar.NewReader(gr)
	var agentYAML []byte
	found := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		if hdr.Name == "agent.yaml" {
			found = true
			agentYAML, err = io.ReadAll(tr)
			if err != nil {
				t.Fatalf("read agent.yaml: %v", err)
			}
		}
	}
	if !found {
		t.Fatal("bundle has no agent.yaml entry")
	}
	if !bytes.Contains(agentYAML, []byte("FAKECERT")) || !bytes.Contains(agentYAML, []byte("FAKEKEY")) || !bytes.Contains(agentYAML, []byte("FAKECA")) {
		t.Errorf("agent.yaml missing minted PEM material: %s", agentYAML)
	}
	if !bytes.Contains(agentYAML, []byte("hub.example.test")) {
		t.Errorf("agent.yaml missing configured hub URL: %s", agentYAML)
	}
}

// TestAddVantageBundleViaFormatQueryParam covers the ?format=bundle content-negotiation path
// (no Accept header needed).
func TestAddVantageBundleViaFormatQueryParam(t *testing.T) {
	srv, _ := adminServer("hunter2")
	srv.AgentHubURL = "https://hub.example.test:8443" // a bundle mint now requires a listener (M20)
	mux := srv.Routes()
	cookie := login(t, mux, "hunter2")

	r := httptest.NewRequest("POST", "/api/admin/vantages?format=bundle", strings.NewReader(`{"name":"nyc"}`))
	r.AddCookie(cookie)
	markSecure(r)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("bundle add via query param = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/gzip" {
		t.Errorf("Content-Type = %q, want application/gzip", ct)
	}
	body := w.Body.Bytes()
	if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
		t.Fatalf("body does not start with gzip magic bytes: %x", body[:min(len(body), 8)])
	}
}

// TestAddVantageBundleRefusedWithoutListener covers CODE_REVIEW M20: with no agent listener
// (AgentHubURL empty) the bundle's hub URL would only be a placeholder the agent can never reach,
// so a bundle mint must be REFUSED — not handed out — regardless of the client-side gate, and
// before any vantage is registered or a cert issued. The plain JSON path is unaffected (it does not
// claim "ready to run"); only the tar.gz/gzip bundle is refused.
func TestAddVantageBundleRefusedWithoutListener(t *testing.T) {
	for _, tc := range []struct {
		name   string
		accept string
		url    string
	}{
		{"accept gzip", "application/gzip", "/api/admin/vantages"},
		{"format=bundle", "", "/api/admin/vantages?format=bundle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, fk := adminServer("hunter2") // AgentHubURL left empty: no listener
			mux := srv.Routes()
			cookie := login(t, mux, "hunter2")

			r := httptest.NewRequest("POST", tc.url, strings.NewReader(`{"name":"nyc"}`))
			r.AddCookie(cookie)
			markSecure(r) // reach the no-listener check; the transport gate runs first
			if tc.accept != "" {
				r.Header.Set("Accept", tc.accept)
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)

			if w.Code != http.StatusConflict {
				t.Fatalf("bundle mint without a listener = %d, want 409", w.Code)
			}
			if ct := w.Header().Get("Content-Type"); strings.Contains(ct, "gzip") {
				t.Errorf("Content-Type = %q, want a JSON error, not a gzip bundle", ct)
			}
			if body := w.Body.Bytes(); len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b {
				t.Errorf("a gzip bundle was written despite no listener: %x", body[:min(len(body), 8)])
			}
			// Refused before minting: no vantage registered, no cert issued.
			if len(fk.issued) != 0 {
				t.Errorf("IssueClientCert was called (%v); a refused mint must not issue a cert", fk.issued)
			}
			if len(fk.registered) != 0 {
				t.Errorf("Register was called (%v); a refused mint must not register the vantage", fk.registered)
			}
		})
	}
}

// TestAddVantageRequiresHTTPS covers the transport precondition on the ONE endpoint that mints a
// vantage's private key. Both the tar.gz bundle (Accept: application/gzip / ?format=bundle) and the
// JSON fallback carry that key, so a plaintext request must be refused (403) BEFORE anything is
// minted — no cert issued, no vantage registered. smoked serves plain HTTP behind a TLS-terminating
// proxy, so "insecure" here is the default off-box request: a non-loopback peer with no
// X-Forwarded-Proto reporting https.
func TestAddVantageRequiresHTTPS(t *testing.T) {
	for _, tc := range []struct {
		name   string
		accept string
		url    string
	}{
		{"json fallback", "application/json", "/api/admin/vantages"},
		{"accept gzip bundle", "application/gzip", "/api/admin/vantages"},
		{"format=bundle", "", "/api/admin/vantages?format=bundle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, fk := adminServer("hunter2")
			srv.AgentHubURL = "https://hub.example.test:8443" // isolate transport as the sole reason to refuse
			mux := srv.Routes()
			cookie := login(t, mux, "hunter2")

			// httptest's default RemoteAddr (192.0.2.1) is non-loopback and no X-Forwarded-Proto is
			// set: a plaintext request arriving from off-box.
			r := httptest.NewRequest("POST", tc.url, strings.NewReader(`{"name":"nyc"}`))
			r.AddCookie(cookie)
			if tc.accept != "" {
				r.Header.Set("Accept", tc.accept)
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)

			if w.Code != http.StatusForbidden {
				t.Fatalf("plaintext mint = %d, want 403", w.Code)
			}
			if body := w.Body.Bytes(); len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b {
				t.Errorf("a gzip bundle leaked over plaintext: %x", body[:min(len(body), 8)])
			}
			if !strings.Contains(w.Body.String(), "HTTPS") {
				t.Errorf("error body = %q, want it to mention HTTPS", w.Body.String())
			}
			// Refused before minting: no key material was ever generated.
			if len(fk.issued) != 0 {
				t.Errorf("IssueClientCert called (%v); a plaintext-refused mint must not issue a cert", fk.issued)
			}
			if len(fk.registered) != 0 {
				t.Errorf("Register called (%v); a plaintext-refused mint must not register", fk.registered)
			}
		})
	}
}

// TestAddVantageAllowsSecureTransport covers the accepted signals: an https X-Forwarded-Proto from
// the terminating proxy (including a case-variant and a multi-proxy comma list whose leftmost value
// is the original client scheme), direct TLS on smoked itself, and a loopback peer over plain HTTP
// (which never crosses the wire — the local-dev / single-host-without-a-proxy case).
func TestAddVantageAllowsSecureTransport(t *testing.T) {
	for _, tc := range []struct {
		name       string
		xfp        string // X-Forwarded-Proto header value; "" = unset
		remoteAddr string // override peer address; "" = httptest default (non-loopback)
		tls        bool
	}{
		{name: "x-forwarded-proto https", xfp: "https"},
		{name: "x-forwarded-proto HTTPS uppercase", xfp: "HTTPS"},
		{name: "x-forwarded-proto comma list", xfp: "https, http"},
		{name: "direct TLS on smoked", tls: true},
		{name: "loopback ipv4 over http", remoteAddr: "127.0.0.1:5555"},
		{name: "loopback ipv6 over http", remoteAddr: "[::1]:5555"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, fk := adminServer("hunter2")
			srv.AgentHubURL = "https://hub.example.test:8443"
			mux := srv.Routes()
			cookie := login(t, mux, "hunter2")

			r := httptest.NewRequest("POST", "/api/admin/vantages?format=bundle", strings.NewReader(`{"name":"nyc"}`))
			r.AddCookie(cookie)
			if tc.xfp != "" {
				r.Header.Set("X-Forwarded-Proto", tc.xfp)
			}
			if tc.remoteAddr != "" {
				r.RemoteAddr = tc.remoteAddr
			}
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)

			if w.Code != http.StatusOK {
				t.Fatalf("secure mint = %d, want 200", w.Code)
			}
			body := w.Body.Bytes()
			if len(body) < 2 || body[0] != 0x1f || body[1] != 0x8b {
				t.Fatalf("secure bundle body missing gzip magic: %x", body[:min(len(body), 8)])
			}
			if len(fk.issued) != 1 || fk.issued[0] != "nyc" {
				t.Errorf("secure mint issued %v, want exactly [nyc]", fk.issued)
			}
		})
	}
}

// TestAddVantageReturnsJSONWithPEMFields covers the JSON content-negotiation path: mint-time is
// the one moment the admin API is allowed to carry private key material (spec §3b), so the JSON
// response must include the minted cert/key/CA alongside the existing registered:true shape.
func TestAddVantageReturnsJSONWithPEMFields(t *testing.T) {
	srv, _ := adminServer("hunter2")
	srv.AgentHubURL = "https://hub.example.test:8443"
	mux := srv.Routes()
	cookie := login(t, mux, "hunter2")

	r := httptest.NewRequest("POST", "/api/admin/vantages", strings.NewReader(`{"name":"nyc"}`))
	r.AddCookie(cookie)
	r.Header.Set("Accept", "application/json")
	markSecure(r)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("json add = %d, want 200", w.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["name"] != "nyc" {
		t.Errorf("name = %v, want nyc", resp["name"])
	}
	if resp["registered"] != true {
		t.Errorf("registered = %v, want true", resp["registered"])
	}
	if resp["hub"] != "https://hub.example.test:8443" {
		t.Errorf("hub = %v, want https://hub.example.test:8443", resp["hub"])
	}
	cert, _ := resp["client_cert"].(string)
	key, _ := resp["client_key"].(string)
	ca, _ := resp["ca_cert"].(string)
	if !strings.Contains(cert, "FAKECERT") {
		t.Errorf("client_cert = %q, want it to contain FAKECERT", cert)
	}
	if !strings.Contains(key, "FAKEKEY") {
		t.Errorf("client_key = %q, want it to contain FAKEKEY", key)
	}
	if !strings.Contains(ca, "FAKECA") {
		t.Errorf("ca_cert = %q, want it to contain FAKECA", ca)
	}
}

// TestAddVantageDefaultHubPlaceholder covers the unwired-AgentHubURL fallback: when main hasn't
// set srv.AgentHubURL (Task 11), the response still carries a clearly-a-placeholder hub value
// rather than an empty string a user might silently ship in a bundle.
func TestAddVantageDefaultHubPlaceholder(t *testing.T) {
	srv, _ := adminServer("hunter2")
	mux := srv.Routes()
	cookie := login(t, mux, "hunter2")

	r := httptest.NewRequest("POST", "/api/admin/vantages", strings.NewReader(`{"name":"nyc"}`))
	r.AddCookie(cookie)
	r.Header.Set("Accept", "application/json")
	markSecure(r)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["hub"] != "https://HUB-HOSTNAME:8443" {
		t.Errorf("hub = %v, want the placeholder https://HUB-HOSTNAME:8443", resp["hub"])
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

	// no cookie: the vantage LIST is an open read (200), but a WRITE stays admin-gated (401).
	r = httptest.NewRequest("GET", "/api/admin/vantages", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unauth list (open read) = %d, want 200", w.Code)
	}
	r = httptest.NewRequest("POST", "/api/admin/vantages", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauth add (write gated) = %d, want 401", w.Code)
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
	markSecure(r)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("add = %d, want 200", w.Code)
	}
	var addResp map[string]any
	_ = json.NewDecoder(w.Body).Decode(&addResp)
	if addResp["registered"] != true {
		t.Errorf("add registered = %v, want true", addResp["registered"])
	}
	if len(fk.registered) != 1 || fk.registered[0] != "lon" {
		t.Errorf("Register not called with lon: %v", fk.registered)
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

// TestListVantagesReportsFederationReady covers CODE_REVIEW M19: the list response must tell the UI
// whether the hub actually runs an agent listener (AgentHubURL populated). Without one, a minted
// bundle embeds a placeholder hub URL and can never connect, so the UI disables onboarding.
func TestListVantagesReportsFederationReady(t *testing.T) {
	get := func(srv *Server) bool {
		t.Helper()
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
			FederationReady bool `json:"federation_ready"`
		}
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp.FederationReady
	}

	// No agent listener configured -> not ready.
	srv, _ := adminServer("hunter2")
	if get(srv) {
		t.Error("federation_ready = true with empty AgentHubURL; want false")
	}

	// A configured listener -> ready.
	srv, _ = adminServer("hunter2")
	srv.AgentHubURL = "https://hub.example.test:8443"
	if !get(srv) {
		t.Error("federation_ready = false with AgentHubURL set; want true")
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
