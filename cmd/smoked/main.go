// Command smoked is the MVP collector: it runs measurement rounds over a small
// built-in target set using pluggable probes, prints a summary, and optionally
// serves the JSON API. It demonstrates the three requirements from the codemap:
// fast parallel polling, per-target isolation, and probes as plugins.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/seitzbg/heliograph/internal/alert"
	"github.com/seitzbg/heliograph/internal/api"
	"github.com/seitzbg/heliograph/internal/config"
	"github.com/seitzbg/heliograph/internal/configstore"
	"github.com/seitzbg/heliograph/internal/federation"
	"github.com/seitzbg/heliograph/internal/model"
	"github.com/seitzbg/heliograph/internal/probe"
	"github.com/seitzbg/heliograph/internal/scheduler"
	"github.com/seitzbg/heliograph/internal/store"
	"github.com/seitzbg/heliograph/internal/store/pgstore"
	"github.com/seitzbg/heliograph/internal/vantage"

	// Register probe plugins (blank imports run their init() -> probe.Register).
	_ "github.com/seitzbg/heliograph/internal/probe/dns"
	_ "github.com/seitzbg/heliograph/internal/probe/fping"
	_ "github.com/seitzbg/heliograph/internal/probe/httpprobe"
	_ "github.com/seitzbg/heliograph/internal/probe/irttprobe"
	_ "github.com/seitzbg/heliograph/internal/probe/pingprobe"
	_ "github.com/seitzbg/heliograph/internal/probe/sshprobe"
	_ "github.com/seitzbg/heliograph/internal/probe/tcpconnect"
)

// version is the smoked release version. Unset in an unversioned build (a plain `go build`
// must not claim a release); overridable at build time with
//
//	go build -ldflags "-X main.version=$(git describe --tags)"
var version = "dev"

// validateRuntimeFlags checks the operational numeric flags the collector shares. A
// non-positive -timeout is the per-ping budget the scheduler multiplies by pings for
// each round; non-positive would cancel every probe immediately (a started-but-dead
// collector) — so reject it at the CLI boundary alongside -pings and -step.
func validateRuntimeFlags(pings int, step, timeout time.Duration) error {
	if pings < 1 || pings > config.MaxPings {
		return fmt.Errorf("-pings must be between 1 and %d, got %d", config.MaxPings, pings)
	}
	if step < config.MinStep {
		return fmt.Errorf("-step must be at least %s, got %s", config.MinStep, step)
	}
	if timeout <= 0 {
		return fmt.Errorf("-timeout must be positive, got %s", timeout)
	}
	return nil
}

