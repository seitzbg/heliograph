// Package httpprobe is a native HTTP(S) probe (no external `curl`) that measures
// time-to-first-byte minus DNS resolution — the "how responsive is this endpoint"
// latency, analogous to SmokePing's Curl probe (see codemap 03). Registered as
// "HTTP". The URL is built from the target host via `urlformat` (%host%).
package httpprobe

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"

	"smokeping-modern/internal/probe"
)

type httpProbe struct {
	urlformat string
	method    string
	insecure  bool
}

func init() {
	probe.Register("HTTP", func(cfg map[string]string) (probe.Probe, error) {
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

func (p *httpProbe) Name() string     { return "HTTP" }
func (p *httpProbe) Describe() string { return "HTTP time-to-first-byte" }

func (p *httpProbe) Schema() map[string]probe.VarSpec {
	return map[string]probe.VarSpec{
		"urlformat":    {Doc: "URL template; %host% is replaced by the target host", Default: "https://%host%/", Scope: probe.TargetVar},
		"method":       {Doc: "HTTP method", Default: "GET", Scope: probe.ProbeVar},
		"insecure_ssl": {Doc: "skip TLS certificate verification (true/false)", Default: "false", Scope: probe.TargetVar},
	}
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
	for i := 0; i < pings; i++ {
		if err := ctx.Err(); err != nil {
			return probe.Result{Samples: samples}, err
		}
		if d, ok := p.once(ctx, client, url); ok {
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
