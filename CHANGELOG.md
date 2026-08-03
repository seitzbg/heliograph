# Changelog

All notable changes to **smokeping-modern** are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/); this project is pre-1.0 and under
active development, so everything currently lives under _Unreleased_.

## [Unreleased]

### Added — MVP collector core (2026-08-03)
- Plugin `Probe` interface + registry (`internal/probe`); probes self-register via `init()`.
- Six probes (the lean v1 set), all live-tested: `FPing` (fping wrapper), `TCPConnect`
  (native), `DNS` (miekg/dns), `HTTP` (net/http + httptrace TTFB), `SSH` (native banner
  timing), `IRTT` (irtt(1) UDP round-trip/jitter wrapper).
- Parallel scheduler (`internal/scheduler`): bounded goroutine worker pool, per-target
  timeout and isolation (one hung target cannot block the round). Phase-aligned `NextDelay`.
- SmokePing sample math (`internal/sample`): median, loss (= missing samples), and the
  "centered" array that renders smoke bands symmetrically. Unit-tested.
- JSON API (`internal/api`): `/api/targets`, `/api/series`, `/api/probes`; static file
  serving so the dashboard is same-origin.
- Client-side canvas smoke renderer (`web/smoke.js`) + live dashboard (`web/index.html`,
  fetches `/api/series`, theme-aware) + self-contained synthetic POC (`web/smoke-poc.html`).
- `cmd/smoked` collector binary with `-rounds/-pings/-workers/-step/-timeout/-serve/-addr`.

### Added — Persistence, config, alerting (2026-08-03)
- TimescaleDB store (`internal/store/pgstore`) behind a `store.Store` interface (`MemStore`
  remains the in-memory default). One row per round in a `samples` hypertable, keeping the
  raw per-round sample array (loss gaps as SQL `NULL`). `-dsn` flag. Verified vs a live DB.
- YAML target-tree config (`internal/config`, `config.example.yaml`) with inheritance of
  `probe`/`pings`/`step`/`params`/`alerts` down the tree; each leaf validated against its
  probe's `Schema()`. `-config` flag.
- Alert engine (`internal/alert`): `CheckLoss`/`CheckLatency` hysteresis matchers + a comma
  pattern DSL (`>50%,>50%` / `>200,>200`); firing/resolved state with edge-triggering; `log`
  and `webhook` notifiers. `-webhook` flag. Verified firing live on real loss and latency.

### Fixed
- `.gitignore` pattern `smoked` was excluding the `cmd/smoked` source directory, so the
  first push could not build; narrowed to `/smoked` (the root-built binary only).
- IRTT parser: irtt emits `lost` as the JSON string `"false"` (not a boolean); the parser
  and its test fixture were corrected after a live run exposed 100% phantom loss.
- Config validation no longer requires probe binaries to be installed: a registered probe
  whose external binary is absent (e.g. `fping` on a CI image) is allowed through — the
  collector already skips it at runtime with a warning. Only an unknown probe kind is a
  config error. (Caught by CI, which lacks `fping`.)

### Added — Phase 3 (production-readiness) (2026-08-03)
- TimescaleDB downsampling: `pgstore.EnableDownsampling` creates an hourly continuous
  aggregate (median avg/min/max + loss per target/hour — the coarse "RRA" tier), a refresh
  policy, and a 30-day retention policy on raw samples. `-downsample` flag. Idempotent;
  integration-tested against a live DB.
- `Dockerfile` (multi-stage; ships `fping`) + `docker-compose.yml` (TimescaleDB + smoked,
  one-command bring-up) + `.dockerignore`. Image builds and runs (verified).
- Verified the live dashboard renders **from TimescaleDB** (not just the in-memory store),
  including the loss-colored median on real persisted loss data.
- Graceful shutdown: `smoked` cancels in-flight probes and shuts the HTTP server down
  cleanly on SIGINT/SIGTERM (signal-aware context). Verified (clean exit 0 on SIGTERM).
- Prometheus `/metrics` endpoint (dependency-free text exposition): per-target/probe
  `smokeping_probe_median_seconds`, `smokeping_probe_loss_ratio`, `smokeping_probe_up`.
  Lets Grafana/Alertmanager-native setups scrape and alert. Unit-tested + live-verified.
- Config reload on SIGHUP: the runtime (jobs + alert engine) is held behind an atomic
  pointer and rebuilt from the config on SIGHUP; a bad edit keeps the running config.
  Verified live (added a target, SIGHUP, it began measuring). Note: alert firing state
  currently resets on reload.
- `/api/rollup?target=…` endpoint: hourly downsampled buckets (median avg/min/max + loss)
  from the continuous aggregate, with real-time aggregation so the current hour is
  included. Returns 501 on the in-memory store. Verified live against TimescaleDB.

### CI
- GitLab CI pipeline (`.gitlab-ci.yml`): `go vet` + `go build` + `go test` on `golang:1.26`,
  with a cached module directory, on the `ubuntu-26.04-amd64` runner tag. No DB service
  needed (the pgstore integration test skips without `SMOKE_TEST_DSN`).

### Docs
- `CHANGELOG.md` and `ROADMAP.md` added to track progress and the plan.
- Full re-implementation reference / code map maintained outside the repo at
  `~/.claude/plans/smokeping-codemap/`.