// envBool reads a boolean flag default from the environment, so a Compose/K8s deployment can drive
// it via `environment:` rather than the command list. Follows strconv.ParseBool; empty or
// unparseable = false.
func envBool(name string) bool {
	b, _ := strconv.ParseBool(os.Getenv(name))
	return b
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "vantage" {
		os.Exit(vantageCmd(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "config" {
		os.Exit(configCmd(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "import" {
		os.Exit(importCmd(os.Args[2:]))
	}

	showVersion := flag.Bool("version", false, "print version and exit")
	rounds := flag.Int("rounds", 2, "number of measurement rounds to run")
	pings := flag.Int("pings", 10, "pings per round (N)")
	workers := flag.Int("workers", 50, "max concurrent probes")
	step := flag.Duration("step", 5*time.Second, "interval between rounds")
	timeout := flag.Duration("timeout", 4*time.Second, "per-ping timeout; a target's round budget is this × pings, capped by its step (so N sequential pings each get ~this long, and a slow endpoint reports latency, not loss)")
	serve := flag.Bool("serve", false, "serve the JSON API + web UI after the rounds (runs forever)")
	addr := flag.String("addr", ":8087", "API listen address when -serve")
	webdir := flag.String("webdir", "web", "directory of static web assets to serve at /")
	dsn := flag.String("dsn", os.Getenv("SMOKED_DSN"), "TimescaleDB/PostgreSQL DSN (or set SMOKED_DSN); if set, persist there instead of in-memory")
	downsample := flag.Bool("downsample", envBool("SMOKED_DOWNSAMPLE"), "with -dsn: enable the hourly continuous aggregate + retention policies (or set SMOKED_DOWNSAMPLE=1)")
	resolveIPs := flag.Bool("resolve-ips", envBool("SMOKED_RESOLVE_IPS"), "show each target's IP in the graph title (or set SMOKED_RESOLVE_IPS=1): a pinned `ip:`, else a literal-IP host, else the resolved hostname (best-effort, refreshed on reload)")
	requireFingerprint := flag.Bool("require-fingerprint", false, "reject agent results that carry no measurement fingerprint (strict mode); default accepts them for pre-fingerprint agents. Flip on once every vantage's agent is upgraded (watch heliograph_agent_missing_fingerprint_total)")
	configPath := flag.String("config", os.Getenv("SMOKED_CONFIG"), "path to a YAML config file, or a directory holding default.yaml + conf.d/*.yaml (or set SMOKED_CONFIG); replaces the built-in demo targets")
	webhook := flag.String("webhook", "", "generic JSON webhook URL for alerts named 'to: [webhook]'")
	slackWebhook := flag.String("slack-webhook", os.Getenv("SMOKED_SLACK_WEBHOOK"), "Slack incoming-webhook URL (or set SMOKED_SLACK_WEBHOOK) for alerts named 'to: [slack]'")
	discordWebhook := flag.String("discord-webhook", os.Getenv("SMOKED_DISCORD_WEBHOOK"), "Discord webhook URL (or set SMOKED_DISCORD_WEBHOOK) for alerts named 'to: [discord]'")
	smtpAddr := flag.String("smtp-addr", os.Getenv("SMOKED_SMTP_ADDR"), "SMTP server host:port (or set SMOKED_SMTP_ADDR) to enable the email notifier ('to: [email]')")
	smtpFrom := flag.String("smtp-from", os.Getenv("SMOKED_SMTP_FROM"), "email From address (or set SMOKED_SMTP_FROM)")
	smtpTo := flag.String("smtp-to", os.Getenv("SMOKED_SMTP_TO"), "comma-separated email recipients (or set SMOKED_SMTP_TO)")
	smtpUser := flag.String("smtp-user", os.Getenv("SMOKED_SMTP_USER"), "SMTP username (or set SMOKED_SMTP_USER); set with -smtp-pass for authenticated submission")
	smtpPass := flag.String("smtp-pass", os.Getenv("SMOKED_SMTP_PASS"), "SMTP password (or set SMOKED_SMTP_PASS)")
	smtpInsecure := flag.Bool("smtp-insecure", envBool("SMOKED_SMTP_INSECURE"), "skip STARTTLS cert verification (or set SMOKED_SMTP_INSECURE=1) — for an internal relay with a self-signed cert")
	logFormat := flag.String("log-format", "text", "operational log format: text or json")
	logLevel := flag.String("log-level", "info", "operational log level: debug, info, warn, error")
	flag.Parse()

	if *showVersion {
		fmt.Printf("smoked %s\n", version)
		return
	}

	setupLogger(*logFormat, *logLevel)

	if err := validateRuntimeFlags(*pings, *step, *timeout); err != nil {
		fatal("invalid flags", err)
	}

	fmt.Printf("smoked %s — registered probe plugins: %s\n\n", version, strings.Join(probe.Registered(), ", "))

	notifiers := map[string]alert.Notifier{"log": alert.LogNotifier{W: os.Stdout}}
	// Concrete refs for /metrics + shutdown drain. webhook, slack, and discord share the same
	// WebhookNotifier delivery pool (bounded queue + retry/backoff + drain), differing only in body.
	var webhookNs []*alert.WebhookNotifier
	addWebhookish := func(name, flagName, rawURL string, ctor func(string, *http.Client) *alert.WebhookNotifier) {
		if rawURL == "" {
			return
		}
		// Validate the URL up front so a typo is a clear startup error, not a stream of per-event
		// delivery failures at runtime.
		if u, err := url.Parse(rawURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			fatal("invalid "+flagName+" URL", fmt.Errorf("must be an absolute http(s) URL, got %q", rawURL))
		}
		n := ctor(rawURL, nil)
		n.Name = name
		notifiers[name] = n
		webhookNs = append(webhookNs, n)
	}
	addWebhookish("webhook", "-webhook", *webhook, alert.NewWebhookNotifier)
	addWebhookish("slack", "-slack-webhook", *slackWebhook, func(u string, c *http.Client) *alert.WebhookNotifier {
		return alert.NewSlackNotifier(u, c, alert.WebhookConfig{})
	})
	addWebhookish("discord", "-discord-webhook", *discordWebhook, func(u string, c *http.Client) *alert.WebhookNotifier {
		return alert.NewDiscordNotifier(u, c, alert.WebhookConfig{})
	})
	// Email/SMTP notifier ('to: [email]'). Enabled when -smtp-addr/-smtp-from/-smtp-to are all set;
	// -smtp-user/-smtp-pass add authenticated submission (STARTTLS when the server offers it).
	var emailN *alert.EmailNotifier
	if *smtpAddr != "" || *smtpFrom != "" || *smtpTo != "" {
		var to []string
		for _, r := range strings.Split(*smtpTo, ",") {
			if r = strings.TrimSpace(r); r != "" {
				to = append(to, r)
			}
		}
		if *smtpAddr == "" || *smtpFrom == "" || len(to) == 0 {
			fatal("invalid SMTP config", fmt.Errorf("-smtp-addr, -smtp-from and -smtp-to are all required to enable email"))
		}
		var auth smtp.Auth
		if *smtpUser != "" || *smtpPass != "" {
			host := *smtpAddr
			if h, _, err := net.SplitHostPort(*smtpAddr); err == nil {
				host = h
			}
			auth = smtp.PlainAuth("", *smtpUser, *smtpPass, host)
		}
		emailN = alert.NewEmailNotifier(alert.EmailConfig{Addr: *smtpAddr, From: *smtpFrom, To: to, Auth: auth, TLSSkipVerify: *smtpInsecure})
		notifiers["email"] = emailN
	}
	// Drain queued deliveries on ANY exit path (serve shutdown, demo-mode return, early exit) through
	// this one lifecycle point (CODE_REVIEW #6). By the time it runs the event producers have stopped
	// — serve joins the poll goroutine (pollDone) before returning, and demo rounds are synchronous —
	// so no Notify races the close.
	if len(webhookNs) > 0 || emailN != nil {
		defer func() {
			drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for _, n := range webhookNs {
				n.Close(drainCtx)
			}
			if emailN != nil {
				emailN.Close(drainCtx)
			}
		}()
	}

	// DB-backed config source (item 1): active only with -dsn. An absent/empty row
	// contributes nothing, so YAML-only deployments are unaffected. Fetched fresh on
	// each build so a SIGHUP reload picks up DB edits, like conf.d drop-ins. cfgStore
	// is hoisted to function scope so the admin config-CRUD wiring below (ConfigGet/
	// ConfigApply) can reach it too.
	var dbFragment func() ([]byte, error)
	var cfgStore *configstore.Store
	if *dsn != "" {
		// The database may still be coming up on a fresh bring-up — notably TimescaleDB's
		// first-run init, where the container's socket-based healthcheck can report healthy
		// before the TCP listener is up, so depends_on releases the collector too early.
		// Wait for the database to accept a connection instead of crashing on the first
		// refused one (bounded, so a genuinely-down/misconfigured DB still fails visibly).
		const dbReadyTimeout = 60 * time.Second
		wctx, wcancel := context.WithTimeout(context.Background(), dbReadyTimeout)
		werr := pgstore.WaitReady(wctx, *dsn, func(e error) {
			slog.Warn("waiting for database to become ready", "err", e)
		})
		wcancel()
		if werr != nil {
			fatal("database not ready", werr)
		}

		var cerr error
		cfgStore, cerr = configstore.New(context.Background(), *dsn)
		if cerr != nil {
			fatal("config store", cerr)
		}
		defer cfgStore.Close()
		dbFragment = func() ([]byte, error) {
			doc, _, err := cfgStore.Get(context.Background())
			return doc, err
		}
	}

	// The runtime (jobs + alert engine) is built from config (or the demo set) and
	// held behind an atomic pointer so it can be swapped on SIGHUP reload.
	rt, err := buildRuntime(*configPath, *pings, *step, *timeout, *resolveIPs, notifiers, dbFragment)
	if err != nil {
		fatal("startup failed", err)
	}
	var current atomic.Pointer[runtime]
	current.Store(rt)
	// evalMu serializes a reload's state-inheritance+swap against a round's alert
	// evaluation, so a round that finishes measuring across a reload boundary still
	// updates the live engine (not the abandoned one) — no lost hysteresis state.
	var evalMu sync.Mutex
	// applyMu serializes the WHOLE read/build/swap of a runtime replacement — the SIGHUP
	// reload below and the API config-apply (ConfigApply) both take it — so a slow builder
	// can't swap a stale runtime over a replacement that completed while it was building,
	// leaving the live runtime out of sync with the persisted config (CODE_REVIEW #1).
	// evalMu (inside swapRuntime) only guards the swap; applyMu is strictly outside it.
	var applyMu sync.Mutex
	// seedFn seeds a freshly built engine's alert windows from durable history on a reload,
	// so a target that is newly alerted or redefined isn't left dark until X fresh samples
	// arrive (CODE_REVIEW #4). Assigned once the store exists (below); the SIGHUP goroutine and
	// the API apply both pass it into swapRuntime. It reads the store, so swapRuntime runs it
	// before taking evalMu. nil until assigned — an early reload simply skips seeding.
	var seedFn func(*runtime)
	if *configPath != "" {
		fmt.Printf("config: %d targets from %s\n", len(rt.jobs), *configPath)
	}

	// Cancel in-flight probes and the HTTP server on SIGINT/SIGTERM.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var st store.Store
	var storeMetrics func(*strings.Builder) // persistent-write health, if the store exposes it
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
		storeMetrics = pg.WriteMetrics
	} else {
		st = store.NewMem(1024)
		fmt.Printf("store: in-memory (pass -dsn to persist to TimescaleDB)\n")
	}

	// Now that the store exists, wire the reload seed: on a config reload, swapRuntime seeds
	// the new engine from durable history for targets it can't inherit (redefined or newly
	// alerted), then InheritStateFrom overwrites the seed for same-identity targets (#4).
	seedFn = func(nrt *runtime) {
		warmStartAlerts(ctx, nrt.engine, nrt.monitors, st, time.Now())
	}

	// SIGHUP reloads the config; on error the running config is kept (a bad edit can't take the
	// collector down). Alert firing state and sample windows carry over from the running engine
	// (see InheritStateFrom) so a reload doesn't re-fire alerts already firing or drop hysteresis
	// history. Registered only after seedFn is assigned, so the reload goroutine never races the
	// assignment (its swapRuntime seeds through seedFn).
	if *configPath != "" {
		hup := make(chan os.Signal, 1)
		signal.Notify(hup, syscall.SIGHUP)
		go func() {
			for range hup {
				// Hold applyMu across the whole read/build/swap so a concurrent API apply
				// can't complete (persist+swap) in the window between this reload's build
				// and its swap and then be clobbered by this stale runtime (CODE_REVIEW #1).
				func() {
					applyMu.Lock()
					defer applyMu.Unlock()
					nrt, err := buildRuntime(*configPath, *pings, *step, *timeout, *resolveIPs, notifiers, dbFragment)
					if err != nil {
						slog.Error("reload failed, keeping running config", "err", err)
						return
					}
					swapRuntime(&current, &evalMu, nrt, seedFn)
					slog.Info("config reloaded", "path", *configPath, "targets", len(nrt.jobs))
				}()
			}
		}()
	}

	// Warm-start each alerted target's sample window from durable history, so a target
	// that is already breaching at startup fires on its first new round instead of
	// waiting X fresh samples (the durable-store replacement for SmokePing's S sentinel).
	// Boot uses this directly; a SIGHUP/API reload runs the same seed via seedFn inside
	// swapRuntime and then carries unchanged windows/state via InheritStateFrom.
	if rt := current.Load(); rt.engine != nil {
		warmStartAlerts(ctx, rt.engine, rt.monitors, st, time.Now())
	}

	roundStats := &api.RoundStats{}

	// localStore persists a completed round's outcomes and evaluates their alerts under one live
	// runtime snapshot, serialized against a reload (swapRuntime) via evalMu so a target redefined
	// mid-flight is dropped from BOTH storage and alerting (CODE_REVIEW M2). Used by the warm-up
	// loop and the serving dispatcher's completion callback.
	localStore := func(out []scheduler.Outcome) {
		evalMu.Lock()
		current.Load().storeLocal(st, out)
		evalMu.Unlock()
	}

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
		localStore(out) // store + eval under one snapshot, dropping obsolete-identity rounds (CODE_REVIEW M2)
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
		// Expose extra operational counters on /metrics so they are scrapeable, not
		// merely logged: the store's persistent-write failures, and the webhook delivery
		// counters (queued/delivered/retried/dropped/failed). Composed into one writer.
		var extra []func(*strings.Builder)
		if storeMetrics != nil {
			extra = append(extra, storeMetrics)
		}
		if len(webhookNs) > 0 {
			extra = append(extra, func(b *strings.Builder) { alert.WriteNotifierMetrics(b, webhookNs) })
		}
		if emailN != nil {
			extra = append(extra, emailN.WriteMetrics)
		}
		if len(extra) > 0 {
			srv.ExtraMetrics = func(b *strings.Builder) {
				for _, f := range extra {
					f(b)
				}
			}
		}
		// Live views report only currently-configured targets, so a target removed or
		// renamed on a SIGHUP reload stops showing as healthy (its history stays in
		// the store but ages out via retention / the bounded cap). Built from the full
		// monitor set (all vantages), not just the hub's local jobs, so a remote-only
		// target survives the activeLatest filter for its own vantage (CODE_REVIEW #3 / P1-3).
		srv.Active = func() map[string]bool {
			ms := current.Load().monitors
			m := make(map[string]bool, len(ms))
			for _, mon := range ms {
				m[mon.Name] = true
			}
			return m
		}
		// Per-target step drives /api/sla's coverage (expected rounds = window/step).
		srv.Steps = func() map[string]time.Duration {
			ms := current.Load().monitors
			m := make(map[string]time.Duration, len(ms))
			for _, mon := range ms {
				m[mon.Name] = mon.Step
			}
			return m
		}
		// The configured-target catalog lets /api/targets list a target that has no stored
		// row for the requested vantage yet — chiefly a remote-only target the hub never
		// probes locally — so it appears in the tree and its deep link resolves (P1-3).
		srv.Configured = func() []model.Monitor { return current.Load().monitors }
		srv.TargetMeta = func() map[string]api.TargetMeta { return current.Load().targetMeta }
		// Federation: only with a DB (the vantage key store is TimescaleDB-backed). The
		// agent routes (below) light up unconditionally here; the admin key-management API
		// additionally requires a configured admin password (fail-closed — no password
		// means no admin routes).
		if *dsn != "" {
			vst, err := vantage.New(ctx, *dsn)
			if err != nil {
				fatal("vantage key store", err)
			}
			defer vst.Close()
			srv.Vantages = vst
			// Agent endpoints (/agent/v1/assignment, /agent/v1/results) are independent of
			// the admin API and its password gate below — they light up whenever -dsn is
			// set, since a remote vantage's agent needs to authenticate and report results
			// regardless of whether the (human) admin key-management API is enabled.
			srv.VantageAuth = vst
			// Strict-mode toggle for the ingest path enabled just above: with it on, an agent
			// round carrying no fingerprint is a visible permanent drop instead of accepted
			// (CODE_REVIEW #2). Lenient by default.
			srv.RequireFingerprint = *requireFingerprint
			srv.Assignment = func(v string) ([]model.Monitor, map[string]map[string]string, string) {
				rt := current.Load()
				a := federation.AssignmentFor(rt.monitors, v)
				return a, rt.probeCfgs, federation.ConfigVersion(a, rt.probeCfgs)
			}
			// Evaluate alerts for ingested remote rounds on the live runtime, serialized with
			// the local measure loop's eval and any reload swap via evalMu (P2-5).
			srv.OnIngest = func(out []scheduler.Outcome) {
				evalMu.Lock()
				current.Load().eval(out)
				evalMu.Unlock()
			}
			// Persist + evaluate remote rounds under evalMu — the same reload boundary the local
			// measure loop takes (localStore) — so the identity re-check and the durable write are
			// atomic against a config reload (CODE_REVIEW M4). Supersedes the OnIngest hook above for
			// the ingest path (that hook stays for pure API tests that don't wire this). st is a
			// ResultIngester here (the agent routes only light up with -dsn, i.e. a PGStore); guard
			// the assertion anyway so a future non-ingesting store cleanly falls back to that hook.
			if ing, ok := st.(store.ResultIngester); ok {
				srv.IngestCommit = func(ctx context.Context, out []scheduler.Outcome) ([]scheduler.Outcome, error) {
					evalMu.Lock()
					defer evalMu.Unlock()
					return current.Load().commitRemote(ctx, ing, out)
				}
			}
			srv.TargetVantages = func() map[string][]string {
				ms := current.Load().monitors
				m := make(map[string][]string, len(ms))
				for _, mon := range ms {
					m[mon.Name] = mon.Vantages
				}
				return m
			}
			slog.Info("agent endpoints enabled at /agent/v1/assignment, /agent/v1/results")
			srv.AdminPassword = os.Getenv("SMOKED_ADMIN_PASSWORD")
			adminKey := make([]byte, 32)
			if _, err := rand.Read(adminKey); err != nil {
				fatal("admin key", err)
			}
			srv.AdminKey = adminKey
			if srv.AdminPassword == "" {
				slog.Warn("admin key-management API disabled: set SMOKED_ADMIN_PASSWORD to enable /api/admin/vantages")
			} else {
				slog.Info("admin key-management API enabled at /api/admin/vantages")
			}
			// DB config CRUD (GET/PUT /api/admin/config) requires a base YAML config to
			// merge the DB fragment into (buildRuntime's dbFragment path), so it's gated
			// on -config in addition to the admin password gate above.
			if *configPath != "" {
				srv.ConfigGet = func() (json.RawMessage, int, error) { return cfgStore.Get(context.Background()) }
				// Both API applies (here) and the SIGHUP reload above take the function-scope
				// applyMu around their whole build->(persist)->swap, so no two runtime
				// replacements interleave and leave the live runtime out of sync with the
				// persisted config (CODE_REVIEW #1). evalMu (inside swapRuntime) only guards
				// the swap; applyMu is strictly outside it, so no deadlock.
				srv.ConfigApply = func(doc json.RawMessage, expectedVersion int) error {
					applyMu.Lock()
					defer applyMu.Unlock()
					build := func(getter func() ([]byte, error)) (*runtime, error) {
						return buildRuntime(*configPath, *pings, *step, *timeout, *resolveIPs, notifiers, getter)
					}
					return applyConfig(cfgStore, &current, &evalMu, build, doc, expectedVersion, seedFn)
				}
				// ConfigImport merges a YAML/JSON config's target branches into the current DB
				// fragment (config.AppendImport; globals aren't imported) and, only if it actually
				// added targets, reuses applyConfig's validate->persist->swap under the same
				// applyMu as ConfigApply/the SIGHUP reload above. A merge with nothing new to add
				// is a no-op — no pointless persist/rebuild/swap.
				srv.ConfigImport = func(body []byte) (added, unchanged, version int, err error) {
					applyMu.Lock()
					defer applyMu.Unlock()
					doc, ver, gerr := cfgStore.Get(context.Background())
					if gerr != nil {
						return 0, 0, 0, gerr
					}
					merged, add, unch, ierr := config.AppendImport(doc, body)
					if ierr != nil {
						return 0, 0, ver, fmt.Errorf("%w: %v", api.ErrConfigInvalid, ierr)
					}
					if add == 0 {
						return 0, unch, ver, nil // nothing to apply
					}
					build := func(getter func() ([]byte, error)) (*runtime, error) {
						return buildRuntime(*configPath, *pings, *step, *timeout, *resolveIPs, notifiers, getter)
					}
					if aerr := applyConfig(cfgStore, &current, &evalMu, build, merged, ver, seedFn); aerr != nil {
						return 0, 0, ver, aerr // applyConfig already returns api.ErrConfig* sentinels
					}
					return add, unch, ver + 1, nil
				}
			}
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
						func(o scheduler.Outcome) { // per outcome: identity-gated store + alert eval, in completion order
							localStore([]scheduler.Outcome{o}) // gate storage on the live fingerprint too (CODE_REVIEW M2)
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
			// Drain queued webhook deliveries before the fatal os.Exit, which would otherwise
			// skip the top-level deferred Close. Close is idempotent, so the defer stays a no-op.
			if len(webhookNs) > 0 || emailN != nil {
				drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				for _, n := range webhookNs {
					n.Close(drainCtx)
				}
				if emailN != nil {
					emailN.Close(drainCtx)
				}
				cancel()
			}
			fatal("http server failed", err)
		}
		<-pollDone // wait for the polling goroutine to drain in-flight probes before exiting
		// The webhook notifier is drained by the single deferred Close registered at its
		// creation, which runs on this (and every other) exit path.
		slog.Info("shutdown complete")
		fmt.Println("shutdown complete")
	}
}

// vantageCmd implements `smoked vantage <add NAME|ls|revoke NAME> [-dsn DSN]` — provisioning
// against the same TimescaleDB the daemon uses. The subcommand and the vantage NAME are
// positional (git-style), so flags may follow them: `vantage add nyc -dsn X`. -dsn defaults
// to SMOKED_DSN.
func vantageCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: smoked vantage <add NAME|ls|revoke NAME> [-dsn DSN]")
		return 2
	}
	sub := args[0]
	fs := flag.NewFlagSet("vantage "+sub, flag.ExitOnError)
	dsn := fs.String("dsn", os.Getenv("SMOKED_DSN"), "TimescaleDB DSN (or set SMOKED_DSN)")
	rest := args[1:]
	var name string
	if sub == "add" || sub == "revoke" {
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			fmt.Fprintf(os.Stderr, "usage: smoked vantage %s NAME [-dsn DSN]\n", sub)
			return 2
		}
		name, rest = rest[0], rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "vantage: -dsn (or SMOKED_DSN) is required — federation uses the database")
		return 2
	}
	ctx := context.Background()
	st, err := vantage.New(ctx, *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vantage: %v\n", err)
		return 1
	}
	defer st.Close()

	switch sub {
	case "add":
		key, err := st.Add(ctx, name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "vantage add: %v\n", err)
			return 1
		}
		fmt.Printf("vantage %q key (shown once — store it now):\n\n%s\n\n%s\n", name, key, vantage.AgentSnippet(name, key))
		return 0
	case "ls":
		infos, err := st.List(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "vantage ls: %v\n", err)
			return 1
		}
		fmt.Printf("%-24s %-20s %s\n", "NAME", "CREATED", "LAST-SEEN")
		for _, in := range infos {
			last := "never"
			if !in.LastSeen.IsZero() {
				last = in.LastSeen.UTC().Format(time.RFC3339)
			}
			fmt.Printf("%-24s %-20s %s\n", in.Name, in.Created.UTC().Format("2006-01-02 15:04"), last)
		}
		return 0
	case "revoke":
		removed, err := st.Revoke(ctx, name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "vantage revoke: %v\n", err)
			return 1
		}
		if !removed {
			fmt.Fprintf(os.Stderr, "vantage %q not found\n", name)
			return 1
		}
		fmt.Printf("revoked vantage %q\n", name)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown vantage subcommand %q\n", sub)
		return 2
	}
}

