package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactURLFormat(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://%host%/", "https://%host%/"},                                          // clean: unchanged
		{"https://%host%/health", "https://%host%/health"},                              // clean path: unchanged
		{"https://alice:swordfish@%host%/health?token=s3cr3t", "https://%host%/health"}, // userinfo + query stripped
		{"http://%host%/p?x=1", "http://%host%/p"},                                      // query stripped
		{"https://u:p@%host%/", "https://%host%/"},                                      // userinfo stripped
		{"https://%host%/a?k=v", "https://%host%/a"},                                    // query only
	}
	for _, c := range cases {
		if got := redactURLFormat(c.in); got != c.want {
			t.Errorf("redactURLFormat(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRedactSecrets_stripsHTTPCredentials(t *testing.T) {
	// Both a probe-level default and a per-target override can carry a credential-bearing URL.
	doc := json.RawMessage(`{
	  "probes": {"HTTP": {"urlformat": "https://svc:pw@%host%/api?apikey=abc123"}},
	  "targets": {"children": {"web": {"host":"1.2.3.4","probe":"HTTP",
	    "params": {"urlformat": "https://alice:swordfish@%host%/health?token=s3cr3t"}}}}
	}`)
	out, err := RedactSecrets(doc)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, leak := range []string{"swordfish", "s3cr3t", "alice", "svc:pw", "apikey=abc123", "token="} {
		if strings.Contains(s, leak) {
			t.Fatalf("redacted config still leaks %q:\n%s", leak, s)
		}
	}
	// The useful, non-secret parts survive so the read-only view stays meaningful.
	if !strings.Contains(s, "%host%") {
		t.Fatalf("redaction dropped the %%host%% placeholder:\n%s", s)
	}
	if !strings.Contains(s, "/health") {
		t.Fatalf("redaction dropped the path:\n%s", s)
	}
}

func TestRedactSecrets_keepsCleanConfig(t *testing.T) {
	// A urlformat with no credentials, and non-secret params, are untouched.
	doc := json.RawMessage(`{"targets":{"children":{"x":{"host":"h","probe":"HTTP",` +
		`"params":{"urlformat":"https://%host%/","method":"GET"}}}}}`)
	out, err := RedactSecrets(doc)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `"https://%host%/"`) {
		t.Fatalf("clean urlformat should round-trip unchanged:\n%s", s)
	}
	if !strings.Contains(s, `"GET"`) {
		t.Fatalf("non-secret param dropped:\n%s", s)
	}
}

func TestRedactSecrets_emptyAndNull(t *testing.T) {
	for _, in := range []string{``, `  `, `null`, `{}`} {
		if _, err := RedactSecrets(json.RawMessage(in)); err != nil {
			t.Errorf("RedactSecrets(%q) errored: %v", in, err)
		}
	}
}
