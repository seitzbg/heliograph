# Roadmap

The plan for **Heliograph** — a modern, non-Perl reimplementation of SmokePing.
Legend: ✅ done · 🚧 in progress · ⬜ todo.

## Guiding requirements
1. **Keep the smoke-graph look & feel** — band distribution + loss-colored median. ✅ (canvas renderer, verified light/dark)
2. **Fast, highly parallel poller** — thousands of targets, per-target timeout, one slow target never blocks others. ✅ (goroutine worker pool)
3. **Probes as plugins** — add a probe without touching the core. ✅ (Probe interface + registry)

## Phase 0 — Analysis ✅
- ✅ Full code map of SmokePing 2.9.0 + modernization blueprint (kept as external design notes).

## Phase 1 — MVP collector core ✅
- ✅ Probe plugin interface + registry
- ✅ Probes: FPing, TCPConnect, DNS, HTTP, SSH, IRTT
- ✅ Parallel scheduler with per-target isolation
- ✅ Sample math (median/loss/centered smoke array)
- ✅ JSON API + static serving
- ✅ Canvas smoke renderer + live dashboard + synthetic POC

## Phase 2 — Persistence, config, alerting ✅
- ✅ TimescaleDB store (raw per-round samples) behind a `store.Store` interface
- ✅ YAML target tree with inheritance + per-probe schema validation
- ✅ Alert engine (hysteresis matchers + pattern DSL, edge-trigger, log/webhook/Slack/Discord/email notifiers)