// configCmd implements `smoked config import <file> [-dsn DSN]` — merges a YAML/JSON config's
// target branches into the database config fragment (idempotent; a differing existing target is a
// conflict). Globals (database/probes/alerts) are not imported. -dsn defaults to SMOKED_DSN.
func configCmd(args []string) int {
	const usage = "usage: smoked config import <file> [-dsn DSN] [-config DIR]"
	if len(args) < 1 || args[0] != "import" {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	rest := args[1:]
	if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
		fmt.Fprintln(os.Stderr, usage)
		return 2
	}
	file := rest[0]
	fs := flag.NewFlagSet("config import", flag.ExitOnError)
	dsn := fs.String("dsn", os.Getenv("SMOKED_DSN"), "TimescaleDB DSN (or set SMOKED_DSN)")
	configDir := fs.String("config", "", "effective-validate the merged fragment against this base config "+
		"(default.yaml + conf.d) before persisting; a target relying on inherited probe/params/alerts is checked "+
		"against the real tree instead of AppendImport's context-free checks alone. If omitted, an invalid "+
		"fragment is only caught (and logged) at the daemon's next reload")
	if err := fs.Parse(rest[1:]); err != nil {
		return 2
	}
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "config import: -dsn (or SMOKED_DSN) is required")
		return 2
	}
	fileBytes, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config import: %v\n", err)
		return 1
	}
	ctx := context.Background()
	cs, err := configstore.New(ctx, *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config import: %v\n", err)
		return 1
	}
	defer cs.Close()
	doc, version, err := cs.Get(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config import: %v\n", err)
		return 1
	}
	merged, added, unchanged, err := config.AppendImport(doc, fileBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config import: %v\n", err)
		return 1
	}
	if added == 0 {
		fmt.Printf("nothing to import (%d unchanged)\n", unchanged)
		return 0
	}
	if *configDir != "" {
		if err := effectiveValidate(*configDir, merged); err != nil {
			fmt.Fprintf(os.Stderr, "config import: effective validation against %s failed: %v\n", *configDir, err)
			return 1
		}
	} else {
		fmt.Println("note: fragment not checked against a running config (-config not given); it's instead")
		fmt.Println("      validated when the config is next built: a running daemon's next reload (SIGHUP)")
		fmt.Println("      rejects and logs an invalid fragment, but a COLD START fails to boot on one")
		fmt.Println("      instead — prefer -config to validate up front.")
	}
	if err := cs.Set(ctx, merged, version); err != nil {
		fmt.Fprintf(os.Stderr, "config import: %v (re-run to retry)\n", err)
		return 1
	}
	fmt.Printf("imported %d targets → database config v%d (%d unchanged)\n", added, version+1, unchanged)
	fmt.Println("note: database/probes/alerts are not imported (globals stay in YAML)")
	fmt.Println("note: a target name that also exists in the running YAML config is a duplicate the")
	fmt.Println("      daemon rejects on its next reload — rename or remove it from one source.")
	return 0
}

