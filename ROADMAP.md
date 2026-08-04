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

## Phase 4 — Federation (multi-vantage) — deferred (followup if users want it)
Not planned for now; pick up only if multi-vantage measurement is requested.
- ⬜ Agent binary that pulls its assignment and pushes results
- ⬜ gRPC transport + mutual TLS (no eval of server-sent config)
- ⬜ Per-vantage series (the `~slave` equivalent) + overlay graphs
- ⬜ Store-and-forward buffering on the agent

## Phase 5 — Parity & polish ⬜
- ⬜ Richer alert DSL: `*N*` skip, `U`/`S` tokens, priority suppression, per-target `alertee`
  - ⚠ When building this, avoid two bugs the Perl original shipped (verified in a correctness
    review): the `*N*` skip-wildcard alignment was wrong, so documented patterns like
    `>0%,*12*,>0%` silently never matched; and unknown loss (`U`) was recorded as 0%, so an
    outage read as clean history and `==U` could never fire. Drive the matcher with
    table-driven tests over both, and keep unknown rounds distinct from 0% (today an errored
    round is 100% loss and an unavailable probe is a gap — never silently 0%).
- ⬜ Emit each probe's config as JSON Schema (docs + validation from one source)
- ⬜ Overview (small multiples) + multi-range detail (3h/30h/10d/400d) + unison scaling in the SPA
- ⬜ Top-N "charts" (worst by loss/median/stddev) and alternate menu hierarchies
- ⬜ Drag-to-zoom time range (refetch, not image swap); in-app dark-mode toggle persistence
- ✅ Prometheus metrics export (`/metrics`: median/loss/up per target) for Grafana-native users
- ⬜ Availability/SLA reporting (the tSmoke equivalent)

## Notes / decisions
- Stack: Go collector · TimescaleDB (raw samples; bands via SQL quantiles) · REST+JSON API ·
  React/canvas frontend · gRPC+mTLS for federation. (See blueprint §1.)
- Storage deliberately keeps the raw N samples per round — the smoke bands need the
  distribution, not just the median.
- Probe binaries that are unavailable are skipped with a warning, never fatal.
