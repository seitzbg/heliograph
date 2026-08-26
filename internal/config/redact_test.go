package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactURLFormat(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://%host%", "https://%host%"},                                                // no path: unchanged
		{"https://%host%/", "https://%host%/"},                                              // bare root: unchanged
		{"https://%host%:8443/", "https://%host%:8443/"},                                    // port kept, root path
		{"https://%host%/health", "https://%host%/[redacted]"},                              // path masked (could be a secret)
		{"https://%host%/hooks/SECRET_TOKEN", "https://%host%/[redacted]"},                  // path-embedded credential masked (M11)
		{"https://%host%:8443/hooks/T/abc", "https://%host%:8443/[redacted]"},               // port kept, path masked
		{"https://alice:swordfish@%host%/health?token=s3cr3t", "https://%host%/[redacted]"}, // userinfo + query + path all masked
		{"http://%host%/p?x=1", "http://%host%/[redacted]"},                                 // query stripped, path masked
		{"https://u:p@%host%/", "https://%host%/"},                                          // userinfo stripped, root path kept
		{"https://%host%/a?k=v", "https://%host%/[redacted]"},                               // query stripped, path masked
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
	// Paths can carry a webhook token too, so they are masked, not preserved (CODE_REVIEW M11).
	for _, leak := range []string{"swordfish", "s3cr3t", "alice", "svc:pw", "apikey=abc123", "token=", "/health", "/api"} {
		if strings.Contains(s, leak) {
			t.Fatalf("redacted config still leaks %q:\n%s", leak, s)
		}
	}
	// The scheme/host stay visible so the read-only view still shows what is probed.
	if !strings.Contains(s, "%host%") {
		t.Fatalf("redaction dropped the %%host%% placeholder:\n%s", s)
	}
	if !strings.Contains(s, "/[redacted]") {
		t.Fatalf("redaction did not mask the path:\n%s", s)
	}
}

func TestRedactSecrets_masksPathEmbeddedCredential(t *testing.T) {
	// A webhook-style urlformat carries its secret in the PATH (Discord/Slack/PagerDuty), not the
	// userinfo or query — this is the M11 regression that path-preserving redaction leaked.
	doc := json.RawMessage(`{"targets":{"children":{"hook":{"host":"1.2.3.4","probe":"HTTP",` +
		`"params":{"urlformat":"https://%host%/api/webhooks/123456/SECRET_TOKEN_ABC"}}}}}`)
	out, err := RedactSecrets(doc)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, "SECRET_TOKEN_ABC") || strings.Contains(s, "webhooks") {
		t.Fatalf("path-embedded credential survived redaction:\n%s", s)
	}
	if !strings.Contains(s, "https://%host%/[redacted]") {
		t.Fatalf("expected the masked urlformat:\n%s", s)
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
