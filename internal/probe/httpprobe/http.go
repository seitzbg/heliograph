// Package httpprobe is a native HTTP(S) probe (no external `curl`) that measures
// time-to-first-byte minus DNS resolution — the "how responsive is this endpoint"
// latency, analogous to SmokePing's Curl probe (see codemap 03). Registered as
// "HTTP". The URL is built from the target host via `urlformat` (%host%).
package httpprobe

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"time"

	"github.com/seitzbg/heliograph/internal/probe"
)

type httpProbe struct {
	urlformat string
	method    string
	insecure  bool
}

func init() {
	probe.Register("HTTP", "HTTP time-to-first-byte", map[string]probe.VarSpec{
		"urlformat":    {Doc: "URL template; %host% is replaced by the target host", Default: "https://%host%/", Scope: probe.TargetVar, Validate: validURLFormat},
		"method":       {Doc: "HTTP method", Default: "GET", Scope: probe.ProbeVar, Validate: validMethod},
		"insecure_ssl": {Doc: "skip TLS certificate verification (true/false)", Default: "false", Scope: probe.TargetVar, Kind: probe.KindBool},
	}, func(cfg map[string]string) (probe.Probe, error) {
		p := &httpProbe{urlformat: "https://%host%/", method: "GET"}
		if v := cfg["urlformat"]; v != "" {
			p.urlformat = v
		}
		if v := cfg["method"]; v != "" {
			p.method = strings.ToUpper(v)
		}
		if cfg["insecure_ssl"] == "true" {
			p.insecure = true
		}
		return p, nil
	})
}

func (p *httpProbe) Name() string { return "HTTP" }

// validURLFormat rejects a urlformat that can't build a request after %host%
// substitution — a malformed scheme/host would otherwise fail request creation at
// runtime and be counted as packet loss rather than flagged as a config error.
func validURLFormat(v string) error {
	u, err := url.Parse(strings.ReplaceAll(v, "%host%", "example.com"))
	if err != nil {
		return fmt.Errorf("urlformat %q is not a valid URL: %w", v, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("urlformat %q must be an http(s) URL", v)
	}
	if u.Host == "" {
		return fmt.Errorf("urlformat %q has no host", v)
	}
	return nil
}

var httpMethods = map[string]bool{
	"GET": true, "HEAD": true, "POST": true, "PUT": true, "DELETE": true,
	"OPTIONS": true, "PATCH": true, "TRACE": true, "CONNECT": true,
}

// validMethod rejects a method that isn't a recognized HTTP verb (an invalid method
// makes http.NewRequest fail at runtime, counted as loss instead of a config error).
func validMethod(v string) error {
	if !httpMethods[strings.ToUpper(v)] {
		return fmt.Errorf("method %q is not a recognized HTTP method", v)
	}
	return nil
}

func (p *httpProbe) Measure(ctx context.Context, t probe.Target, pings int) (probe.Result, error) {
	urlformat := t.Param("urlformat", p.urlformat)
	url := strings.ReplaceAll(urlformat, "%host%", t.Host)

	// insecure_ssl is target-scoped (see Schema): a per-target value overrides the
	// probe-level default.
	insecure := p.insecure
	if v := t.Param("insecure_ssl", ""); v != "" {
		insecure = v == "true"
	}

	// Fresh connection per ping so each sample reflects full connect+TLS+TTFB
	// (like SmokePing's Curl), not a reused keep-alive.
	tr := &http.Transport{
		DisableKeepAlives: true,
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: insecure}, //nolint:gosec // opt-in via config
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}

	var samples []float64
	// Fair per-ping share of the round budget so a hung/unresponsive endpoint is probed
	// all `pings` times instead of the first request eating the whole round (correct
	// loss, no worker held for the full step).
	perPing := probe.PerPingBudget(ctx, pings)
	for i := 0; i < pings; i++ {
		if err := ctx.Err(); err != nil {
			return probe.Result{Samples: samples}, err
		}
		actx, cancel := probe.AttemptContext(ctx, perPing)
		d, ok := p.once(actx, client, url)
		cancel()
		if ok {
			samples = append(samples, d)
		}
	}
	return probe.Result{Samples: samples}, nil
}

func (p *httpProbe) once(ctx context.Context, client *http.Client, url string) (float64, bool) {
	var dnsStart, dnsDone, firstByte time.Time
	trace := &httptrace.ClientTrace{
		DNSStart:             func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone:              func(httptrace.DNSDoneInfo) { dnsDone = time.Now() },
		GotFirstResponseByte: func() { firstByte = time.Now() },
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), p.method, url, nil)
	if err != nil {
		return 0, false
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, false // connection/timeout => lost
	}
	if firstByte.IsZero() {
		firstByte = time.Now()
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1)) // ensure TTFB observed
	_ = resp.Body.Close()

	elapsed := firstByte.Sub(start)
	if !dnsStart.IsZero() && !dnsDone.IsZero() {
		elapsed -= dnsDone.Sub(dnsStart) // report latency excluding DNS, like Curl
	}
	if elapsed <= 0 {
		return 0, false
	}
	return elapsed.Seconds(), true
}
