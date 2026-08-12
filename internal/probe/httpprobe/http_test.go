package httpprobe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/seitzbg/heliograph/internal/probe"
)

// insecure_ssl is declared TargetVar; a per-target value must actually take
// effect. Against a self-signed TLS server the default (verify) yields a lost
// round, while per-target insecure_ssl=true lets the probe connect.
func TestHTTPPerTargetInsecureSSL(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "https://") // 127.0.0.1:PORT

	p, err := probe.New("HTTP", nil) // probe-level default: verify (insecure=false)
	if err != nil {
		t.Fatalf("probe.New: %v", err)
	}

	// Default (verify on): the self-signed cert is rejected -> no samples.
	verify := probe.Target{Name: "s", Host: host}
	res, err := p.Measure(context.Background(), verify, 2)
	if err != nil {
		t.Fatalf("Measure(verify): %v", err)
	}
	if len(res.Samples) != 0 {
		t.Errorf("with cert verification a self-signed server should be unreachable, got %d samples", len(res.Samples))
	}

	// Per-target insecure_ssl=true: the probe connects and records samples.
	insecure := probe.Target{Name: "s", Host: host, Params: map[string]string{"insecure_ssl": "true"}}
	res, err = p.Measure(context.Background(), insecure, 2)
	if err != nil {
		t.Fatalf("Measure(insecure): %v", err)
	}
	if len(res.Samples) == 0 {
		t.Errorf("per-target insecure_ssl=true should connect to the self-signed server, got 0 samples")
	}
}

// #12(c): urlformat/method must be validated at config time so a malformed value is a
// loud config error instead of phantom packet loss when request creation fails.
func TestHTTPParamValidators(t *testing.T) {
	for _, ok := range []string{"https://%host%/", "http://%host%/path?q=1", "https://%host%:8443/x"} {
		if err := validURLFormat(ok); err != nil {
			t.Errorf("validURLFormat(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"%host%/no-scheme", "ftp://%host%/", "https:///no-host", "://%host%"} {
		if err := validURLFormat(bad); err == nil {
			t.Errorf("validURLFormat(%q) = nil, want error", bad)
		}
	}
	for _, ok := range []string{"GET", "post", "HEAD", "Delete"} {
		if err := validMethod(ok); err != nil {
			t.Errorf("validMethod(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"FETCH", "", "GET POST", "123"} {
		if err := validMethod(bad); err == nil {
			t.Errorf("validMethod(%q) = nil, want error", bad)
		}
	}
}
