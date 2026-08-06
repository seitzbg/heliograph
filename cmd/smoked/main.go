// Command smoked is the MVP collector: it runs measurement rounds over a small
// built-in target set using pluggable probes, prints a summary, and optionally
// serves the JSON API. It demonstrates the three requirements from the codemap:
// fast parallel polling, per-target isolation, and probes as plugins.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"smokeping-modern/internal/alert"
	"smokeping-modern/internal/api"
	"smokeping-modern/internal/config"
	"smokeping-modern/internal/model"
	"smokeping-modern/internal/probe"
	"smokeping-modern/internal/scheduler"
	"smokeping-modern/internal/store"
	"smokeping-modern/internal/store/pgstore"

	// Register probe plugins (blank imports run their init() -> probe.Register).
	_ "smokeping-modern/internal/probe/dns"
	_ "smokeping-modern/internal/probe/fping"
	_ "smokeping-modern/internal/probe/httpprobe"
	_ "smokeping-modern/internal/probe/irttprobe"
	_ "smokeping-modern/internal/probe/sshprobe"
	_ "smokeping-modern/internal/probe/tcpconnect"
)

// version is the smoked release version. Overridable at build time with
//
//	go build -ldflags "-X main.version=$(git describe --tags)"
var version = "0.1.0"

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	rounds := flag.Int("rounds", 2, "number of measurement rounds to run")
	pings := flag.Int("pings", 10, "pings per round (N)")
	workers := flag.Int("workers", 50, "max concurrent probes")
	step := flag.Duration("step", 5*time.Second, "interval between rounds")
	timeout := flag.Duration("timeout", 4*time.Second, "per-target timeout")
	serve := flag.Bool("serve", false, "serve the JSON API + web UI after the rounds (runs forever)")
	addr := flag.String("addr", ":8087", "API listen address when -serve")
	webdir := flag.String("webdir", "web", "directory of static web assets to serve at /")
	dsn := flag.String("dsn", "", "TimescaleDB/PostgreSQL DSN; if set, persist there instead of in-memory")
	downsample := flag.Bool("downsample", false, "with -dsn: enable the hourly continuous aggregate + retention policies")
	configPath := flag.String("config", "", "path to a YAML config file, or a directory holding default.yaml + conf.d/*.yaml; replaces the built-in demo targets")
	webhook := flag.String("webhook", "", "webhook URL for alerts named 'to: [webhook]'")
	logFormat := flag.String("log-format", "text", "operational log format: text or json")
	logLevel := flag.String("log-level", "info", "operational log level: debug, info, warn, error")
	flag.Parse()

	if *showVersion {
		fmt.Printf("smoked %s\n", version)
		return
	}

	setupLogger(*logFormat, *logLevel)

	if *pings < 1 || *pings > config.MaxPings {
		fatal("invalid -pings", fmt.Errorf("must be between 1 and %d, got %d", config.MaxPings, *pings))
	}
	if *step < config.MinStep {
		fatal("invalid -step", fmt.Errorf("must be at least %s, got %s", config.MinStep, *step))
	}

	fmt.Printf("smoked %s — registered probe plugins: %s\n\n", version, strings.Join(probe.Registered(), ", "))

	notifiers := map[string]alert.Notifier{"log": alert.LogNotifier{W: os.Stdout}}
	var webhookN *alert.WebhookNotifier // concrete ref for /metrics + shutdown drain
	if *webhook != "" {
		// Validate the URL up front so a typo is a clear startup error, not a stream
		// of per-event delivery failures at runtime.
		if u, err := url.Parse(*webhook); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			fatal("invalid -webhook URL", fmt.Errorf("must be an absolute http(s) URL, got %q", *webhook))
		}
		webhookN = alert.NewWebhookNotifier(*webhook, nil) // bounded queue + retry/backoff + drain
		notifiers["webhook"] = webhookN
	}

	// The runtime (jobs + alert engine) is built from config (or the demo set) and
	// held behind an atomic pointer so it can be swapped on SIGHUP reload.
	rt, err := buildRuntime(*configPath, *pings, *step, *timeout, notifiers)
	if err != nil {
		fatal("startup failed", err)
	}
	var current atomic.Pointer[runtime]
	current.Store(rt)
	// evalMu serializes a reload's state-inheritance+swap against a round's alert
	// evaluation, so a round that finishes measuring across a reload boundary still
	// updates the live engine (not the abandoned one) — no lost hysteresis state.
	var evalMu sync.Mutex
	if *configPath != "" {
		fmt.Printf("config: %d targets from %s\n", len(rt.jobs), *configPath)
	}

	// Cancel in-flight probes and the HTTP server on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// SIGHUP reloads the config; on error the running config is kept (a bad edit
	// can't take the collector down). Alert firing state and sample windows carry
	// over from the running engine so a reload doesn't re-fire alerts already
	// firing or drop the history a hysteresis alert needs (see InheritStateFrom).
	if *configPath != "" {
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		go func() {
			for range hup {
				nrt, err := buildRuntime(*configPath, *pings, *step, *timeout, notifiers)
				if err != nil {
					slog.Error("reload failed, keeping running config", "err", err)
					continue
				}
				// Snapshot the running engine and swap under evalMu so a concurrent
				// round's eval is either fully before (its update is inherited) or
				// fully after (it lands in the new engine) — never lost in between.
				evalMu.Lock()
				old := current.Load()
				if nrt.engine != nil && old.engine != nil {
					valid := make(map[string]bool, len(nrt.alertsByTarget))
					for t := range nrt.alertsByTarget {
						valid[t] = true
					}
					nrt.engine.InheritStateFrom(old.engine, valid)
				}
				current.Store(nrt)
				evalMu.Unlock()
				slog.Info("config reloaded", "path", *configPath, "targets", len(nrt.jobs))
			}
		}()
	}

	var st store.Store
	if *dsn != "" {
		pg, err := pgstore.New(ctx, *dsn, 1024, func(e error) { slog.Error("store error", "err", e) })
		if err != nil {
			fatal("store init failed", err)
		}
		defer pg.Close()
		if *downsample {
			if err := pg.EnableDownsampling(ctx); err != nil {
				fatal("enabling downsampling failed", err)
			}
			fmt.Printf("store: TimescaleDB (downsampling enabled)\n")
		} else {
			fmt.Printf("store: TimescaleDB\n")
		}
		st = pg
	} else {
		st = store.NewMem(1024)
		fmt.Printf("store: in-memory (pass -dsn to persist to TimescaleDB)\n")
	}

	roundStats := &api.RoundStats{}

	// In serve mode the per-target planner fires each target on its own cadence, so
	// synchronous warm-up rounds (run on the global -step, not per-target steps) would
	// only double-fire every target and rush hysteresis at startup — skip them
	// (CODE_REVIEW #8). Non-serve (demo) mode still runs -rounds.
	warmupRounds := *rounds
	if *serve {
		warmupRounds = 0
	}
	for r := 1; r <= warmupRounds && ctx.Err() == nil; r++ {
		rt := current.Load()
		start := time.Now()
		out := scheduler.RunRound(ctx, rt.jobs, *workers)
		dur := time.Since(start)
		st.Add(out)
		evalMu.Lock()
		current.Load().eval(out) // eval on the live runtime (may differ after a reload)
		evalMu.Unlock()
		roundStats.Observe(dur, len(out), countErrs(out), start)
		logRound(r, dur, out)
		printRound(r, dur, out)
		if r < *rounds {
			select {
			case <-ctx.Done():
			case <-time.After(*step):
			}
		}
	}

	if *serve && ctx.Err() == nil {
		srv := api.New(st, *webdir)
		srv.Rounds = roundStats
		// Expose the webhook delivery counters (queued/delivered/retried/dropped/failed)
		// on /metrics so delivery health is scrapeable, not merely logged.
		if webhookN != nil {
			srv.ExtraMetrics = webhookN.WriteMetrics
		}
		// Live views report only currently-configured targets, so a target removed or
		// renamed on a SIGHUP reload stops showing as healthy (its history stays in
		// the store but ages out via retention / the bounded cap).
		srv.Active = func() map[string]bool {
			jobs := current.Load().jobs
			m := make(map[string]bool, len(jobs))
			for _, j := range jobs {
				m[j.Target.Name] = true
			}
			return m
		}
		// Per-target step drives /api/sla's coverage (expected rounds = window/step).
		srv.Steps = func() map[string]time.Duration {
			jobs := current.Load().jobs
			m := make(map[string]time.Duration, len(jobs))
			for _, j := range jobs {
				m[j.Target.Name] = j.Step
			}
			return m
		}
		// Defensive timeouts so a slow or idle client can't tie up a connection
		// indefinitely (the read endpoints are expensive). WriteTimeout is generous
		// because an aggregate scan over many targets can take a little while.
		httpSrv := &http.Server{
			Addr:              *addr,
			Handler:           srv.Routes(),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
		fmt.Printf("\nserving web UI + JSON API on %s  (/, /api/targets, /api/series?target=NAME, /api/probes, /metrics)\n", *addr)
		slog.Info("serving", "addr", *addr)
		// Keep polling in the background while serving. Each target fires on its own
		// Step via the Planner (a slow-cadence target no longer polls at the fast
		// default), and the loop wakes at least once a second so a SIGHUP reload's
		// new target set is picked up promptly.
		// pollDone closes when the polling goroutine has returned and drained its
		// in-flight probes, so main can join it on shutdown rather than exiting while
		// store writes are still running (CODE_REVIEW lower-severity shutdown note).
		pollDone := make(chan struct{})
		go func() {
			defer close(pollDone)
			// The Dispatcher runs each tick's due targets without blocking the loop, so
			// a slow target burning down its timeout no longer delays faster targets that
			// come due meanwhile (review item #3a). Each batch's store-write + alert eval
			// happen on completion; eval stays serialized under evalMu, and store.Add /
			// RoundStats are concurrency-safe, so overlapping batches are fine.
			disp := scheduler.NewDispatcher(*workers)
			planner := scheduler.NewPlanner()
			const maxSleep = time.Second
			for {
				now := time.Now()
				due, _ := planner.Tick(current.Load().jobs, now, maxSleep)
				if len(due) > 0 {
					start := now
					disp.Go(ctx, due,
						func(o scheduler.Outcome) { // per outcome: store + alert eval, in completion order
							st.Add([]scheduler.Outcome{o})
							evalMu.Lock()
							current.Load().eval([]scheduler.Outcome{o}) // eval on the live runtime (may differ after a reload)
							evalMu.Unlock()
						},
						func(bs scheduler.BatchStat) { // once per tick's batch: operational round metrics
							roundStats.Observe(bs.Duration, bs.Ran, bs.Errs, start)
							slog.Info("round complete", "targets", bs.Ran, "errors", bs.Errs,
								"duration_ms", float64(bs.Duration.Microseconds())/1000)
						})
				}
				// Recompute the sleep from the current time so the phase-aligned grid holds
				// and an overrun target fires on the next tick, not a step later.
				sleep := planner.SleepToNext(time.Now(), maxSleep)
				select {
				case <-ctx.Done():
					disp.Wait() // let in-flight batches drain (their probes honor ctx)
					return
				case <-time.After(sleep):
				}
			}
		}()
		// graceful shutdown on signal
		go func() {
			<-ctx.Done()
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = httpSrv.Shutdown(shutCtx)
		}()
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fatal("http server failed", err)
		}
		<-pollDone // wait for the polling goroutine to drain in-flight probes before exiting
		// The poll goroutine has stopped producing events, so no more Notify calls can
		// race the close; drain any queued webhook deliveries within a deadline.
		if webhookN != nil {
			drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			webhookN.Close(drainCtx)
			cancel()
		}
		slog.Info("shutdown complete")
		fmt.Println("shutdown complete")
	}
}

