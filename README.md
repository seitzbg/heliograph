# Heliograph

[![CI](https://github.com/seitzbg/heliograph/actions/workflows/ci.yml/badge.svg)](https://github.com/seitzbg/heliograph/actions/workflows/ci.yml)
&nbsp;[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**Read the network in smoke and light** — a modern, non-Perl reimplementation of
[SmokePing](https://oss.oetiker.ch/smokeping/) in Go on TimescaleDB. It reproduces SmokePing's
features and its signature **smoke graphs** with a fast, parallel, plugin-based poller, and goes
beyond parity with multi-vantage **federation** and database-sourced configuration.

> A *heliograph* is a signaling instrument that flashes messages across long distances — and it has
> *graph* right in the name. Fitting for a tool whose remote **vantages** signal what they see and
> whose smoke bands you read at a glance. (The daemon is still `smoked`.)

Stable since **v1.0.0** — see [CHANGELOG.md](CHANGELOG.md) and the [roadmap](ROADMAP.md).

## Screenshots

The signature **smoke graph**, rebuilt on HTML canvas — nested percentile bands (the "smoke" = jitter)
darkening toward the median, with an 8-bucket loss-colored median line, theme-aware. Four scenarios,
from a steady fiber link to a flaky, lossy one:

![Heliograph smoke graphs — congested / steady / jittery / flaky links](docs/img/smoke-graph.png)

The **live dashboard** — the Overview ranks the worst targets by loss/latency/jitter and reports
per-target availability:

![Heliograph dashboard — worst targets and availability](docs/img/dashboard-overview.png)

**Graphs** is the per-target grid (filter the tree, click a target for all four time ranges, then
click a graph to zoom):

![Heliograph per-target graph grid](docs/img/dashboard-graphs.png)

## What works today (verified)

- **Smoke-graph renderer** (`web/smoke-poc.html`) — a self-contained canvas re-implementation of SmokePing's signature chart: nested percentile bands darkening toward the median + the 8-bucket loss-colored median line, light/dark theme-aware, across four latency scenarios. This de-risks the "keep the look & feel" requirement. Open it in a browser to explore.
- **Plugin probes** — a `Probe` interface + registry; probes self-register via `init()`. Seven shipped, all live-tested against real hosts:
  - `FPing` — wraps `fping(8)` for ICMP echo RTT (CLI-wrapper style).
  - `Ping` — native ICMP echo via `golang.org/x/net/icmp`, no `fping` binary/`setcap`: an
    unprivileged datagram socket first (needs the `net.ipv4.ping_group_range` sysctl), falling
    back to a raw socket (needs `CAP_NET_RAW`) — `mode: auto|unprivileged|privileged` can pin
    one path. Params: `packetsize` (default `56`), `interval_ms` (default `50`). Coexists with
    `FPing`.
  - `TCPConnect` — native TCP-connect timing, no external binary.
  - `DNS` — native resolver query timing via `miekg/dns` (no external `dig`).
  - `HTTP` — native HTTP(S) time-to-first-byte (minus DNS) via `net/http` + `httptrace` (no external `curl`).
  - `SSH` — native SSH-banner-read timing, no external binary.
  - `IRTT` — wraps `irtt(1)` for UDP round-trip / jitter (needs an irtt server).
- **Fast, parallel scheduler** — a bounded goroutine worker pool runs all probes concurrently, each under its own timeout. One slow/hung target cannot block the others (proven by `scheduler_test.go`).
- **SmokePing sample math** — median, loss (= missing samples), and the "centered" array that makes smoke bands render symmetrically (`internal/sample`, unit-tested against SmokePing's `rrdupdate_string` semantics).
- **JSON API** — `/api/probes`, `/api/probes/schema` (each probe's config as JSON Schema, generated from the same source as runtime validation), `/api/targets`, `/api/series?target=NAME`, `/api/charts?by=loss|median|stddev` (worst-N targets), `/api/sla?window=24h` (per-target availability). `series` returns the raw per-round sample array (the input a client-side smoke chart needs).
- **Live web dashboard** (`web/index.html`) — fetches `/api/series` and renders each target with the shared canvas smoke renderer (`web/smoke.js`), auto-refreshing; light/dark theme-aware. Served same-origin by the collector.
- **YAML config with inheritance** (`internal/config`, `config.example.yaml`) — a target tree where `probe`/`pings`/`step`/`params`/`alerts` set on a node apply to everything beneath it until overridden (SmokePing's key ergonomic). Each leaf is validated against its probe's `Schema()` — the modern stand-in for SmokePing's per-probe dynamic grammar. `-config file.yaml` replaces the built-in demo targets. `-config` also accepts a **directory** (`examples/config-dir/`): `default.yaml` holds `database`/`probes`/`alerts` + tree-wide defaults, and `conf.d/*.yaml` drop-in fragments each add top-level target branches (SmokePing `@include`-style concatenation, loaded in sorted filename order; a fragment may contain only `targets.children`).
- **Alert engine** (`internal/alert`) — per-target windows of recent loss/latency samples, with hysteresis matchers (`CheckLoss`, `CheckLatency`: raise after X bad rounds, clear after X good) and a pattern DSL — right-anchored shape matches (`>50%,>50%`, `>200,>200`) with `*N*` skips, a bare `*` wildcard, and `==U`/`!=U` for a lost round's unknown rtt. Firing/resolved state with edge-triggering; per-alert `priority` inhibits noisier alerts on the same target, and a per-target `alertee` adds extra recipients. Notifiers = log, generic webhook (JSON POST), **Slack**, and **Discord** (each configured by URL via
a flag or `SMOKED_*` env var, and referenced from an alert as `to: [slack]` / `to: [discord]`).
Verified firing live on real loss and latency. Alerts are defined in config and attached to targets by name.
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

### Container image

Prebuilt images are published to the [GitHub Container Registry](https://github.com/seitzbg/heliograph/pkgs/container/heliograph)
by CI on every push to `main` (and tagged releases):

```sh
docker pull ghcr.io/seitzbg/heliograph:main
# Bind loopback only: the dashboard + read API are UNAUTHENTICATED, so never publish them on a
# reachable interface without a reverse proxy (TLS + auth) in front — see Federation deployment below.
docker run --rm -p 127.0.0.1:8087:8087 ghcr.io/seitzbg/heliograph:main \
  -serve -addr :8087 -webdir /web
#   -> open http://localhost:8087/
```

### Docker Compose

A minimal two-service stack (collector + TimescaleDB) using the **prebuilt GHCR image** — no clone
or build required. Save as `compose.yaml` and run `docker compose up -d`, then open
<http://localhost:8087/>:

```yaml
services:
  timescaledb:
    image: timescale/timescaledb:2.29.1-pg16
    environment:
      POSTGRES_USER: smoke
      POSTGRES_PASSWORD: ${SMOKE_DB_PASSWORD:-smoke}   # override in a .env file (see note)
      POSTGRES_DB: smoke
    volumes:
      - tsdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U smoke -d smoke"]
      interval: 5s
      timeout: 3s
      retries: 10

  smoked:
    image: ghcr.io/seitzbg/heliograph:main
    depends_on:
      timescaledb:
        condition: service_healthy
    cap_add: [NET_RAW]                                  # fping ICMP (non-root, setcap'd binary)
    sysctls:
      net.ipv4.ping_group_range: "0 2147483647"        # native Ping via an unprivileged socket
    environment:
      # The image's default command already runs `-serve -addr :8087 -webdir /web`; these env vars
      # drive the rest, keeping the DB password out of the command list. Each mirrors a -flag.
      SMOKED_DSN: postgres://smoke:${SMOKE_DB_PASSWORD:-smoke}@timescaledb:5432/smoke?sslmode=disable
      SMOKED_DOWNSAMPLE: "1"                            # hourly/daily aggregates for the UI
      # Runs a built-in demo target set. To measure your own, mount a YAML tree and point at it:
      #   volumes: ["./config.yaml:/etc/smokeping/config.yaml:ro"]
      #   SMOKED_CONFIG: /etc/smokeping/config.yaml
    ports:
      - "127.0.0.1:8087:8087"                           # loopback only — the read API is unauthenticated

volumes:
  tsdata: {}
```

Set `SMOKE_DB_PASSWORD` (e.g. in a `.env` file beside the compose file) to change the database
password in **both** the DB and the collector's DSN at once. Note: the password is written into the
`tsdata` volume on first init, so changing it later does **not** re-initialize an existing volume —
recreate the volume (or `ALTER ROLE` inside the DB) to rotate it.

This binds `127.0.0.1` because the dashboard + read API are unauthenticated; to reach it beyond
localhost, front it with TLS + auth. The repo's own [`docker-compose.yml`](docker-compose.yml)
adds a `federation` profile with a bundled Caddy reverse proxy (auto Let's Encrypt, per-vantage
API keys, Basic Auth) — see [Federation deployment](#federation-deployment-reverse-proxy).

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

### Federation deployment (reverse proxy)

Remote **vantages** (the `smoke-agent` collector) reach the hub over HTTPS with a per-vantage
API key. `smoked` never terminates TLS itself — a reverse proxy does. The proxy serves two
surfaces with two auth models: the **agent API** (`/agent/v1/*`) authenticated by the per-vantage
API key, and the **dashboard + read API + admin panel** behind **HTTP Basic Auth** (smoked's read
API has no auth of its own).

> For the end-to-end walkthrough — declaring `vantages:`, minting a key, running an agent, and
> reading the overlay — see the **[federation operator guide](docs/federation.md)**.

**Bundled Caddy** (automatic Let's Encrypt) — opt-in via the `federation` compose profile:

```sh
cp .env.example .env            # set DOMAIN + ACME_EMAIL (DNS for DOMAIN must point here)
# dashboard Basic Auth password (bcrypt); see .env.example for the $-escaping note:
export DASH_PASSWORD_HASH="$(docker run --rm caddy:2.11-alpine caddy hash-password --plaintext 'choose-a-password')"
docker compose --profile federation up --build
```

The default `docker compose up` starts no proxy (federation stays dark). With the profile, Caddy
obtains and auto-renews the cert and reverse-proxies `https://$DOMAIN/` to smoked: `/agent/v1/*`
by API key, everything else behind Basic Auth (`DASH_USER` / `DASH_PASSWORD_HASH`). Set
`SMOKED_ADMIN_PASSWORD` in `.env` to also enable the Vantages admin GUI panel (over the proxy's
TLS, the admin session cookie works remotely). smoked's own `127.0.0.1:8087` stays available **on
the hub itself only** — it binds loopback, so it is not reachable from other LAN hosts; reach it
remotely through the proxy or an SSH tunnel, never by rebinding it to `0.0.0.0` (the read API is
unauthenticated).

**Certificate challenge.** By default Caddy uses **HTTP-01** (needs inbound port 80 during
issuance/renewal). To use **DNS-01** instead — no inbound port needed, works behind NAT, supports
wildcards — set `CADDY_ACME_DNS` to your provider's line. The bundled image (`Caddy.Dockerfile`,
built via `xcaddy`) includes the **cloudflare, route53, digitalocean, duckdns, namecheap, gandi**
plugins. E.g. Cloudflare (a token with Zone:DNS:Edit):

```sh
# in .env
CADDY_ACME_DNS=acme_dns cloudflare {env.CF_API_TOKEN}
CF_API_TOKEN=your-cloudflare-token
```

See `.env.example` for every provider's exact line and credentials (route53/namecheap use a
multi-field block). To add a provider not listed, add a `--with github.com/caddy-dns/<name>` line
to `Caddy.Dockerfile` and rebuild.

**External proxy** — to front smoked with your own proxy instead, skip the profile and mirror the
same split: forward `/agent/v1/*` (API-key auth) and put Basic Auth on the rest. Caddy:

```
smoke.example.com {
    handle /agent/v1/* {
        reverse_proxy 127.0.0.1:8087
    }
    handle {
        basic_auth {
            admin <bcrypt-hash-from-caddy-hash-password>
        }
        reverse_proxy 127.0.0.1:8087
    }
}
```

nginx:

```nginx
server {
    listen 443 ssl;
    server_name smoke.example.com;
    # ssl_certificate / ssl_certificate_key ...
    location /agent/v1/ {                      # agents: API-key auth, no Basic Auth
        proxy_pass http://127.0.0.1:8087;
        proxy_set_header Authorization $http_authorization;
    }
    location / {                               # dashboard + read API: Basic Auth
        auth_basic "heliograph";
        auth_basic_user_file /etc/nginx/.htpasswd;
        proxy_pass http://127.0.0.1:8087;
    }
}
```

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
    pingprobe/   native ICMP echo probe (golang.org/x/net/icmp; datagram-first, raw fallback)
    dns/         native DNS probe (miekg/dns)
    httpprobe/   native HTTP TTFB probe (net/http + httptrace)
    sshprobe/    native SSH banner-timing probe
    irttprobe/   irtt(1) UDP round-trip/jitter wrapper (+ parser test)
  sample/        median / loss / centered-smoke-array math (+ tests)
  scheduler/     parallel worker pool, per-target timeout, phase-aligned NextDelay (+ tests)
  config/        YAML target-tree loader + inheritance resolver (+ tests)
  alert/         matchers (hysteresis + pattern DSL) + engine + notifiers (+ tests)
  model/         Monitor (a configured leaf target)
  store/         store.Store interface + MemStore (in-memory)
    pgstore/     TimescaleDB implementation (samples hypertable, raw sample arrays)
  configstore/   versioned DB config fragment (config-in-DB, optimistic concurrency)
  api/           JSON HTTP API + agent + admin endpoints + static file serving
  federation/    per-vantage assignment builder + measurement fingerprint
  vantage/        per-vantage API-key store (salted hash, constant-time verify)
  agentwire/     shared hub<->agent wire types
  agent/         smoke-agent buffer + on-disk spool + hub client
  importer/
    smokeping/   SmokePing Targets/Probes/RRD import (config + history backfill)
cmd/smoked/      the hub/collector binary (serve, vantage, config/smokeping import)
cmd/smoke-agent/ the remote-vantage collector binary
web/
  dashboard.js   the single-page dashboard (overview, graphs, config, vantages)
  smoke.js       shared canvas smoke renderer (bands + loss-coloured median)
  index.html     live dashboard (fetches /api/series)
  smoke-poc.html self-contained synthetic smoke-graph demo
```

## Design decisions

- **Go** for the poller: goroutines make massively-parallel probing with per-target timeouts cheap; replaces SmokePing's process-per-probe + fork-pool with one worker pool.
- **Probes as plugins**: native interface for the core set; a gRPC `go-plugin` protocol (planned) will let third parties add probes in any language without recompiling.
- **Storage keeps raw samples**: the smoke graph needs the per-round distribution, so store the N samples (not just the median) and compute bands at query time.

## Beyond parity and roadmap

**Federation (multi-vantage) is complete** — the hub, the `smoke-agent` remote collector, the per-vantage overlay UI, the Vantages admin GUI panel, and the bundled reverse proxy: a per-vantage storage dimension (the hub probes as `local`), `vantages:` config with a per-vantage assignment builder, a TimescaleDB-backed API-key store with a `smoked vantage` CLI + a password-gated admin API and login-gated GUI panel, the agent-facing endpoints (`GET /agent/v1/assignment`, `POST /agent/v1/results`) with idempotent ingest and per-vantage alert evaluation, per-vantage overlay graphs in the detail views, and a bundled Caddy compose profile that terminates TLS (automatic Let's Encrypt / DNS-01) and serves the agent API by key + the dashboard behind Basic Auth. Transport is HTTPS/JSON with per-vantage API keys behind a required reverse proxy (the bundled Caddy, or your own) — superseding the earlier gRPC+mTLS plan. See the **[federation operator guide](docs/federation.md)** and [Federation deployment](#federation-deployment-reverse-proxy).

**Database-sourced configuration also ships in 1.0** — targets/probes/alerts can live in the store
alongside YAML (additive, `conf.d`-style), edited from an in-browser **Config** tab, with a
`smoked config import` / `smoked import smokeping` path to migrate an existing SmokePing install.

Still planned: **email (SMTP)** and further notifier integrations (e.g. PagerDuty). See the
[roadmap](ROADMAP.md).

## Acknowledgements

- **Inspired by [SmokePing](https://oss.oetiker.ch/smokeping/)** by Tobi Oetiker — the classic that
  made latency "smoke" graphs and the inherited target tree the way a generation of us learned to
  watch a network. This project is a ground-up Go reimagining of those ideas, not a fork; all credit
  for the original concept and its iconic visualization is Tobi's.
- **Built with AI.** Heliograph was designed, implemented, and tested collaboratively with
  [Claude Code](https://www.anthropic.com/claude-code), Anthropic's agentic coding tool — from the
  smoke-graph renderer and probe plugins through the TimescaleDB store, federation, and CI — and
  code-reviewed with [OpenAI Codex](https://openai.com/codex/) and
  [CodeRabbit](https://www.coderabbit.ai/).

## License

[MIT](LICENSE) © 2026 Bryan Seitz
