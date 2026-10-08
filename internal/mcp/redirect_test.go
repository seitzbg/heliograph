package mcp

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// hostnameTransport returns a transport that trusts the httptest TLS certificate and dials every
// connection to 127.0.0.1 (keeping the port). It lets a test give the hub and the redirect
// targets real-looking, non-loopback hostnames — the case where Go forwards Basic Auth on a
// same-hostname redirect and the client's loopback exemption does not apply — while serving
// them all from local httptest servers.
func hostnameTransport(t *testing.T, ts *httptest.Server) *http.Transport {
	t.Helper()
	tr := ts.Client().Transport.(*http.Transport).Clone()
	tr.TLSClientConfig.ServerName = "127.0.0.1" // the httptest certificate's IP SAN
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
	}
	t.Cleanup(tr.CloseIdleConnections)
	return tr
}

// answerLikeHub answers the way a hub would — 204 to a login POST, a JSON body to a GET — so a
// client that follows the redirect sees success, and only the redirect policy can produce an error.
func answerLikeHub(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	_, _ = w.Write([]byte(`{"targets":[]}`))
}

func portOf(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(ts.URL, "https://"), "http://"))
	if err != nil {
		t.Fatalf("split %s: %v", ts.URL, err)
	}
	return port
}

// TestClientRefusesRedirectsOffTheHubOrigin is the regression test for credential leakage via
// redirect: a hub URL that answers with a redirect to another origin — a plaintext http:// URL on
// the same hostname, another port, or another host — must not receive the admin password (the
// 307-replayed login body), the Basic Auth header, or any request at all. The sinks count every
// request they receive, so "zero hits" is a direct observation that nothing was sent.
func TestClientRefusesRedirectsOffTheHubOrigin(t *testing.T) {
	var plainHits, tlsHits atomic.Int32
	plainSink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plainHits.Add(1)
		answerLikeHub(w, r)
	}))
	defer plainSink.Close()
	tlsSink := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tlsHits.Add(1)
		answerLikeHub(w, r)
	}))
	defer tlsSink.Close()

	cases := []struct {
		name   string
		target string // redirect destination (path appended)
		hits   *atomic.Int32
	}{
		{"https to http on the same hostname", "http://hub.test.invalid:" + portOf(t, plainSink), &plainHits},
		{"same hostname, different port", "https://hub.test.invalid:" + portOf(t, tlsSink), &tlsHits},
		{"different hostname", "https://elsewhere.test.invalid:" + portOf(t, tlsSink), &tlsHits},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hub := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, tc.target+r.URL.Path, http.StatusTemporaryRedirect)
			}))
			defer hub.Close()
			c, err := NewClient(Config{
				BaseURL:   "https://hub.test.invalid:" + portOf(t, hub),
				BasicUser: "proxy-user", BasicPass: "proxy-pass", AdminPass: "admin-secret",
			})
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			c.http.Transport = hostnameTransport(t, hub)
			tc.hits.Store(0)

			if err := c.login(context.Background(), true); err == nil {
				t.Error("login followed a redirect off the hub origin; want an error")
			}
			if _, err := c.getBytes(context.Background(), "/api/targets", nil); err == nil {
				t.Error("GET followed a redirect off the hub origin; want an error")
			}
			if n := tc.hits.Load(); n != 0 {
				t.Fatalf("redirect target received %d request(s) carrying hub credentials; want 0", n)
			}
		})
	}
}

// TestClientFollowsSameOriginRedirect is the positive control for the redirect policy: a redirect
// that stays on the configured hub origin is followed, with Basic Auth intact.
func TestClientFollowsSameOriginRedirect(t *testing.T) {
	hub := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/targets" {
			http.Redirect(w, r, "/api/targets/", http.StatusFound)
			return
		}
		if u, p, ok := r.BasicAuth(); !ok || u != "proxy-user" || p != "proxy-pass" {
			http.Error(w, "no basic auth", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"targets":[]}`))
	}))
	defer hub.Close()
	c, err := NewClient(Config{BaseURL: "https://hub.test.invalid:" + portOf(t, hub), BasicUser: "proxy-user", BasicPass: "proxy-pass"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	c.http.Transport = hostnameTransport(t, hub)
	if _, err := c.getBytes(context.Background(), "/api/targets", nil); err != nil {
		t.Fatalf("same-origin redirect not followed: %v", err)
	}
}