// setupLogger installs the process-wide structured logger for operational events
// (lifecycle, reloads, errors, per-round summaries). The human-facing round table
// and startup banner stay on fmt; everything an operator would grep or ship to a
// log pipeline goes through slog. json is for log aggregators; text for a console.
func setupLogger(format, level string) {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if strings.ToLower(format) == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
}

// fatal logs a structured error and exits non-zero (slog has no Fatal helper).
func fatal(msg string, err error) {
	slog.Error(msg, "err", err)
	os.Exit(1)
}

// countErrs returns how many outcomes in a round carried a probe error.
func countErrs(out []scheduler.Outcome) int {
	n := 0
	for _, o := range out {
		if o.Err != nil {
			n++
		}
	}
	return n
}

// logRound emits one structured record summarizing a completed round: its
// wall-clock, target count, and how many errored. round=0 marks a background
// (serving-mode) round, which has no sequence number.
func logRound(round int, dur time.Duration, out []scheduler.Outcome) {
	attrs := []any{
		"targets", len(out),
		"errors", countErrs(out),
		"duration_ms", float64(dur.Microseconds()) / 1000,
	}
	if round > 0 {
		attrs = append(attrs, "round", round)
	}
	slog.Info("round complete", attrs...)
}

// runtime is the swappable set of work: the probe jobs and the alert engine.
// It is rebuilt (and atomically swapped) on SIGHUP config reload.
type runtime struct {
	jobs            []scheduler.Job
	engine          *alert.Engine
	alertsByTarget  map[string][]string
	alerteeByTarget map[string][]string
}