## Phase 3 — Production-readiness ✅
- ✅ CI pipeline (GitLab CI: vet + build + test) — green on the `ubuntu-26.04-amd64` runner
- ✅ Live dashboard verified rendering from the TimescaleDB store (raw-sample path)
- ✅ Continuous-aggregate downsampling tier (hourly) + refresh + 30-day retention policy
- ✅ API serves downsampled long ranges — `/api/rollup` (hourly) done; SPA range selector switches raw ↔ hourly (min/avg/max band; degrades gracefully when no TimescaleDB)
- ✅ Dockerfile + compose (smoked + TimescaleDB) for one-command bring-up
- ✅ Graceful shutdown (SIGINT/SIGTERM: cancel in-flight probes + clean HTTP shutdown)
- ✅ Config reload without restart (SIGHUP; atomic runtime swap, keeps running config on error)
- ✅ Structured logging (slog; `-log-format` text/json) + operational metrics (round duration/size/errors + per-probe timings on `/metrics`)
- ✅ Preserve alert firing state across config reload (reload reconciles by identity: it inherits firing state + sample windows for unchanged targets/matchers so alerts don't re-fire or lose hysteresis, seeds newly-alerted or redefined targets from durable history, and drops in-flight rounds measured under an obsolete definition — see CHANGELOG "reconciled by identity, not names")

## Phase 4 — Federation (multi-vantage) ✅
The hub, the `smoke-agent` remote collector, the per-vantage overlay UI, the Vantages admin
panel, the bundled reverse proxy, and the operator guide are all in — the feature is complete.

Design (settled): the hub **assigns** work — agents pull a strict, schema-validated assignment
(no eval of server-sent config); targets declare `vantages: [...]` (inherited, default
`[local]`); transport is **HTTPS/JSON with a per-vantage API key** behind a **required reverse
proxy** (a bundled Caddy with Let's Encrypt, or your own) — superseding the earlier gRPC+mTLS
sketch; agents poll a versioned assignment (`304` when unchanged); keys are managed by a
`smoked vantage` CLI and a password-gated admin API. Federation requires `-dsn`.

- ✅ Per-round vantage dimension in the store (the hub probes as `local`)
- ✅ `vantages:` config (inherited down the tree) + a pure per-vantage assignment builder + a
  content-version hash for the `304` check
- ✅ Vantage API-key store (salted-hash, constant-time verify) + `smoked vantage add/ls/revoke`
  CLI + a password-gated `/api/admin/vantages` API with a session login
- ✅ Agent-facing endpoints: `GET /agent/v1/assignment` (304-aware, carries effective probe-level
  config) + `POST /agent/v1/results` (API-key auth, validated + idempotent ingest); vantage-aware
  storage reads + continuous aggregates (default `local`) so multi-vantage data no longer
  conflates; remote rounds are alert-evaluated per vantage after durable ingest
- ✅ `smoke-agent` binary: pull assignment → probe → push results + in-memory store-and-forward
- ✅ Per-vantage overlay graphs (the `~slave` equivalent) in the detail views — a median line per
  vantage, the smoke band on the focused one, a chip legend/selector
- ✅ Vantages admin GUI panel — a login-gated tab to add / list / regenerate / revoke vantages and copy the agent snippet (one-time key reveal); `/api/admin/vantages` reports a per-vantage target count
- ✅ Bundled Caddy (Let's Encrypt HTTP-01/DNS-01, `federation` compose profile, dashboard behind
  Basic Auth) + documented external-reverse-proxy deployment
- ✅ Docs: README federation-deployment section + an operator guide (`docs/federation.md`,
  provision a vantage end-to-end)

## Phase 5 — Parity & polish 🚧
- ✅ Richer alert DSL — pattern `*N*` skip, bare `*`, `U` token, priority suppression,
  per-target `alertee` (all done)
  - Priority: `priority: 1` (highest) inhibits lower-priority alerts on the same target
    while firing; unset (0) is never inhibited (Alertmanager-style inhibition). A RESOLVED
    is emitted only if the matching FIRING was actually delivered — a suppressed alert stays
    fully silent (no orphan RESOLVED), and one delivered before being inhibited still gets
    its close-out. `alertee` adds per-target recipients, inherited down the tree and deduped
    against each alert's `to` at dispatch.
  - ✅ Both Perl bugs avoided (table-driven tests): `*N*` alignment is correct
    (`>0%,*12*,>0%` matches; patterns are right-anchored to the newest sample and the
    window retains the full skip allowance), and `==U` fires on a lost round's unknown
    rtt, kept distinct from 0% loss (an errored round is 100% loss; a lost round's median
    is NaN = U; an unavailable probe is a gap — never silently 0%).
  - `S` (startup sentinel): dropped by design, not implemented. It only existed because
    SmokePing's alert state was in-memory; the durable store answers "already bad at
    startup" from real history (blueprint §07). `==S`/`!=S` are rejected at parse time.
    Follow-up for the same use case: warm-start the alert window from the store on boot —
    done ✅ (`Engine.SeedWindow`; an already-breaching target fires on its first post-boot
    round instead of waiting X fresh samples).
- ✅ Emit each probe's config as JSON Schema (`GET /api/probes/schema`; draft 2020-12,
  probe-level + per-target vars as separate closed objects, from the same VarSpec source
  as runtime validation)
- 🚧 Overview (small multiples) + multi-range detail (3h/30h/10d/400d) done ✅ (Overview
  tab + per-target 3h/30h/10d/400d drill-down); unison scaling done ✅ (shared Y-axis
  across the Graphs grid, toggle in the legend)
- 🚧 Top-N "charts" (worst by loss/median/stddev) done ✅ (`/api/charts` + dashboard
  "Worst targets" panel); config-tree menu done ✅ (Graphs-view left nav built from the
  target name paths: collapsible folders with worst-child status dots + subtree counts,
  a filter box, deep-linkable subtree scope `#graphs&path=<folder>` that scopes the grid
  and its unison Y-axis; a leaf opens the target drill-down). Other pivots (by-probe,
  by-status, flat A–Z) intentionally not built — the config tree is the workhorse and
  by-status would duplicate the Overview tab (see the decision log); revisit only if asked.
- ✅ **Author-defined menu order** — config nodes (YAML and DB fragments) can now carry an optional
  `weight:` (int); siblings sort by `(weight, name)` instead of strict A–Z — a negative weight pins
  a node to the top of its group, unset/`0` preserves today's alphabetical default (backward-compatible).
  Honored by the config flatten, `/api/targets`, the dashboard menu, and the grid. SmokePing ordered
  menus by file position; this is the DB-config equivalent. The Config-tab **tree UI with drag-to-reorder among siblings** now ships (PR B); follow-ups: keyboard-accessible reorder, cross-folder move.
- ✅ **Show the resolved IP in the graph title** — done: a target carries an optional `title:`
  (display-name override) and, with `-resolve-ips` / `SMOKED_RESOLVE_IPS`, its IP in the graph header —
  a pinned `ip:`, a literal-IP host, or the hostname resolved at config-load (best-effort, concurrent,
  refreshed on SIGHUP reload). The header reads `<probe> <title-or-name> (<ip>)`. Both fields are
  display-only (kept out of the measurement fingerprint, so a label/IP edit never resets a series).
  Opt-in (default off); the bundled Compose demo enables it.
- ✅ Drag-to-zoom time range — done: drag a range on the detail graph and it refetches
  that `[from,to]` at the resolution best for the span (raw/hourly/daily), not an image
  swap. In-app dark-mode toggle persistence done ✅ (localStorage, restored before first paint)
- ✅ Prometheus metrics export (`/metrics`: median/loss/up per target) for Grafana-native users
- ✅ Availability/SLA reporting (the tSmoke equivalent) — `GET /api/sla?window=…` per-target
  availability % over a window + dashboard "Availability" panel

## Phase 6 — Beyond parity 🚧
Post-federation work, in build order (each an independent PR). Federation (Phase 4) and these
Phase-6 items all ship in the single **v1.0** line.

- ✅ **Config in a database** — targets/probes/alerts were YAML-only; config now also lives in the
  store with an in-browser **target-management UI** (add / edit / remove), reusing the admin auth.
  Built **additively**: the DB is a live source concatenated with YAML (`conf.d`-style), not a
  replacement. The marquee feature — goes past SmokePing's file-only config. *(Large; 4 slices, all shipped.)*
  - ✅ DB config source + load — a versioned `config_fragment` (`internal/configstore`,
    optimistic-concurrency `Get`/`Set`) merged into the config tree on boot/SIGHUP when `-dsn` is
    set; dark until configured
  - ✅ Config CRUD API — admin-gated `GET`/`PUT /api/admin/config` (whole-doc, optimistic
    concurrency) that validates the candidate config, persists it, and hot-reloads atomically
  - ✅ Target-management UI — a login-gated **Config** tab: list DB targets, add/edit/remove via a
    modal (read-modify-write the fragment through the CRUD API), with 400/409 handling
  - ✅ YAML → DB import — a `smoked config import <file>` CLI and a Config-tab **Import YAML**
    button that additively merge a YAML/JSON file's target branches into the DB fragment;
    an entry identical to the stored one is skipped (idempotent), a same-name entry with
    different settings is a conflict (nothing imported), and globals stay in YAML
- ✅ **SmokePing → TimescaleDB importer** — read an existing SmokePing install's `Targets` config +
  RRD history and load it here, so a running SmokePing can migrate (RRD → TSDB is the core shift).
  *(Medium; 2 slices, both shipped.)*
  - ✅ Slice A: config import — `smoked import smokeping <dir>` parses a legacy `Targets`/`Probes`/
    `Database` config (FPing/FPing6/DNS/TCPPing → the modern probe map; `speedtest`/unmapped probes
    skipped and reported) into a reviewable YAML target tree (default: stdout/`--out`) or merges it
    straight into the DB config fragment (`--apply`, reusing `config.AppendImport`)
  - ✅ Slice B: RRD history backfill — `--report` (dry-run) reconciles config targets against the
    RRD data dir (matched/config-only/orphan counts, data dir resolved as sibling `../data` or a
    `data` subdir); `--history` extracts each matched target's full RRD history via `rrdtool` and
    backfills median/loss into `samples` + the hourly/daily aggregates, idempotently. Requires the
    continuous aggregates already enabled (`smoked -downsample`) — refuses to import (no rows
    written) otherwise, so old history is never left unmaterialized ahead of raw retention
- ✅ **Native ICMP probe** — a new `Ping` probe kind (`internal/probe/pingprobe`) speaks ICMP
  echo itself via `golang.org/x/net/icmp`: datagram socket first (unprivileged, needs the
  `net.ipv4.ping_group_range` sysctl), raw-socket fallback (`CAP_NET_RAW`). Ships alongside
  `FPing` rather than replacing it — the external binary and `setcap` dance stay for users who
  keep using `FPing`, but new deployments can drop both by using `Ping` instead. *(Small–medium.)*
- 🚧 **NTP probe** (planned; 2026-08-12) — a new probe kind measuring NTP server responsiveness: an
  SNTP request/response round-trip (and optionally clock offset / stratum), self-registering like the
  other native probes, no external binary. *(Small.)*
- ✅ **On-disk agent store-and-forward** — the agent persists its buffer to disk (opt-in `spool_dir`) so a vantage restart — including a hard crash — loses at most ~1s of unpushed rounds instead of the whole buffer; append-only CRC-framed segments mirror the in-memory buffer, crash-safe replay on startup, `flock` against shared dirs, degrade-to-memory on I/O error.
- ✅ **Cut the release** — **v1.0.0** (2026-08-11): everything built to date ships under a single
  1.0 line (the earlier "single-node v1.0 / federation v2.0" split was collapsed, since nothing had
  been released yet). Version constants bumped off `0.1.0`, CHANGELOG `[Unreleased]` promoted to
  `[1.0.0]`, and an annotated `v1.0.0` git tag + GitLab release carry the notes.

## Known limitations / follow-ups (from the 2026-08-04 code review)
- ✅ **Per-target polling intervals** — while serving, each target fires on its own `step`
  via `scheduler.Planner` (phase-aligned per target); config/per-node `step` is honored. The
  `-rounds` foreground demo still runs synchronized rounds at `-step`.
- ✅ **Dashboard read amplification at scale** — done: bulk `LatestAll` +
  `AvailabilityAll` collapse the aggregate endpoints to one query each, the 10d/400d
  rollup is windowed server-side, and the raw 3h grid now uses one bulk, incremental
  `GET /api/series/all?window=&since=` (a single store query per tick, transferring only
  rounds newer than the client watermark) instead of a request + query per target.
- ✅ **SLA over long windows on pgstore** — done: a time-bounded store aggregate
  (`Availabler`/`AvailabilityAll`, and windowed `HistorySince`) computes availability over
  the full requested window, unbounded by the `History` cap, with per-target coverage.

## Hardening & UI backlog (from the 2026-08-14 code review + session)

Merged this session: #34 vantage docker-compose generator · #35 band-panel "collecting…" message ·
#36 author-defined menu order (weight) · #37 Config-tab tree + drag-reorder · #38 hardening
(M6 spool-alloc bound, M2 now-blocking Caddy scan, M8 build-time version injection).

### Next up
- 🚧 **M3 — SMTP notifier hardening** — IN PROGRESS: committed + `-race`-tested on branch
  `worktree-m3-smtp-hardening` (per-transaction deadline, bounded retry/backoff, and an explicit
  error when AUTH is configured but the relay doesn't advertise it — was silently unauthenticated).
  Next: confirm the in-flight review → open PR → merge.

### CODE_REVIEW.md items still open (triaged 2026-08-14; none critical/high)
- **M5** — `/api/series/all` still `row_number()`-ranks the whole window per target though the rows
  are capped, and the read API is unauthenticated. Fix: lateral per-target `LIMIT N`. *(Needs a
  TimescaleDB to validate with `EXPLAIN (ANALYZE, BUFFERS)`.)*
- **M1** — CI builds the collector image twice, so the scanned/SBOM'd digest isn't the one pushed +
  attested. Fix: build-once → scan → promote the same digest. *(Do before cutting a release; L.)*
- **M4** — remote-ingest validates a config snapshot then writes with no reload lock; a reload
  between validate and insert can store a round under a redefined target. *(Opportunistic — can't
  cause a false alert, only pollutes stored history/rollups.)*
- **M7** — the aggregate-migration check validates only `samples_hourly`; a stale `samples_daily`
  is never rebuilt. *(Narrow trigger; needs a DB.)*
- **L1/L2/L3/L4** (small): probe per-ping budget ignores the `(pings-1)×interval` sleeps + unbounded
  `interval_ms`; native-Ping reply-window tail capped at 1s even when `timeout_ms`>1s; false
  "truncated" warning at an exact-cap dataset; `.trivyignore` expiry token isn't valid for the flat
  file (move to `.trivyignore.yaml` `expired_at`).

### Follow-ups from merged work
- **Config-tab tree (#37):** keyboard-accessible reorder + real tree ARIA (tabindex/aria-expanded),
  cross-folder drag, add-into-folder.
- **Caddy image (#38):** the runtime `apk --upgrade c-ares/curl` + the three `xcaddy --with
  golang.org/x/net|x/text|grpc` floors are interim patches over a stale pinned base — drop them once
  `caddy:2.11-alpine` (or a newer Caddy release) ships the fixes natively, and bump the digest instead.
- **README:** refresh the screenshots in dark mode.

## Notes / decisions
- Stack: Go collector · TimescaleDB (raw samples; bands via SQL quantiles) · REST+JSON API ·
  vanilla-JS/canvas frontend · HTTPS/JSON + per-vantage API keys for federation. (See blueprint §1.)
- Storage deliberately keeps the raw N samples per round — the smoke bands need the
  distribution, not just the median.
- Probe binaries that are unavailable are skipped with a warning, never fatal.