// effectiveValidate composes a candidate DB config fragment with the base YAML config at
// configDir (default.yaml + conf.d, via LoadPath) and validates the merged result via
// Monitors() — the same composition buildRuntime performs at startup, SIGHUP reload, and every
// API config apply. AppendImport's own checks are deliberately context-free (Finding #6): a
// fragment leaf relying on inherited probe/params/alerts can't be judged without the real base
// config, so this is what actually catches a leaf that still doesn't resolve — no probe
// anywhere, an unknown probe kind, an undefined alert reference, a bad param — once composed.
func effectiveValidate(configDir string, fragment []byte) error {
	base, err := config.LoadPath(configDir)
	if err != nil {
		return fmt.Errorf("loading %s: %w", configDir, err)
	}
	if err := config.AppendDBFragment(base, fragment); err != nil {
		return err
	}
	if _, err := base.Monitors(); err != nil {
		return err
	}
	return nil
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
	monitors        []model.Monitor              // full post-inheritance set, all vantages (for the agent assignment endpoint)
	probeCfgs       map[string]map[string]string // probe kind -> effective probe-level config, served to agents so remote probes match the hub's
	engine          *alert.Engine
	alertsByTarget  map[string][]string
	alerteeByTarget map[string][]string
	// targetFP is each target's current measurement-identity fingerprint (federation.Fingerprint).
	// eval drops an outcome whose fingerprint disagrees — a round measured under an obsolete
	// definition that a reload has since redefined (CODE_REVIEW #4, in-flight completion).
	targetFP map[string]string
	// targetMeta is each target's display-only metadata (title override + IP to show in the
	// title), recomputed on every build so it refreshes on SIGHUP reload.
	targetMeta map[string]api.TargetMeta
}

