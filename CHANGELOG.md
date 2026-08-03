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

### Docs
- `CHANGELOG.md` and `ROADMAP.md` added to track progress and the plan.
- Full re-implementation reference / code map maintained outside the repo at
  `~/.claude/plans/smokeping-codemap/`.
