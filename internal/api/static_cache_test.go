package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/seitzbg/heliograph/internal/store"
)

// TestStaticAssetsRevalidate guards the fix for stale dashboard JS surviving a
// deploy: the static file handler must send Cache-Control: no-cache so browsers
// revalidate every asset instead of serving a heuristically-fresh cached copy.
// The FileServer's Last-Modified must still drive a cheap 304 on revalidation.
func TestStaticAssetsRevalidate(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "dashboard.js"), []byte("console.log('v2')"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := New(store.NewMem(10), dir)
	h := srv.Routes()

	// First load: 200 with no-cache so the browser must revalidate next time.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/dashboard.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard.js = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
	lastMod := rec.Header().Get("Last-Modified")
	if lastMod == "" {
		t.Fatal("Last-Modified missing — revalidation can't return a 304 without it")
	}

	// Revalidation with the file unchanged returns a cheap 304, not the body.
	rec2 := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/dashboard.js", nil)
	req.Header.Set("If-Modified-Since", lastMod)
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("conditional GET = %d, want 304", rec2.Code)
	}
	if rec2.Body.Len() != 0 {
		t.Errorf("304 response carried a %d-byte body, want empty", rec2.Body.Len())
	}
}