// eval runs the alert engine over a round's outcomes and dispatches notifications.
// Each outcome is evaluated under its own vantage (local for hub rounds, the agent's
// vantage for ingested remote rounds), so alert state is per measuring location (P2-5).
func (rt *runtime) eval(out []scheduler.Outcome) {
	if rt.engine == nil {
		return
	}
	for _, o := range out {
		names := rt.alertsByTarget[o.Target.Name]
		if len(names) == 0 {
			continue
		}
		// Drop an outcome measured under an obsolete target definition: if a reload redefined
		// this target between when the round was measured and now, the round's stamped
		// fingerprint won't match the live target's, and evaluating it would feed a stale
		// measurement into the new alert identity (CODE_REVIEW #4).
		if rt.fingerprintStale(o) {
			continue
		}
		events := rt.engine.Evaluate(o.Target.Name, store.VantageOf(o), names,
			o.Computed.LossFraction()*100, o.Computed.Median, o.When)
		rt.engine.Dispatch(events, rt.alerteeByTarget[o.Target.Name]...)
	}
}

// fingerprintStale reports whether a locally-measured outcome was measured under a target
// definition a reload has since replaced: its non-empty fingerprint no longer matches the live
// target's. An empty fingerprint — a path predating stamping — is never stale, matching the
// ingest side's transitional rule.
func (rt *runtime) fingerprintStale(o scheduler.Outcome) bool {
	return o.Fingerprint != "" && rt.targetFP[o.Target.Name] != o.Fingerprint
}

