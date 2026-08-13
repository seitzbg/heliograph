// Package dns is a native DNS probe (no external `dig`) that measures how long
// a resolver takes to answer a query. Registered as "DNS". The Target.Host is
// the DNS server under test; the queried name is the `lookup` param — matching
// SmokePing's DNS/AnotherDNS probes (see codemap 03).
package dns

import (
	"context"
	"fmt"
	"net"

	"github.com/miekg/dns"

	"github.com/seitzbg/heliograph/internal/probe"
)

type dnsProbe struct {
	lookup     string
	recordType uint16
	port       string
	proto      string // "udp" or "tcp"
}

func init() {
	probe.Register("DNS", "DNS query", map[string]probe.VarSpec{
		"lookup":     {Doc: "name to query against the target resolver", Default: "google.com", Scope: probe.TargetVar},
		"recordtype": {Doc: "DNS record type: any recognized type (A, AAAA, MX, TXT, NS, CNAME, SOA, PTR, SRV, CAA, ...)", Default: "A", Scope: probe.TargetVar, Validate: validRecordType},
		"port":       {Doc: "resolver port", Default: "53", Scope: probe.TargetVar, Kind: probe.KindPort},
		"protocol":   {Doc: "transport: udp or tcp", Default: "udp", Scope: probe.ProbeVar, Enum: []string{"udp", "tcp"}},
	}, func(cfg map[string]string) (probe.Probe, error) {
		p := &dnsProbe{lookup: "google.com.", recordType: dns.TypeA, port: "53", proto: "udp"}
		if v := cfg["lookup"]; v != "" {
			p.lookup = dns.Fqdn(v)
		}
		if v := cfg["recordtype"]; v != "" {
			if t, ok := dns.StringToType[v]; ok {
				p.recordType = t
			} else {
				return nil, fmt.Errorf("dns: unknown record type %q", v)
			}
		}
		if v := cfg["port"]; v != "" {
			p.port = v
		}
		if v := cfg["protocol"]; v == "tcp" || v == "udp" {
			p.proto = v
		}
		return p, nil
	})
}

func (p *dnsProbe) Name() string { return "DNS" }

// validRecordType accepts any DNS record type miekg/dns recognizes — the same set
// the probe can actually query — so config validation matches runtime behavior.
func validRecordType(v string) error {
	if _, ok := dns.StringToType[v]; !ok {
		return fmt.Errorf("recordtype %q is not a known DNS record type", v)
	}
	return nil
}

func (p *dnsProbe) Measure(ctx context.Context, t probe.Target, pings int) (probe.Result, error) {
	lookup := t.Param("lookup", p.lookup)
	if lookup == "" {
		lookup = "google.com."
	}
	server := net.JoinHostPort(t.Host, t.Param("port", p.port))

	// recordtype is target-scoped (see Schema): a per-target value overrides the
	// probe-level default; an unrecognized value falls back to it.
	recordType := p.recordType
	if v := t.Param("recordtype", ""); v != "" {
		if rt, ok := dns.StringToType[v]; ok {
			recordType = rt
		}
	}

	var samples []float64
	// Fair per-ping share of the round budget so a hung/unresponsive resolver is queried
	// all `pings` times instead of the first query eating the whole round (correct loss,
	// no worker held for the full step).
	perPing := probe.PerPingBudget(ctx, pings)
	for i := 0; i < pings; i++ {
		if err := ctx.Err(); err != nil {
			return probe.Result{Samples: samples}, err
		}
		c := &dns.Client{Net: p.proto}
		m := new(dns.Msg)
		m.SetQuestion(dns.Fqdn(lookup), recordType)
		actx, cancel := probe.AttemptContext(ctx, perPing)
		_, rtt, err := c.ExchangeContext(actx, m, server)
		cancel()
		if err != nil {
			continue // query failed/timed out => lost
		}
		samples = append(samples, rtt.Seconds())
	}
	return probe.Result{Samples: samples}, nil
}