// eval runs the alert engine over a round's outcomes and dispatches notifications.
func (rt *runtime) eval(out []scheduler.Outcome) {
	if rt.engine == nil {
		return
	}
	for _, o := range out {
		names := rt.alertsByTarget[o.Target.Name]
		if len(names) == 0 {
			continue
		}
		events := rt.engine.Evaluate(o.Target.Name, names,
			o.Computed.LossFraction()*100, o.Computed.Median, o.When)
		rt.engine.Dispatch(events, rt.alerteeByTarget[o.Target.Name]...)
	}
}

// buildRuntime loads targets (from YAML config, or the demo set) and builds the
// probe jobs and alert engine. A probe whose binary/deps are unavailable is
// skipped with a warning, not fatal.
func buildRuntime(configPath string, demoPings int, demoStep, timeout time.Duration, notifiers map[string]alert.Notifier) (rt *runtime, err error) {
	// Final safeguard for the SIGHUP reload path: a panic while building the runtime
	// (e.g. a config edge case the validator misses) must not take the collector
	// down — the reload goroutine turns this error into "keep the running config".
	defer func() {
		if r := recover(); r != nil {
			rt, err = nil, fmt.Errorf("build runtime panicked: %v", r)
		}
	}()
	var monitors []model.Monitor
	var probeCfgs map[string]map[string]string
	var alertDefs map[string]*alert.Alert
	if configPath != "" {
		cfg, err := config.LoadPath(configPath)
		if err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
		if monitors, err = cfg.Monitors(); err != nil {
			return nil, err
		}
		if alertDefs, err = cfg.BuildAlerts(); err != nil {
			return nil, err
		}
		probeCfgs = cfg.Probes
	} else {
		monitors = demoMonitors(demoPings, demoStep)
	}

	probes := map[string]probe.Probe{}
	var jobs []scheduler.Job
	for _, m := range monitors {
		p, ok := probes[m.ProbeKind]
		if !ok {
			var err error
			if p, err = probe.New(m.ProbeKind, probeCfgs[m.ProbeKind]); err != nil {
				slog.Warn("skipping unavailable probe", "probe", m.ProbeKind, "err", err)
				p = nil
			}
			probes[m.ProbeKind] = p // cache success or failure (nil)
		}
		if p == nil {
			continue
		}
		jobs = append(jobs, scheduler.Job{
			Probe:   p,
			Target:  probe.Target{Name: m.Name, Host: m.Host, Params: m.Params},
			Pings:   m.Pings,
			Timeout: timeout,
			Step:    m.Step,
		})
	}

	alertsByTarget := map[string][]string{}
	alerteeByTarget := map[string][]string{}
	for _, m := range monitors {
		if len(m.Alerts) > 0 {
			alertsByTarget[m.Name] = m.Alerts
		}
		if len(m.Alertee) > 0 {
			alerteeByTarget[m.Name] = m.Alertee
		}
	}
	var engine *alert.Engine
	if len(alertDefs) > 0 {
		engine = alert.NewEngine(alertDefs, notifiers)
	}
	return &runtime{jobs: jobs, engine: engine, alertsByTarget: alertsByTarget, alerteeByTarget: alerteeByTarget}, nil
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
		{Name: "github.com (SSH banner)", ProbeKind: "SSH", Host: "github.com", Pings: pings, Step: step},
		{Name: "localhost (IRTT, needs irtt server)", ProbeKind: "IRTT", Host: "127.0.0.1", Pings: pings, Step: step, Params: map[string]string{"port": "2112"}},
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