// storeLocal persists locally-measured outcomes and evaluates their alerts under THIS runtime
// snapshot, dropping any measured under an obsolete target identity so a redefined target's
// in-flight round is neither stored nor alerted. The caller holds evalMu so a concurrent
// swapRuntime can't change the identity between the check and the write. The remote ingest path
// already gates storage this way before st.Add (internal/api/agent.go); this closes the same gap
// for local probes, whose completion previously stored before the fingerprint check (CODE_REVIEW M2).
func (rt *runtime) storeLocal(st store.Store, out []scheduler.Outcome) {
	stale := false
	for _, o := range out {
		if rt.fingerprintStale(o) {
			stale = true
			break
		}
	}
	kept := out
	if stale { // allocate only when a reload actually invalidated something
		kept = make([]scheduler.Outcome, 0, len(out))
		for _, o := range out {
			if !rt.fingerprintStale(o) {
				kept = append(kept, o)
			}
		}
	}
	if len(kept) == 0 {
		return
	}
	st.Add(kept)
	rt.eval(kept)
}

// commitRemote durably persists and alert-evaluates a batch of remote outcomes that the ingest
// handler already validated against a runtime SNAPSHOT, re-checking each against THIS (the live)
// runtime's target identities first. The caller holds evalMu, so between this re-check and the write
// no swapRuntime can redefine a target — closing the window in which a reload landing after the
// handler's snapshot validation could store a round under a since-redefined target (CODE_REVIEW M4).
// It is the ingest-path analog of storeLocal, but uses the ResultIngester so a replayed round is
// deduplicated (returning only the newly-inserted rounds, which are the only ones alert-evaluated).
func (rt *runtime) commitRemote(ctx context.Context, ing store.ResultIngester, out []scheduler.Outcome) ([]scheduler.Outcome, error) {
	kept := make([]scheduler.Outcome, 0, len(out))
	for _, o := range out {
		// Drop a round whose target a reload has since removed (absent from targetFP) or redefined
		// (fingerprintStale) — the same identity gate storeLocal and eval apply, now enforced under
		// the lock at write time rather than only against the handler's earlier snapshot.
		if _, ok := rt.targetFP[o.Target.Name]; !ok {
			continue
		}
		if rt.fingerprintStale(o) {
			continue
		}
		kept = append(kept, o)
	}
	if dropped := len(out) - len(kept); dropped > 0 {
		// A reload redefined/removed these targets between the handler's snapshot validation and
		// this write, so the rounds are dropped at commit rather than stored under a stale identity.
		// Log it (mirroring the handler's snapshot-time drop warning) so the event isn't silent.
		slog.Warn("agent ingest: dropped rounds at commit; target redefined or removed by a reload", "dropped", dropped)
	}
	if len(kept) == 0 {
		return nil, nil
	}
	inserted, err := ing.AddResults(ctx, kept)
	if err != nil {
		return nil, err
	}
	rt.eval(inserted)
	return inserted, nil
}

// swapRuntime atomically installs nrt as the live runtime, carrying alert firing
// state + sample windows over from the running engine (so a reload/apply doesn't
// re-fire alerts already firing or drop hysteresis history). Serialized against a
// round's alert eval via evalMu. Shared by the SIGHUP reload and the config-apply API.
func swapRuntime(current *atomic.Pointer[runtime], evalMu *sync.Mutex, nrt *runtime, seed func(*runtime)) {
	// Seed the new engine's windows from durable history BEFORE the swap and OUTSIDE evalMu.
	// A target whose measurement identity changed, or one that just gained its first alert,
	// won't inherit a window below — without seeding it would start dark and fire late
	// (CODE_REVIEW #4). recentContiguous filters the seed to the current host/probe, so a
	// redefined target pulls nothing stale. nrt.engine isn't live yet, so this needs no lock,
	// keeping the store read off the evalMu critical path. Inherit then overwrites the seed for
	// same-identity targets with their live window.
	if seed != nil && nrt.engine != nil {
		seed(nrt)
	}
	evalMu.Lock()
	old := current.Load()
	if nrt.engine != nil && old.engine != nil {
		nrt.engine.InheritStateFrom(old.engine, reloadIdentity(old, nrt))
	}
	current.Store(nrt)
	evalMu.Unlock()
}

// reloadIdentity builds the per-target/per-alert identity InheritStateFrom uses to decide what
// survives a reload: which targets exist (ValidTarget) with an unchanged measurement identity
// (SameTarget), which alerts are attached to each target now (Attached), and whether each
// target's alertee recipients are unchanged (SameAlertee). The alert's own To recipients and
// matcher identity are compared inside the engine (CODE_REVIEW #3/#4).
func reloadIdentity(old, nrt *runtime) alert.ReloadIdentity {
	valid := make(map[string]bool, len(nrt.alertsByTarget))
	attached := make(map[string]map[string]bool, len(nrt.alertsByTarget))
	sameAlertee := make(map[string]bool, len(nrt.alertsByTarget))
	for t, names := range nrt.alertsByTarget {
		valid[t] = true
		set := make(map[string]bool, len(names))
		for _, n := range names {
			set[n] = true
		}
		attached[t] = set
		sameAlertee[t] = sameStringSet(old.alerteeByTarget[t], nrt.alerteeByTarget[t])
	}
	return alert.ReloadIdentity{
		ValidTarget: valid,
		SameTarget:  sameTargetIdentity(old, nrt),
		Attached:    attached,
		SameAlertee: sameAlertee,
	}
}

// sameStringSet reports whether two string slices hold the same elements, ignoring order —
// used to compare a target's alertee recipients across a reload.
func sameStringSet(x, y []string) bool {
	if len(x) != len(y) {
		return false
	}
	xs := append([]string(nil), x...)
	ys := append([]string(nil), y...)
	sort.Strings(xs)
	sort.Strings(ys)
	for i := range xs {
		if xs[i] != ys[i] {
			return false
		}
	}
	return true
}

// sameTargetIdentity maps target name -> whether its measurement identity is unchanged between
// the old and new runtime, using the same federation.Fingerprint (probe/host/params/pings/
// probe-config) the ingest and in-flight-drop paths key on — so the reload inherit-gate can't
// disagree with them. A target that is new, removed, or redefined in ANY of those fields is
// absent (false), so InheritStateFrom carries neither its window nor its firing/visible state;
// it is seeded fresh from durable history instead (CODE_REVIEW #4).
func sameTargetIdentity(old, nrt *runtime) map[string]bool {
	same := make(map[string]bool, len(nrt.targetFP))
	for name, fp := range nrt.targetFP {
		if o, ok := old.targetFP[name]; ok && o == fp {
			same[name] = true
		}
	}
	return same
}

