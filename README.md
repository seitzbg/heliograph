# smokeping-modern (working codename)

A modern, non-Perl reimplementation of [SmokePing](https://github.com/oetiker/SmokePing) — MVP scaffold.
Goal: reproduce SmokePing's features and its signature **smoke graphs**, with a fast, parallel, plugin-based poller.

> Full analysis of the original and the rewrite plan: `~/.claude/plans/smokeping-codemap/`.
> This repo is the MVP **collector core** (steps 1–2 of the roadmap in `07-modernization-blueprint.md`).

## What works today (verified)

- **Smoke-graph renderer** (`web/smoke-poc.html`) — a self-contained canvas re-implementation of SmokePing's signature chart: nested percentile bands darkening toward the median + the 8-bucket loss-colored median line, light/dark theme-aware, across four latency scenarios. This de-risks the "keep the look & feel" requirement. Open it in a browser, or view the published version at the artifact URL noted in the session.
- **Plugin probes** — a `Probe` interface + registry; probes self-register via `init()`. Six shipped (the lean v1 set), all live-tested against real hosts:
  - `FPing` — wraps `fping(8)` for ICMP echo RTT (CLI-wrapper style).
  - `TCPConnect` — native TCP-connect timing, no external binary.
  - `DNS` — native resolver query timing via `miekg/dns` (no external `dig`).
  - `HTTP` — native HTTP(S) time-to-first-byte (minus DNS) via `net/http` + `httptrace` (no external `curl`).
  - `SSH` — native SSH-banner-read timing, no external binary.
  - `IRTT` — wraps `irtt(1)` for UDP round-trip / jitter (needs an irtt server).
- **Fast, parallel scheduler** — a bounded goroutine worker pool runs all probes concurrently, each under its own timeout. One slow/hung target cannot block the others (proven by `scheduler_test.go`).
- **SmokePing sample math** — median, loss (= missing samples), and the "centered" array that makes smoke bands render symmetrically (`internal/sample`, unit-tested against SmokePing's `rrdupdate_string` semantics).
- **JSON API** — `/api/probes`, `/api/probes/schema` (each probe's config as JSON Schema, generated from the same source as runtime validation), `/api/targets`, `/api/series?target=NAME`. `series` returns the raw per-round sample array (the input a client-side smoke chart needs).
- **Live web dashboard** (`web/index.html`) — fetches `/api/series` and renders each target with the shared canvas smoke renderer (`web/smoke.js`), auto-refreshing; light/dark theme-aware. Served same-origin by the collector.
- **YAML config with inheritance** (`internal/config`, `config.example.yaml`) — a target tree where `probe`/`pings`/`step`/`params`/`alerts` set on a node apply to everything beneath it until overridden (SmokePing's key ergonomic). Each leaf is validated against its probe's `Schema()` — the modern stand-in for SmokePing's per-probe dynamic grammar. `-config file.yaml` replaces the built-in demo targets.
- **Alert engine** (`internal/alert`) — per-target windows of recent loss/latency samples, with hysteresis matchers (`CheckLoss`, `CheckLatency`: raise after X bad rounds, clear after X good) and a pattern DSL — right-anchored shape matches (`>50%,>50%`, `>200,>200`) with `*N*` skips, a bare `*` wildcard, and `==U`/`!=U` for a lost round's unknown rtt. Firing/resolved state with edge-triggering; per-alert `priority` inhibits noisier alerts on the same target, and a per-target `alertee` adds extra recipients. Notifiers = log + webhook (JSON POST). Verified firing live on real loss and latency. Alerts are defined in config and attached to targets by name.
- **Pluggable store** — a `store.Store` interface with two implementations:
  - `MemStore` — in-memory (default; for dev/tests).
  - `pgstore` — **TimescaleDB**: one row per round in a `samples` hypertable keeping the raw per-round sample array (loss gaps stored as SQL `NULL`), so smoke bands come from the real distribution. Verified end-to-end against a live TimescaleDB.

## Run it

```sh
go test ./...                              # unit tests (sample math + scheduler isolation/parallelism)
go run ./cmd/smoked -rounds 2 -pings 10    # measure a demo target set, print a table
go run ./cmd/smoked -serve -addr :8087     # serve the live dashboard + JSON API (polls forever)
#   -> open http://localhost:8087/  for the live smoke graphs
curl 'localhost:8087/api/series?target=Cloudflare%20DNS%20(ICMP)'

# Persist to TimescaleDB instead of memory:
go run ./cmd/smoked -serve -dsn 'postgres://user:pass@host:5432/smoke?sslmode=disable'

# Load targets from a YAML tree (with inheritance) instead of the demo set:
go run ./cmd/smoked -serve -config config.example.yaml
```

### TimescaleDB (dev + integration test)

```sh
# ephemeral instance for local dev/testing
podman run -d --name sp-ts -e POSTGRES_USER=smoke -e POSTGRES_PASSWORD=smoke \
  -e POSTGRES_DB=smoke -p 127.0.0.1:5433:5432 docker.io/timescale/timescaledb:latest-pg16

# the pgstore integration test is skipped unless a DSN is provided:
SMOKE_TEST_DSN='postgres://smoke:smoke@127.0.0.1:5433/smoke?sslmode=disable' \
  go test ./internal/store/pgstore
```

The schema (one `samples` hypertable) is created automatically on first connect.

Example output:

```
registered probe plugins: FPing, TCPConnect
── round 1  (6 targets in 4.00s, wall-clock) ─────────────────
TARGET                                 PROBE         MEDIAN    LOSS  NOTE
Cloudflare DNS (ICMP)                  FPing         5.05ms    0/10
Google DNS (ICMP)                      FPing         8.74ms    0/10
localhost (ICMP)                       FPing         0.04ms    0/10
Cloudflare 443 (TCP)                   TCPConnect    5.07ms    0/10
Google 443 (TCP)                       TCPConnect    8.93ms    0/10
Unreachable :9 (TCP, expect loss)      TCPConnect        --   10/10  err: context deadline exceeded
```

## Layout

```
internal/
  probe/         Probe interface + registry (the plugin contract)
    tcpconnect/  native TCP-connect probe
    fping/       fping(8) wrapper probe (own process group; killed as a group on timeout)
  sample/        median / loss / centered-smoke-array math (+ tests)
  scheduler/     parallel worker pool, per-target timeout, phase-aligned NextDelay (+ tests)
  config/        YAML target-tree loader + inheritance resolver (+ tests)
  alert/         matchers (hysteresis + pattern DSL) + engine + notifiers (+ tests)
  model/         Monitor (a configured leaf target)
  store/         store.Store interface + MemStore (in-memory)
    pgstore/     TimescaleDB implementation (samples hypertable, raw sample arrays)
  api/           JSON HTTP API + static file serving (SmokePing CGI replacement)
    probe/dns/       native DNS probe (miekg/dns)
    probe/httpprobe/ native HTTP TTFB probe (net/http + httptrace)
    probe/sshprobe/  native SSH banner-timing probe
    probe/irttprobe/ irtt(1) UDP round-trip/jitter wrapper (+ parser test)
cmd/smoked/      the collector binary
web/
  smoke.js       shared canvas smoke renderer (bands + loss-coloured median)
  index.html     live dashboard (fetches /api/series)
  smoke-poc.html self-contained synthetic demo (published as an Artifact)
```

## Design decisions (see codemap `07`)

- **Go** for the poller: goroutines make massively-parallel probing with per-target timeouts cheap; replaces SmokePing's process-per-probe + fork-pool with one worker pool.
- **Probes as plugins**: native interface for the core set; a gRPC `go-plugin` protocol (planned) will let third parties add probes in any language without recompiling.
- **Storage keeps raw samples**: the smoke graph needs the per-round distribution, so store the N samples (not just the median) and compute bands at query time.

## Not built yet (roadmap)

DNS/HTTP/SSH/IRTT native probes · TimescaleDB storage + downsampling tiers · YAML/DB config target-tree with inheritance · the React/canvas smoke-graph frontend · the pattern/matcher alert engine · gRPC+mTLS federation agents. See `07-modernization-blueprint.md` §8.
```
