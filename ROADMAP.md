# Roadmap

The plan for **smokeping-modern** — a modern, non-Perl reimplementation of SmokePing.
Full design rationale and the original-system code map live at `~/.claude/plans/smokeping-codemap/`
(`07-modernization-blueprint.md` is the north star). Legend: ✅ done · 🚧 in progress · ⬜ todo.

## Guiding requirements
1. **Keep the smoke-graph look & feel** — band distribution + loss-colored median. ✅ (canvas renderer, verified light/dark)
2. **Fast, highly parallel poller** — thousands of targets, per-target timeout, one slow target never blocks others. ✅ (goroutine worker pool)
3. **Probes as plugins** — add a probe without touching the core. ✅ (Probe interface + registry)

## Phase 0 — Analysis ✅
- ✅ Full code map of SmokePing 2.9.0 + modernization blueprint (external: `~/.claude/plans/smokeping-codemap/`).

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
- ✅ Alert engine (hysteresis matchers + pattern DSL, edge-trigger, log/webhook notifiers)

## Phase 3 — Production-readiness ✅
- ✅ CI pipeline (GitLab CI: vet + build + test) — green on the `ubuntu-26.04-amd64` runner
- ✅ Live dashboard verified rendering from the TimescaleDB store (raw-sample path)
- ✅ Continuous-aggregate downsampling tier (hourly) + refresh + 30-day retention policy
- ✅ API serves downsampled long ranges — `/api/rollup` (hourly) done; SPA range selector switches raw ↔ hourly (min/avg/max band; degrades gracefully when no TimescaleDB)
- ✅ Dockerfile + compose (smoked + TimescaleDB) for one-command bring-up
- ✅ Graceful shutdown (SIGINT/SIGTERM: cancel in-flight probes + clean HTTP shutdown)
- ✅ Config reload without restart (SIGHUP; atomic runtime swap, keeps running config on error)
- ✅ Structured logging (slog; `-log-format` text/json) + operational metrics (round duration/size/errors + per-probe timings on `/metrics`)
- ✅ Preserve alert firing state across config reload (reload inherits firing state + sample windows, so alerts don't re-fire or lose hysteresis history)

## Phase 4 — Federation (multi-vantage) 🚧 in progress
Hub-side groundwork is in place; the agent and the agent-facing endpoints are next.

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
- ⬜ Agent-facing endpoints: `GET /agent/v1/assignment` (304-aware) + `POST /agent/v1/results`
- ⬜ `smoke-agent` binary: pull assignment → probe → push results + store-and-forward buffering
- ⬜ Per-vantage series (the `~slave` equivalent) overlay graphs + a Vantages admin GUI panel
- ⬜ Bundled Caddy (Let's Encrypt) + documented external-reverse-proxy deployment

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
- ✅ Drag-to-zoom time range — done: drag a range on the detail graph and it refetches
  that `[from,to]` at the resolution best for the span (raw/hourly/daily), not an image
  swap. In-app dark-mode toggle persistence done ✅ (localStorage, restored before first paint)
- ✅ Prometheus metrics export (`/metrics`: median/loss/up per target) for Grafana-native users
- ✅ Availability/SLA reporting (the tSmoke equivalent) — `GET /api/sla?window=…` per-target
  availability % over a window + dashboard "Availability" panel

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

## Notes / decisions
- Stack: Go collector · TimescaleDB (raw samples; bands via SQL quantiles) · REST+JSON API ·
  vanilla-JS/canvas frontend · HTTPS/JSON + per-vantage API keys for federation. (See blueprint §1.)
- Storage deliberately keeps the raw N samples per round — the smoke bands need the
  distribution, not just the median.
- Probe binaries that are unavailable are skipped with a warning, never fatal.
