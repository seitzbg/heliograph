# Heliograph

[![CI](https://github.com/seitzbg/heliograph/actions/workflows/ci.yml/badge.svg)](https://github.com/seitzbg/heliograph/actions/workflows/ci.yml)
&nbsp;[![Release](https://img.shields.io/github/v/release/seitzbg/heliograph?label=release)](https://github.com/seitzbg/heliograph/releases)
&nbsp;[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**Read the network in smoke and light.** Heliograph is a from-scratch reimplementation of
[SmokePing](https://oss.oetiker.ch/smokeping/) in Go, backed by TimescaleDB. It keeps the parts of
SmokePing worth keeping — the **smoke graphs** and the pluggable probe model — drops the Perl and the
RRD files, and adds multi-vantage **federation** and a database-backed config you can edit from the
browser.

> A *heliograph* is a signaling instrument that flashes messages across long distances — and it has
> *graph* right in the name. Fitting for a tool whose remote **vantages** signal what they see and
> whose smoke bands you read at a glance. (The daemon is still `smoked`.)

See the [changelog](CHANGELOG.md) for release notes and the [roadmap](ROADMAP.md) for what's next.

## Screenshots

The signature **smoke graph**, rebuilt on HTML canvas — nested percentile bands (the "smoke" = jitter)
darkening toward the median, with an 8-bucket loss-colored median line, theme-aware. Four scenarios,
from a steady fiber link to a flaky, lossy one:

![Heliograph smoke graphs — congested / steady / jittery / flaky links](docs/img/smoke-graph-dark.png)

The **live dashboard** — the Overview ranks the worst targets by loss/latency/jitter and reports
per-target availability:

![Heliograph dashboard — worst targets and availability](docs/img/dashboard-overview-dark.png)

**Graphs** is the per-target grid (filter the tree, click a target for all four time ranges, then
click a graph to zoom):

![Heliograph per-target graph grid](docs/img/dashboard-graphs-dark.png)

The **Config** tab edits DB-backed targets as a drag-to-reorder tree (merged live with the YAML
config), and the **Vantages** panel manages federation agent keys — both behind the admin password:

![Heliograph Config tab — DB-backed target tree with drag-to-reorder](docs/img/config-tree-dark.png)

![Heliograph Vantages panel — federation agent keys](docs/img/vantages-dark.png)

## Features

- **Smoke graphs from the real distribution.** One row per round in a TimescaleDB `samples`
  hypertable keeps the whole sample array (loss stored as SQL `NULL`), so the percentile bands show
  what actually happened rather than a smoothed average. Hourly and daily continuous aggregates
  cover long time ranges; an in-memory store backs dev and tests.
- **Fast parallel polling.** A bounded worker pool probes every target concurrently, each under its
  own timeout and schedule, so one slow or hung target never holds up the rest.
- **Eight built-in probes**, from native ICMP to NTP clock offset. See [Probes](#probes).
- **Alerting.** Hysteresis loss/latency matchers and a right-anchored pattern DSL, edge-triggered
  with priority inhibition and per-target recipients. Notifiers: log, webhook, Slack, Discord, email.
- **Inheritable config.** A YAML target tree where `probe`/`step`/`params`/`alerts` cascade to
  children and each leaf is schema-validated; load it from a file or a `conf.d/` directory, or edit
  it live from a DB-backed Config tab.
- **Multi-vantage federation.** Remote `smoke-agent` collectors report to the hub and overlay a
  median line per vantage on every graph. See [Beyond parity](#beyond-parity).
- **JSON API and migration.** Everything the dashboard reads is plain JSON (`/api/series`,
  `/api/charts`, `/api/sla`, `/api/probes/schema`, …), and `smoked import smokeping` brings over an
  existing install's targets and RRD history.

## Probes

A probe measures one target, one ping at a time, and hands back the round-trip times it saw. The
collector does the rest — median, loss (a missing sample *is* the loss), and the percentile bands.
Each probe is a self-registering plugin under `internal/probe/`, so a new one is a single small file
and needs no changes to the core.

Eight ship with Heliograph:

| Probe | What it times | Notes |
|-------|---------------|-------|
| `Ping` | ICMP echo round-trip | Pure Go (`golang.org/x/net/icmp`). Uses an unprivileged datagram socket where the OS allows it, and falls back to a raw socket otherwise — no `fping`, no `setcap`. |
| `FPing` | ICMP echo round-trip | Wraps the `fping(8)` binary, the way classic SmokePing does. Kept for parity; needs `fping` on `PATH` and `NET_RAW`/setcap. Reach for `Ping` first. |
| `TCPConnect` | TCP handshake completion | Pure Go. Set `port` per target. Runs unprivileged and works against anything listening on a port. |
| `DNS` | Query resolution time | Pure Go (`miekg/dns`). `lookup` a name of `recordtype` (A, AAAA, MX, …) against the target resolver, over udp or tcp. |
| `HTTP` | Time to first byte | Pure Go (`net/http` + `httptrace`). Stops at the first response byte, so it times the server rather than the size of the body. |
| `SSH` | Time to the server banner | Pure Go. Measures how long the SSH server takes to send its identification string — a cheap liveness and latency check that never authenticates. |
| `IRTT` | UDP round-trip + one-way jitter | Wraps the `irtt(1)` client and needs an `irtt` server on the far end. The most precise latency/jitter source when you control both ends. |
| `NTP` | SNTP round-trip, or clock offset | Pure Go SNTP over UDP/123. `measure: rtt` (default) graphs the query round-trip; `measure: offset` graphs the server's clock offset as a signed, zero-baselined smoke graph, with stratum shown alongside. It paces its own requests, backs off on a Kiss-o'-Death reply, and checks that each response echoes the request it answers before trusting an offset. |

Settings that apply to a probe kind go under `probes:` in the config; a single target overrides them
in its own `params:`. Both are schema-checked at load time, and the live schema is served at
`/api/probes/schema`.

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
docker pull ghcr.io/seitzbg/heliograph:latest
# Bind loopback only: the dashboard + read API are UNAUTHENTICATED, so never publish them on a
# reachable interface without a reverse proxy (TLS + auth) in front — see Federation deployment below.
docker run --rm -p 127.0.0.1:8087:8087 ghcr.io/seitzbg/heliograph:latest \
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
    image: ghcr.io/seitzbg/heliograph:latest
    depends_on:
      timescaledb:
        condition: service_healthy
    cap_add: [NET_RAW]                                  # fping ICMP (non-root, setcap'd binary)
    sysctls:
      net.ipv4.ping_group_range: "0 10001"             # native Ping via an unprivileged socket (the collector's GID; a wider range like "0 2147483647" fails to start under rootless Podman)
    environment:
      # The image's default command already runs `-serve -addr :8087 -webdir /web`; these env vars
      # drive the rest, keeping the DB password out of the command list. Each mirrors a -flag.
      SMOKED_DSN: postgres://smoke:${SMOKE_DB_PASSWORD:-smoke}@timescaledb:5432/smoke?sslmode=disable
      SMOKED_DOWNSAMPLE: "1"                            # hourly/daily aggregates for the UI
      SMOKED_ABSOLUTE_TIME: "1"                         # graph x-axes show absolute clock time (default); "0" = relative -3h/now
      # Runs a built-in demo target set. To measure your own, mount a YAML tree and point at it:
      #   volumes: ["./config.yaml:/etc/heliograph/config.yaml:ro"]
      #   SMOKED_CONFIG: /etc/heliograph/config.yaml
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
`SMOKED_ADMIN_PASSWORD` in `.env` to enable the Config/Vantages admin GUI, and generate an
independent persistent cookie-signing secret with `openssl rand -hex 32` as
`SMOKED_ADMIN_SESSION_KEY`. If that key is omitted, login remains secure but sessions end whenever
the collector restarts. A login stays valid for 12 hours by default; set `SMOKED_ADMIN_SESSION_TTL`
to any Go duration (e.g. `24h`, `168h`, `30m`, minimum `1m`) to lengthen or shorten it. Over the
proxy's TLS, the admin session cookie works remotely. smoked's own
`127.0.0.1:8087` stays available **on
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
Cloudflare ICMP (FPing)                FPing         5.05ms    0/10
Google ICMP (FPing)                    FPing         8.74ms    0/10
localhost ICMP (FPing)                 FPing         0.04ms    0/10
Cloudflare TCP :443                    TCPConnect    5.07ms    0/10
Google TCP :443                        TCPConnect    8.93ms    0/10
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
    ntpprobe/    native SNTP probe — graphs RTT or signed clock offset (measure), stratum stat (+ tests)
    allprobes/   blank-imports every probe so both binaries register the same set (+ parity test)
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

## Beyond parity

Two capabilities go past SmokePing, and both ship in the 1.0 line:

- **Multi-vantage federation** — remote `smoke-agent` collectors report to the hub over HTTPS with
  per-vantage API keys, and each detail graph overlays a median line per vantage. Transport is
  HTTPS/JSON behind a required reverse proxy — a bundled Caddy (automatic Let's Encrypt / DNS-01) or
  your own. See the **[federation operator guide](docs/federation.md)** and
  [Federation deployment](#federation-deployment-reverse-proxy).
- **Database-sourced configuration** — targets, probes, and alerts can live in the store alongside
  YAML (additive, `conf.d`-style), edited from the in-browser **Config** tab, with
  `smoked config import` / `smoked import smokeping` to migrate an existing SmokePing install.

Planned next: further notifier integrations (e.g. PagerDuty). See the [roadmap](ROADMAP.md).

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
