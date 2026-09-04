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

Every graph can **overlay a median line per federation vantage**, so you read one target from
several networks at once — here `github.com` from the hub plus two remote uplinks (Comcast and
FiOS), each vantage a distinct line over its own smoke band:

![Heliograph multi-vantage federation overlay — one target across three uplinks](docs/img/federation-overlay-dark.png)

The **Config** tab edits DB-backed targets as a drag-to-reorder tree (merged live with the YAML
config); each target keeps a stable, server-managed identity, so moving or renaming it in the tree
preserves its existing graph rather than starting a new one. The **Vantages** panel manages
federation vantages — both behind the admin password:

![Heliograph Config tab — DB-backed target tree with drag-to-reorder](docs/img/config-tree-dark.png)

![Heliograph Vantages panel — federation vantages](docs/img/vantages-dark.png)

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
  See the **[alerting operator guide](docs/alerting.md)** for defining alerts and configuring each
  notifier channel.
- **Inheritable config.** A YAML target tree where `probe`/`step`/`params`/`alerts` cascade to
  children and each leaf is schema-validated; load it from a file or a `conf.d/` directory, or edit
  it live from a DB-backed Config tab.
- **Multi-vantage federation.** Remote `smoke-agent` collectors report to the hub and overlay a
  median line per vantage on every graph. See [Beyond parity](#beyond-parity).
- **JSON API and migration.** Everything the dashboard reads is plain JSON (`/api/series`,
  `/api/charts`, `/api/sla`, `/api/probes/schema`, …), and `smoked import smokeping` brings over an
  existing install's targets; add `--history` (with a DSN and `rrdtool`) to also backfill its RRD
  history.
- **MCP server for AI-assisted diagnosis and config.** `smoked mcp` exposes Heliograph over the
  [Model Context Protocol](https://modelcontextprotocol.io/) — 13 tools covering triage/status/SLA/
  series/vantages, config reads, and a stage → review → apply flow for config edits — so an
  MCP-aware assistant can investigate and, with an admin password, safely propose changes. See
  [MCP server](#mcp-server).

## Probes

A probe measures one target, one ping at a time, and hands back the per-ping samples it took —
round-trip times for most probes, or a signed clock offset when NTP runs in `measure: offset` mode.
The collector does the rest — median, loss (a missing sample *is* the loss), and the percentile
bands. Each probe is a self-registering plugin under `internal/probe/`, so adding one is mostly a
single small file; the one shared edit is its blank import (and matching parity-test entry) in
`internal/probe/allprobes`, which both the hub and the vantage agent load so either can run it.

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
adds a `federation` profile with a bundled Caddy reverse proxy (auto Let's Encrypt, Basic Auth) for
the dashboard, plus smoked's own published mutual-TLS listener for remote agents (no API key) —
see [Federation deployment](#federation-deployment-reverse-proxy).

Prefer a ready-made, copyable stack? [`examples/`](examples/) has two: **[`standalone/`](examples/standalone/)**
(collector + TimescaleDB, the common single-host case) and **[`federation/`](examples/federation/)**
(adds the Caddy reverse proxy for remote vantages) — each with its own `.env.example`.

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

Remote **vantages** (the `smoke-agent` collector) reach the hub over two independent surfaces
that share no auth mechanism:

- The **dashboard + read API + admin panel** (`:8087`) — `smoked` never terminates TLS itself, so
  a reverse proxy sits in front, gating it with **HTTP Basic Auth** over TLS.
- The **agent API** — `smoked`'s own dedicated mutual-TLS listener, enabled with
  `-agent-addr :8443 -agent-hostname <domain>` (requires `-dsn`; smoked fatals at startup if
  `-agent-addr` is set without it). smoked self-bootstraps a CA, issues its own server certificate
  (SAN from `-agent-hostname`), and requires every connecting agent to present a CA-signed
  **client certificate** — that certificate is the vantage's identity, so there is no API key to
  mint, store, or leak. This listener is published directly to the internet; it is **not** proxied
  by Caddy or any other reverse proxy.

> For the end-to-end walkthrough — declaring `vantages:`, onboarding a vantage, running an agent,
> and reading the overlay — see the **[federation operator guide](docs/federation.md)**.

**Onboarding a vantage is one click.** In the dashboard's admin panel, **Vantages → Add
vantage**: type a name and it downloads a ready-to-run `<name>-vantage.tar.gz` — `agent.yaml`
(hub URL, vantage name, and the client certificate/key/CA embedded as PEM) plus a
`docker-compose.yml` and `README.txt`. Copy it to the vantage host, `tar xzf`, `docker compose up
-d`. The CLI equivalent is `smoked vantage add <name> -out <name>-vantage.tar.gz` (or omit `-out`
to print the rendered `agent.yaml` to stdout, or pass `-json` for the raw PEMs). The panel also
**list**s vantages and lets you **regenerate** one — issuing and downloading a fresh certificate
bundle for an existing name, which does *not* invalidate the certificate already deployed there
(there's no revocation list; both stay valid until the vantage is revoked). Revocation is by
removal: `smoked vantage revoke <name>` deletes it from the registry, and any certificate bearing
that name is rejected on its very next request even though the certificate itself remains
cryptographically valid. To fully retire a credential, revoke the vantage and re-add it.

**Bundled Caddy** (automatic Let's Encrypt, dashboard only) — opt-in via the `federation` compose
profile:

```sh
cp .env.example .env            # set DOMAIN + ACME_EMAIL (DNS for DOMAIN must point here)
# dashboard Basic Auth password (bcrypt); see .env.example for the $-escaping note:
export DASH_PASSWORD_HASH="$(docker run --rm caddy:2.11-alpine caddy hash-password --plaintext 'choose-a-password')"
docker compose --profile federation up --build
```

The default `docker compose up` starts no proxy and no agent listener (federation stays dark).
With the profile, Caddy obtains and auto-renews the cert and reverse-proxies `https://$DOMAIN/` to
smoked's dashboard, behind Basic Auth (`DASH_USER` / `DASH_PASSWORD_HASH`); smoked separately
publishes its own mTLS agent listener on `:8443`, untouched by Caddy. Set `SMOKED_ADMIN_PASSWORD`
in `.env` to enable the Config/Vantages admin GUI, and generate an independent persistent
cookie-signing secret with `openssl rand -hex 32` as `SMOKED_ADMIN_SESSION_KEY`. If that key is
omitted, login remains secure but sessions end whenever the collector restarts. A login stays
valid for 12 hours by default; set `SMOKED_ADMIN_SESSION_TTL` to any Go duration (e.g. `24h`,
`168h`, `30m`, minimum `1m`) to lengthen or shorten it. Over the proxy's TLS, the admin session
cookie works remotely. smoked's own `127.0.0.1:8087` stays available **on the hub itself only** —
it binds loopback, so it is not reachable from other LAN hosts; reach it remotely through the
proxy or an SSH tunnel, never by rebinding it to `0.0.0.0` (the read API is unauthenticated).

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
to `Caddy.Dockerfile` and rebuild. (This only affects the dashboard's certificate — the agent
listener's certificate is self-issued by smoked's own CA and never touches Let's Encrypt.)

**External proxy** — to front the dashboard with your own proxy instead of the bundled Caddy,
gate `:8087` with Basic Auth **and forward `X-Forwarded-Proto`**; there's nothing agent-related to
add, since agents bypass the dashboard proxy entirely and connect straight to smoked's `:8443`
listener. The `X-Forwarded-Proto` header is how smoked — which never terminates TLS itself — learns
the request reached you over HTTPS: the vantage-onboarding bundle embeds a vantage's private key, so
**minting one is refused over plaintext** (`403`). Caddy's `reverse_proxy` sets the header
automatically; nginx needs an explicit `proxy_set_header`. (Loopback on the hub is exempt — it never
crosses the wire — and the `smoked vantage add` CLI is the local/headless escape hatch.) Caddy:

```
smoke.example.com {
    basic_auth {
        admin <bcrypt-hash-from-caddy-hash-password>
    }
    reverse_proxy 127.0.0.1:8087   # sets X-Forwarded-Proto automatically
}
```

nginx:

```nginx
server {
    listen 443 ssl;
    server_name smoke.example.com;
    # ssl_certificate / ssl_certificate_key ...
    location / {                               # dashboard + read API: Basic Auth
        auth_basic "heliograph";
        auth_basic_user_file /etc/nginx/.htpasswd;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;   # required: vantage-bundle mint refuses plaintext
        proxy_pass http://127.0.0.1:8087;
    }
}
```

Publish `-agent-addr`'s port (`:8443` by default) straight through your firewall/load balancer to
smoked — do not route it through the proxy above.

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

## MCP server

`smoked mcp` runs a stdio [Model Context Protocol](https://modelcontextprotocol.io/) server that
wraps Heliograph's own HTTP API, so an MCP-aware assistant (Claude Code, Claude Desktop, etc.) can
diagnose your network and — with an admin password — stage and apply config changes, without ever
touching the database or the raw API directly. It's a thin client: every tool call maps onto the
same `/api/*` endpoints the dashboard uses; there's no separate auth model or separate state.

### Env / flags

| Flag | Env var | Required | Meaning |
|------|---------|----------|---------|
| `-url` | `HELIOGRAPH_URL` | yes | Hub base URL, e.g. `https://heliograph.example` |
| `-basic-user` | `HELIOGRAPH_BASIC_USER` | no | Reverse-proxy Basic Auth username, if the hub sits behind one |
| `-basic-pass` | `HELIOGRAPH_BASIC_PASS` | no | Reverse-proxy Basic Auth password |
| `-admin-pass` | `HELIOGRAPH_ADMIN_PASS` | no | Admin password — required only for `config_apply` (the tool that actually writes to the hub) and for unredacted admin reads. `config_stage_*`, `config_review`, and `config_discard` work locally without it. Omit it to run read-only. |

Point `-url` at the hub's **root** (e.g. `https://heliograph.example`), not a sub-path mount — the
client's admin-route detection matches `/api/admin` at the start of the request path, and a base
URL with a sub-path prefix (`https://heliograph.example/some/path`) shifts every `/api/admin/...`
request past that prefix, breaking it.

Use an **`https://`** URL whenever Basic Auth or an admin password is configured: the client
refuses a plaintext `http://` base URL when credentials are set, so they are never transmitted in
cleartext (loopback hosts are exempt for local development).

### Tools

13 tools, grouped by what they touch:

**Diagnosis (read-only)**

| Tool | What it does |
|------|--------------|
| `heliograph_status` | Current per-target snapshot: probe, median latency, loss %, recent loss %, NTP offset/stratum, and which vantages measure it. |
| `heliograph_sla` | Per-target availability over a window (worst-first): availability %, rounds up/measured, coverage, average loss. |
| `heliograph_series` | Per-round latency/loss history for one target; compact by default (`t`/median/loss/pings), `detail=true` adds per-ping `rtts_ms`. |
| `heliograph_triage` | Classifies every target healthy/degraded/down/no-data across vantages, splits GLOBAL problems (bad from every vantage that returned a reading → a target issue) from VANTAGE-SPECIFIC ones (bad from some vantages but healthy or no-data from others → a path/ISP issue), and flags stale collectors. Start here for an open-ended "what's wrong?" question. |
| `heliograph_vantages` | Lists measurement vantages: name, created, last-seen, target count, and whether the hub's federation agent listener is up. |

**Config read**

| Tool | What it does |
|------|--------------|
| `heliograph_config_get` | Reads the monitoring config — `source=db` (the editable DB fragment, with a version for optimistic-concurrency writes) or `source=effective` (the read-only file+DB merged config the collector actually runs); `format` yaml (default) or json. |

**Config write (stage → review → apply)**

| Tool | What it does |
|------|--------------|
| `config_stage_add_target` | Stage adding a target (group path, host, probe, params, step, pings, NTP `measure`, vantages). Mints a stable id. |
| `config_stage_edit_target` | Stage an edit to an existing target, including moving/renaming it (identity-preserving). |
| `config_stage_remove_target` | Stage removing a target by id or path; empty groups are pruned. |
| `config_stage_replace` | Stage a wholesale YAML/JSON replacement of the DB config fragment — the escape hatch for shapes the typed tools above don't cover (alert routing, probe defaults, …). |
| `config_review` | Show the currently staged diff (added/removed/changed targets), the full working config, and whether the live config has drifted since staging. |
| `config_apply` | **Commit the staged changes to the live hub** (`PUT /api/admin/config`). |
| `config_discard` | Discard all staged changes and reset the staging buffer. |

`config_stage_replace` mints a fresh id for any host node without one, so a hand-authored replacement
doc that omits an existing target's `id` orphans that target's history on apply. To preserve
identity, base the replacement on `heliograph_config_get source=db format=json` — its output
includes each target's `id`.

### Safety model: staging is local, only `config_apply` writes

Every `config_stage_*` tool, `config_review`, and `config_discard` operates on an **in-process,
per-server-instance staging buffer** — nothing reaches the hub. `config_stage_*` validates the
staged document locally using the daemon's own config parser and validator (`internal/config`), so
most structural and probe-param mistakes surface immediately, before anything is sent anywhere.

`config_apply` is the **only** tool that writes to the live hub: it `PUT`s the staged document to
`/api/admin/config` with the config version it was staged against (optimistic concurrency). The
hub's own validation is authoritative — the local pass runs against a defaults-only base and can
miss something the real merged config (with the hub's file-defined targets) would catch, and the
server's error is surfaced verbatim if it rejects the apply. A version conflict (someone else
changed the config since you staged) comes back as an error; `config_review`'s `drifted: true`
flags that before you even try.

`GET /api/admin/config[.yaml]` is an open, unauthenticated read (only the `PUT` is admin-gated), so
`config_stage_*`, `config_review`, and `config_discard` all work locally without `-admin-pass` /
`HELIOGRAPH_ADMIN_PASS` — you can build up and inspect a full staged change with no admin password
configured. `-admin-pass` / `HELIOGRAPH_ADMIN_PASS` is required for exactly one thing: `config_apply`,
which returns an error without it.

### `claude mcp add`

```sh
claude mcp add heliograph -- \
  smoked mcp -url https://heliograph.example \
  -basic-user "$HELIO_USER" -basic-pass "$HELIO_PASS" \
  -admin-pass "$HELIO_ADMIN"
```

Equivalent JSON client config (e.g. `.mcp.json`, or Claude Desktop's `claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "heliograph": {
      "command": "smoked",
      "args": ["mcp", "-url", "https://heliograph.example"],
      "env": {
        "HELIOGRAPH_BASIC_USER": "your-proxy-user",
        "HELIOGRAPH_BASIC_PASS": "your-proxy-pass",
        "HELIOGRAPH_ADMIN_PASS": "your-admin-password"
      }
    }
  }
}
```

Drop the Basic Auth / admin-pass entries for a hub with no reverse-proxy auth or no admin GUI
enabled. Omitting `-admin-pass`/`HELIOGRAPH_ADMIN_PASS` leaves diagnosis, config-read, and
`config_stage_*`/`config_review`/`config_discard` all working (they only ever read the open
`GET /api/admin/config` endpoint); only `config_apply` — the tool that writes to the live hub —
errors without it.

A minimal, copy-pasteable version of this config lives in [`examples/mcp/`](examples/mcp/).

### Manual smoke

No automated test in this repo drives `smoked mcp` against a live network — `internal/mcp`'s test
suite runs against an in-process `httptest` server, not a real hub. To confirm the real thing works
end to end, run it against a real hub and call a couple of tools by hand — for example with the
[MCP Inspector](https://modelcontextprotocol.io/legacy/tools/inspector):

```sh
npx @modelcontextprotocol/inspector smoked mcp -url https://heliograph.example -admin-pass "$HELIO_ADMIN"
```

Call `heliograph_status` and `heliograph_triage` from the Inspector's tool panel and confirm the
response matches your hub's actual targets and their current health (cross-check against the
dashboard). If you've already `claude mcp add`'d it, the same check works by asking the assistant
to run those two tools.

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
  mcp/           MCP server (stdio) — diagnosis tools + a stage/review/apply config-write flow
                 over the same HTTP API, for AI-assisted operation (+ tests)
  federation/    per-vantage assignment builder + measurement fingerprint
  vantage/        per-vantage registry + self-bootstrapped CA (mTLS client-cert auth; revoke by
                  removing the name)
  agentwire/     shared hub<->agent wire types
  agent/         smoke-agent buffer + on-disk spool + hub client
  importer/
    smokeping/   SmokePing Targets/Probes/RRD import (config + history backfill)
cmd/smoked/      the hub/collector binary (serve, vantage, config/smokeping import, mcp)
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

- **Multi-vantage federation** — remote `smoke-agent` collectors report to the hub over their own
  mutually-authenticated TLS connection (a CA-signed client certificate is the vantage's identity,
  no API key involved), and each detail graph overlays a median line per vantage. The dashboard
  separately sits behind a required reverse proxy — a bundled Caddy (automatic Let's Encrypt /
  DNS-01) or your own. See the **[federation operator guide](docs/federation.md)** and
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