// applyConfig validates a candidate DB config fragment by building a runtime from it
// (YAML + this doc), persists it with optimistic concurrency, then swaps the built
// runtime in. Validate → persist → swap: an invalid doc never persists, a stale
// version never swaps. Returns api.ErrConfigInvalid / api.ErrConfigConflict.
func applyConfig(cfgStore *configstore.Store, current *atomic.Pointer[runtime], evalMu *sync.Mutex,
	build func(dbFragment func() ([]byte, error)) (*runtime, error), doc json.RawMessage, expectedVersion int, seed func(*runtime)) error {
	nrt, berr := build(func() ([]byte, error) { return doc, nil })
	if berr != nil {
		return fmt.Errorf("%w: %v", api.ErrConfigInvalid, berr)
	}
	if serr := cfgStore.Set(context.Background(), doc, expectedVersion); serr != nil {
		if errors.Is(serr, configstore.ErrConflict) {
			return api.ErrConfigConflict
		}
		return serr
	}
	swapRuntime(current, evalMu, nrt, seed)
	slog.Info("config applied via API", "targets", len(nrt.jobs))
	return nil
}

// warmMeta is the current target identity used to decide which stored history may seed a
// target's alert window: only rounds matching the current host+probe and cadence count.
type warmMeta struct {
	host  string
	probe string
	step  time.Duration
}

// warmStartLookback bounds how far back a remote vantage's warm-start history read goes.
// SeedWindow trims to the deepest matcher window anyway; this just caps the query.
const warmStartLookback = 24 * time.Hour

// warmStartAlerts seeds each alerted target's per-vantage alert window from that vantage's
// recent history (loss% and rtt median per round, oldest->newest — the same values eval
// feeds Evaluate), so an already-breaching target fires immediately after boot. It reads
// the matching vantage: the hub's own "local" via History, every remote vantage via
// HistorySince, so a remote outage warm-starts too (P2-5). Best-effort: a (target,vantage)
// with no history, a read error, or a store that can't range-read remote vantages is skipped.
func warmStartAlerts(ctx context.Context, engine *alert.Engine, monitors []model.Monitor, st store.Store, now time.Time) {
	rh, _ := st.(store.RangeHistorier)
	for _, m := range monitors {
		if len(m.Alerts) == 0 {
			continue
		}
		meta := warmMeta{host: m.Host, probe: m.ProbeKind, step: m.Step}
		for _, v := range m.Vantages {
			var hist []scheduler.Outcome
			var err error
			if v == store.DefaultVantage {
				hist, err = st.History(m.Name)
			} else if rh != nil {
				hist, err = rh.HistorySince(ctx, m.Name, v, now.Add(-warmStartLookback))
			} else {
				continue
			}
			if err != nil || len(hist) == 0 {
				continue
			}
			// Seed only the recent, cadence-contiguous suffix matching the current host/probe,
			// so stale or semantically-different history can't satisfy a consecutive-sample
			// matcher (e.g. two bad samples from months ago + one bad post-restart sample) (#6).
			suffix := recentContiguous(hist, meta, now)
			if len(suffix) == 0 {
				continue
			}
			loss := make([]float64, len(suffix))
			rtt := make([]float64, len(suffix))
			for i, o := range suffix {
				loss[i] = o.Computed.LossFraction() * 100
				rtt[i] = o.Computed.Median // NaN for a lost round
			}
			engine.SeedWindow(m.Name, v, m.Alerts, loss, rtt)
		}
	}
}

// recentContiguous returns the newest run of rounds (oldest->newest) that all match the
// current host+probe (m) and are cadence-contiguous — no neighbor gap larger than 2×step —
// ending at a round recent enough to be "current" (within 2×step of now). If the newest
// stored round is stale or from a different host/probe, it returns nil: the target is
// effectively dark, and seeding old data would fire a false alert at boot.
func recentContiguous(hist []scheduler.Outcome, m warmMeta, now time.Time) []scheduler.Outcome {
	if len(hist) == 0 {
		return nil
	}
	gap := 2 * m.step
	if gap <= 0 {
		gap = 2 * time.Minute // unknown cadence: a conservative fallback
	}
	last := hist[len(hist)-1] // History returns oldest->newest
	if now.Sub(last.When) > gap || last.Target.Host != m.host || last.ProbeName != m.probe {
		return nil
	}
	start := len(hist) - 1
	for start > 0 {
		cur, prev := hist[start], hist[start-1]
		if prev.Target.Host != m.host || prev.ProbeName != m.probe || cur.When.Sub(prev.When) > gap {
			break
		}
		start--
	}
	return hist[start:]
}

