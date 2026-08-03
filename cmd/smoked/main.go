// Command smoked is the MVP collector: it runs measurement rounds over a small
// built-in target set using pluggable probes, prints a summary, and optionally
// serves the JSON API. It demonstrates the three requirements from the codemap:
// fast parallel polling, per-target isolation, and probes as plugins.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"smokeping-modern/internal/api"
	"smokeping-modern/internal/model"
	"smokeping-modern/internal/probe"
	"smokeping-modern/internal/scheduler"
	"smokeping-modern/internal/store"
	"smokeping-modern/internal/store/pgstore"

	// Register probe plugins (blank imports run their init() -> probe.Register).
	_ "smokeping-modern/internal/probe/dns"
	_ "smokeping-modern/internal/probe/fping"
	_ "smokeping-modern/internal/probe/httpprobe"
	_ "smokeping-modern/internal/probe/tcpconnect"
)

func main() {
	rounds := flag.Int("rounds", 2, "number of measurement rounds to run")
	pings := flag.Int("pings", 10, "pings per round (N)")
	workers := flag.Int("workers", 50, "max concurrent probes")
	step := flag.Duration("step", 5*time.Second, "interval between rounds")
	timeout := flag.Duration("timeout", 4*time.Second, "per-target timeout")
	serve := flag.Bool("serve", false, "serve the JSON API + web UI after the rounds (runs forever)")
	addr := flag.String("addr", ":8087", "API listen address when -serve")
	webdir := flag.String("webdir", "web", "directory of static web assets to serve at /")
	dsn := flag.String("dsn", "", "TimescaleDB/PostgreSQL DSN; if set, persist there instead of in-memory")
	flag.Parse()

	fmt.Printf("registered probe plugins: %s\n\n", strings.Join(probe.Registered(), ", "))

	monitors := demoMonitors(*pings, *step)

	// Build probe instances once per (kind,config); reuse across rounds/targets.
	probes := map[string]probe.Probe{}
	var jobs []scheduler.Job
	for _, m := range monitors {
		p, ok := probes[m.ProbeKind]
		if !ok {
			var err error
			p, err = probe.New(m.ProbeKind, nil)
			if err != nil {
				log.Fatalf("probe %s: %v", m.ProbeKind, err)
			}
			probes[m.ProbeKind] = p
		}
		jobs = append(jobs, scheduler.Job{
			Probe:   p,
			Target:  probe.Target{Name: m.Name, Host: m.Host, Params: m.Params},
			Pings:   m.Pings,
			Timeout: *timeout,
		})
	}

	ctx := context.Background()

	var st store.Store
	if *dsn != "" {
		pg, err := pgstore.New(ctx, *dsn, 1024, func(e error) { log.Printf("store: %v", e) })
		if err != nil {
			log.Fatalf("store: %v", err)
		}
		defer pg.Close()
		st = pg
		fmt.Printf("store: TimescaleDB\n")
	} else {
		st = store.NewMem(1024)
		fmt.Printf("store: in-memory (pass -dsn to persist to TimescaleDB)\n")
	}

	for r := 1; r <= *rounds; r++ {
		start := time.Now()
		out := scheduler.RunRound(ctx, jobs, *workers)
		st.Add(out)
		printRound(r, time.Since(start), out)
		if r < *rounds {
			time.Sleep(*step)
		}
	}

	if *serve {
		srv := api.New(st, *webdir)
		fmt.Printf("\nserving web UI + JSON API on %s  (/, /api/targets, /api/series?target=NAME, /api/probes)\n", *addr)
		// keep polling in the background while serving
		go func() {
			for {
				time.Sleep(scheduler.NextDelay(time.Now(), *step, 0))
				st.Add(scheduler.RunRound(ctx, jobs, *workers))
			}
		}()
		if err := http.ListenAndServe(*addr, srv.Routes()); err != nil {
			log.Fatal(err)
		}
	}
}

func demoMonitors(pings int, step time.Duration) []model.Monitor {
	return []model.Monitor{
		{Name: "Cloudflare DNS (ICMP)", ProbeKind: "FPing", Host: "1.1.1.1", Pings: pings, Step: step},
		{Name: "Google DNS (ICMP)", ProbeKind: "FPing", Host: "8.8.8.8", Pings: pings, Step: step},
		{Name: "localhost (ICMP)", ProbeKind: "FPing", Host: "127.0.0.1", Pings: pings, Step: step},
		{Name: "Cloudflare 443 (TCP)", ProbeKind: "TCPConnect", Host: "1.1.1.1", Pings: pings, Step: step, Params: map[string]string{"port": "443"}},
		{Name: "Google 443 (TCP)", ProbeKind: "TCPConnect", Host: "www.google.com", Pings: pings, Step: step, Params: map[string]string{"port": "443"}},
		{Name: "Unreachable :9 (TCP, expect loss)", ProbeKind: "TCPConnect", Host: "192.0.2.1", Pings: pings, Step: step, Params: map[string]string{"port": "9"}},
		{Name: "Cloudflare resolver (DNS)", ProbeKind: "DNS", Host: "1.1.1.1", Pings: pings, Step: step, Params: map[string]string{"lookup": "example.com"}},
		{Name: "Google resolver (DNS)", ProbeKind: "DNS", Host: "8.8.8.8", Pings: pings, Step: step, Params: map[string]string{"lookup": "example.com"}},
		{Name: "example.com (HTTP TTFB)", ProbeKind: "HTTP", Host: "example.com", Pings: pings, Step: step},
		{Name: "cloudflare.com (HTTP TTFB)", ProbeKind: "HTTP", Host: "www.cloudflare.com", Pings: pings, Step: step},
	}
}

func printRound(n int, dur time.Duration, out []scheduler.Outcome) {
	sorted := make([]scheduler.Outcome, len(out))
	copy(sorted, out)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Target.Name < sorted[j].Target.Name })

	fmt.Printf("── round %d  (%d targets in %s, wall-clock) ─────────────────\n", n, len(out), dur.Round(time.Millisecond))
	fmt.Printf("%-38s %-10s %9s %7s  %s\n", "TARGET", "PROBE", "MEDIAN", "LOSS", "NOTE")
	for _, o := range sorted {
		med := "  --"
		if !math.IsNaN(o.Computed.Median) {
			med = fmt.Sprintf("%7.2fms", o.Computed.Median*1000)
		}
		note := ""
		if o.Err != nil {
			note = "err: " + o.Err.Error()
		}
		fmt.Printf("%-38s %-10s %9s %4d/%-2d  %s\n",
			trunc(o.Target.Name, 38), o.ProbeName, med, o.Computed.Loss, o.Computed.Pings, note)
	}
	fmt.Println()
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