// unresolvedRecipients returns the recipient names referenced by any alert's `to` or any target's
// `alertee` that have no matching enabled notifier — a typo, or `webhook` configured without
// `-webhook`. Deduped and sorted. buildRuntime logs these at startup and on every reload so a
// misrouted alert is visible up front, instead of only when the incident whose notification is
// silently dropped finally happens (CODE_REVIEW L2).
func unresolvedRecipients(alertDefs map[string]*alert.Alert, alerteeByTarget map[string][]string, notifiers map[string]alert.Notifier) []string {
	missing := map[string]bool{}
	check := func(recips []string) {
		for _, r := range recips {
			if _, ok := notifiers[r]; !ok {
				missing[r] = true
			}
		}
	}
	for _, a := range alertDefs {
		check(a.To)
	}
	for _, recips := range alerteeByTarget {
		check(recips)
	}
	out := make([]string, 0, len(missing))
	for r := range missing {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// buildRuntime loads targets (from YAML config, or the demo set) and builds the
// probe jobs and alert engine. A probe whose binary/deps are unavailable is
// skipped with a warning, not fatal.
func buildRuntime(configPath string, demoPings int, demoStep, timeout time.Duration, resolveIPs bool, notifiers map[string]alert.Notifier, dbFragment func() ([]byte, error)) (rt *runtime, err error) {
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
		if dbFragment != nil {
			fragBytes, ferr := dbFragment()
			if ferr != nil {
				return nil, fmt.Errorf("config: database fragment: %w", ferr)
			}
			if err := config.AppendDBFragment(cfg, fragBytes); err != nil {
				return nil, fmt.Errorf("config: %w", err)
			}
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

	// The hub probes only the targets assigned to its own vantage (local). A remote-only
	// target (e.g. `vantages: [nyc]`) stays dark until its agent connects, rather than being
	// silently measured here and stored/alerted as a `local` observation (a false location).
	// Config monitors already default to [local]; demo monitors carry no vantages, so default
	// them first, then filter. This scopes both the probe jobs and the alert loops below.
	for i := range monitors {
		if len(monitors[i].Vantages) == 0 {
			monitors[i].Vantages = []string{store.DefaultVantage}
		}
	}
	// Keep the full post-inheritance set (all vantages) for the agent assignment endpoint,
	// while the hub itself probes + alerts only its own vantage below.
	fullMonitors := append([]model.Monitor(nil), monitors...)
	localMonitors := federation.AssignmentFor(monitors, store.DefaultVantage)

	probes := map[string]probe.Probe{}
	var jobs []scheduler.Job
	for _, m := range localMonitors {
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
			// Stamp the local job with its measurement identity so eval can drop an in-flight
			// round whose target a reload has since redefined (CODE_REVIEW #4). Same fingerprint
			// the assignment endpoint stamps for remote agents, so both paths agree.
			Fingerprint: federation.Fingerprint(m, probeCfgs[m.ProbeKind]),
		})
	}

	// Alert maps cover the full monitor set (all vantages), not just local jobs: remote
	// outcomes are now evaluated per-vantage on ingest, and eval is vantage-scoped, so a
	// remote-only target's alerts fire on its own data without the hub ever probing it
	// locally (P2-5). The local measure loop only ever feeds local outcomes, so including
	// remote targets here cannot make the hub alert them against local data.
	alertsByTarget := map[string][]string{}
	alerteeByTarget := map[string][]string{}
	for _, m := range fullMonitors {
		if len(m.Alerts) > 0 {
			alertsByTarget[m.Name] = m.Alerts
		}
		if len(m.Alertee) > 0 {
			alerteeByTarget[m.Name] = m.Alertee
		}
	}
	if miss := unresolvedRecipients(alertDefs, alerteeByTarget, notifiers); len(miss) > 0 {
		known := make([]string, 0, len(notifiers))
		for n := range notifiers {
			known = append(known, n)
		}
		sort.Strings(known)
		slog.Warn("alert recipients reference unknown notifiers; their notifications will be dropped until the notifier is enabled",
			"unresolved", miss, "known", known)
	}
	var engine *alert.Engine
	if len(alertDefs) > 0 {
		engine = alert.NewEngine(alertDefs, notifiers)
	}
	// Per-target measurement fingerprint over the full monitor set (all vantages), so eval can
	// verify a completing outcome still matches its target's current identity (CODE_REVIEW #4).
	targetFP := make(map[string]string, len(fullMonitors))
	for _, m := range fullMonitors {
		targetFP[m.Name] = federation.Fingerprint(m, probeCfgs[m.ProbeKind])
	}
	return &runtime{jobs: jobs, monitors: fullMonitors, probeCfgs: probeCfgs, engine: engine,
		alertsByTarget: alertsByTarget, alerteeByTarget: alerteeByTarget, targetFP: targetFP,
		targetMeta: buildTargetMeta(fullMonitors, resolveIPs)}, nil
}

// buildTargetMeta computes each target's display metadata: its title override (always) and
// the IP to show in the graph title (only when resolveIPs is set — a pinned ip: or a
// literal-IP host need no DNS; a hostname is resolved best-effort). Only targets with a
// title or IP get an entry. Recomputed on every build, so a SIGHUP reload re-resolves.
func buildTargetMeta(monitors []model.Monitor, resolveIPs bool) map[string]api.TargetMeta {
	var lookup func(string) []string
	if resolveIPs {
		resolved := resolveHosts(hostsToResolve(monitors))
		lookup = func(h string) []string { return resolved[h] }
	}
	meta := make(map[string]api.TargetMeta, len(monitors))
	for _, m := range monitors {
		md := api.TargetMeta{Title: m.Title}
		if resolveIPs {
			md.IP = config.DisplayIP(m, lookup)
		}
		if md.Title != "" || md.IP != "" {
			meta[m.Name] = md
		}
	}
	return meta
}

// hostsToResolve returns the distinct hostnames that need a DNS lookup for display — those
// with no pinned ip: and a non-literal-IP host. (Pinned IPs and literal-IP hosts are shown
// as-is.)
func hostsToResolve(monitors []model.Monitor) []string {
	seen := map[string]bool{}
	var hosts []string
	for _, m := range monitors {
		if m.IP != "" || m.Host == "" || net.ParseIP(m.Host) != nil {
			continue
		}
		if !seen[m.Host] {
			seen[m.Host] = true
			hosts = append(hosts, m.Host)
		}
	}
	return hosts
}

// resolveHosts resolves each hostname to its IP addresses concurrently (bounded), best
// effort: a failed or slow lookup just yields no entry, so display resolution never blocks
// startup/reload for long. Bounded per-host and overall so many targets can't stall a build.
func resolveHosts(hosts []string) map[string][]string {
	const perHost, overall = 2 * time.Second, 5 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), overall)
	defer cancel()
	out := make(map[string][]string, len(hosts))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for _, h := range hosts {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			hctx, hcancel := context.WithTimeout(ctx, perHost)
			defer hcancel()
			addrs, err := net.DefaultResolver.LookupHost(hctx, h)
			if err != nil || len(addrs) == 0 {
				return
			}
			mu.Lock()
			out[h] = addrs
			mu.Unlock()
		}(h)
	}
	wg.Wait()
	return out
}

func demoMonitors(pings int, step time.Duration) []model.Monitor {
	return []model.Monitor{
		// Leaf names describe the PROBE METHOD, not "DNS" — these ICMP pings etc. hit a DNS
		// server's IP but aren't DNS queries (only the "DNS query" targets below are).
		{Name: "Cloudflare ICMP (FPing)", ProbeKind: "FPing", Host: "1.1.1.1", Pings: pings, Step: step},
		{Name: "Cloudflare ICMP (native)", ProbeKind: "Ping", Host: "1.1.1.1", Pings: pings, Step: step},
		{Name: "Google ICMP (FPing)", ProbeKind: "FPing", Host: "8.8.8.8", Pings: pings, Step: step},
		{Name: "Google ICMP (native)", ProbeKind: "Ping", Host: "8.8.8.8", Pings: pings, Step: step},
		{Name: "localhost ICMP (FPing)", ProbeKind: "FPing", Host: "127.0.0.1", Pings: pings, Step: step},
		{Name: "Cloudflare TCP :443", ProbeKind: "TCPConnect", Host: "1.1.1.1", Pings: pings, Step: step, Params: map[string]string{"port": "443"}},
		{Name: "Google TCP :443", ProbeKind: "TCPConnect", Host: "www.google.com", Pings: pings, Step: step, Params: map[string]string{"port": "443"}},
		{Name: "Unreachable :9 (TCP, expect loss)", ProbeKind: "TCPConnect", Host: "192.0.2.1", Pings: pings, Step: step, Params: map[string]string{"port": "9"}},
		{Name: "Cloudflare DNS query", ProbeKind: "DNS", Host: "1.1.1.1", Pings: pings, Step: step, Params: map[string]string{"lookup": "example.com"}},
		{Name: "Google DNS query", ProbeKind: "DNS", Host: "8.8.8.8", Pings: pings, Step: step, Params: map[string]string{"lookup": "example.com"}},
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
