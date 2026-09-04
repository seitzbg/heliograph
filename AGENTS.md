# AGENTS.md

Setup, build, test, and run instructions for coding agents (and humans) working on
**Heliograph** — a Go + TimescaleDB reimplementation of SmokePing. This file is the quick
operational contract; [`README.md`](README.md) has the deep dives, [`docs/federation.md`](docs/federation.md)
covers multi-vantage deployment, [`docs/alerting.md`](docs/alerting.md) covers alert configuration and
notifiers, and [`CHANGELOG.md`](CHANGELOG.md) / [`ROADMAP.md`](ROADMAP.md) track what shipped and what's next.

> Names: the Go module is `github.com/seitzbg/heliograph`, the hub/collector binary is `smoked`, and the
> remote-vantage collector is `smoke-agent`. The product name is Heliograph.

## Prerequisites

| Tool | Version | Needed for |
|------|---------|------------|
| Go | see `go.mod` (`go 1.26.6`) | everything; CI uses `go-version-file: go.mod` |
| Node.js | 22 | the web unit tests (`web/*.test.mjs`) |
| rrdtool | any | the SmokePing importer tests (`internal/importer/smokeping`) and `smoked import ... --history`; those tests **skip** without it |
| Docker or Podman | any | building the container image; running a throwaway TimescaleDB for integration tests |

No Makefile — everything is plain `go`/`node` commands. No frontend build step: `web/` ships
hand-written ES modules served as static files (`smoked -webdir web`).

## Build

```sh
go build ./...                 # build everything
go build ./cmd/smoked          # -> ./smoked   (gitignored)
go build ./cmd/smoke-agent     # -> ./smoke-agent
```

## Test — the full local gate (mirror of `.github/workflows/ci.yml`)

Run these before claiming a change is done; they reproduce every CI job locally.

```sh
# 1. formatting must be clean (CI scopes gofmt to cmd + internal)
test -z "$(gofmt -l cmd internal)"

# 2. vet + build + race-detector test suite
go vet ./...
go build ./...
go test -race ./...

# 3. go.mod / go.sum must stay tidy
go mod tidy && git diff --exit-code go.mod go.sum

# 4. web unit tests (pure helpers + headless canvas render; no browser)
node web/dashboard.test.mjs
node web/smoke.render.test.mjs

# 5. dependency vulnerability scan (pinned, not @latest, for reproducibility)
go install golang.org/x/vuln/cmd/govulncheck@v1.6.0
govulncheck ./...

# 6. browser layout regression test (the CI `layout-test` job — the only check that
#    catches the narrow-viewport grid/legend/tab-bar overflow at the seam a user sees).
#    Serves a freshly built collector and drives real Chromium.
npm ci
npx playwright install --with-deps chromium   # or: PLAYWRIGHT_CHANNEL=chrome to reuse system Chrome
go build -o smoked ./cmd/smoked
SMOKED_BIN=./smoked node web/layout.test.mjs
```

### Integration tests (real TimescaleDB)

The `pgstore` / `vantage` / `api` / `configstore` / `cmd/smoked --history` tests are **skipped
unless `SMOKE_TEST_DSN` is set**. Spin up a throwaway instance and point them at it:

```sh
# ephemeral TimescaleDB (podman or docker)
podman run -d --name sp-ts -e POSTGRES_USER=smoke -e POSTGRES_PASSWORD=smoke \
  -e POSTGRES_DB=smoke -p 127.0.0.1:5433:5432 docker.io/timescale/timescaledb:latest-pg16

export SMOKE_TEST_DSN='postgres://smoke:smoke@127.0.0.1:5433/smoke?sslmode=disable'

# -p 1 is REQUIRED: these packages share single-row config + the samples hypertable, so their
# test binaries must run serialized, not in parallel.
go test -p 1 -count=1 \
  ./internal/store/pgstore/ ./internal/vantage/ ./internal/api/ ./internal/configstore/ ./cmd/smoked/
```

The schema (one `samples` hypertable) is created automatically on first connect — no migration step.

## Run it locally (and verify)

```sh
# measure the built-in demo target set and print a table (no server)
go run ./cmd/smoked -rounds 2 -pings 10

# serve the live dashboard + JSON API (polls forever)
go run ./cmd/smoked -serve -addr :8087
#   -> open http://localhost:8087/  and probe the API:
curl 'localhost:8087/api/targets'
curl 'localhost:8087/api/series?target=Cloudflare%20DNS%20(ICMP)'

# use your own YAML target tree, or a directory (default.yaml + conf.d/*.yaml):
go run ./cmd/smoked -serve -config config.example.yaml
go run ./cmd/smoked -serve -config examples/config-dir/

# persist to TimescaleDB instead of memory:
go run ./cmd/smoked -serve -dsn 'postgres://smoke:smoke@127.0.0.1:5433/smoke?sslmode=disable'
```

Most flags also read a `SMOKED_*` env var (`SMOKED_DSN`, `SMOKED_CONFIG`, `SMOKED_DOWNSAMPLE`,
`SMOKED_SLACK_WEBHOOK`, `SMOKED_SMTP_*`, …); run `go run ./cmd/smoked -h` for the full list.

**Subcommands** (dispatched from `os.Args[1]`, before flag parsing):

```sh
smoked vantage add|ls|revoke ...   # per-vantage mTLS registry + self-bootstrapped CA (needs -dsn)
smoked config import ...           # import a YAML tree into the DB config store
smoked import smokeping ...        # import an existing SmokePing install (config; --history for RRD backfill)
smoked mcp -url ... ...            # stdio MCP server: diagnosis + config staging/apply (README#mcp-server)
```

**Verifying a change:** a green `go test` is necessary but not sufficient for behavior changes —
bring up `-serve` and hit the real `/api/*` endpoint (or drive the UI) to confirm the live symptom,
matching the project's history of integration bugs that unit tests missed.

## Container image

```sh
docker build -t heliograph:dev .                          # collector image (Dockerfile)
docker build -f Caddy.Dockerfile -t heliograph-caddy .    # bundled reverse proxy (xcaddy + DNS plugins)
```

Prebuilt collector images are published to `ghcr.io/seitzbg/heliograph:latest` by CI. A minimal
Compose stack (collector + TimescaleDB) and the `federation` profile (bundled Caddy fronting the
dashboard with TLS + Basic Auth; agents authenticate to smoked's own mTLS listener) live in
[`docker-compose.yml`](docker-compose.yml) / [`README.md`](README.md#docker-compose).
The read API and dashboard are **unauthenticated** — bind loopback and front with a reverse proxy for
anything beyond localhost.

## Project layout

See the **Layout** section of [`README.md`](README.md#layout) for the annotated tree. Orientation:

- `cmd/smoked/` — hub/collector binary (serve, `vantage`, `config`/`import`, `mcp` subcommands).
- `cmd/smoke-agent/` — remote-vantage collector binary.
- `internal/probe/` — the `Probe` plugin contract; each probe self-registers via `init()` in its
  subpackage (`fping`, `pingprobe`, `tcpconnect`, `dns`, `httpprobe`, `sshprobe`, `irttprobe`,
  `ntpprobe`). Both binaries pull in the full set through `internal/probe/allprobes` (blank-imported
  by `smoked` and `smoke-agent`), so the hub and vantage agent can't drift to different probe sets.
- `internal/mcp/` — the `smoked mcp` stdio MCP server: diagnosis tools + a local stage/review/apply
  config-write flow, all client calls onto the same HTTP API the dashboard uses.
- `internal/sample/` — median / loss / centered-smoke-array math (the smoke-graph input).
- `internal/scheduler/` — parallel worker pool with per-target timeouts.
- `internal/store/` + `internal/store/pgstore/` — `store.Store` interface, in-memory + TimescaleDB.
- `internal/config/`, `internal/configstore/` — YAML target-tree loader; DB-backed config.
- `internal/alert/` — hysteresis matchers + pattern DSL + notifiers (log, webhook, Slack, Discord, email).
- `internal/federation/`, `internal/vantage/`, `internal/agent/`, `internal/agentwire/` — multi-vantage plumbing.
- `internal/importer/smokeping/` — SmokePing config + RRD-history import.
- `web/` — `index.html` + hand-written ES modules (`dashboard.js`, `smoke.js`); tests are `*.test.mjs`.

## Conventions

- **Formatting:** `gofmt` clean (CI checks `cmd` + `internal`); keep `go.mod`/`go.sum` tidy.
- **Concurrency:** the scheduler, dispatcher, and delivery pool are async — run tests with `-race`,
  and remember DB-integration packages must run with `-p 1` (see above).
- **Probes are plugins:** add a probe by creating a subpackage that self-registers in `init()` and
  implements the `Probe` interface + a `Schema()`; don't wire it into a central switch.
- **Storage keeps raw samples:** the smoke graph needs the per-round distribution — store the N
  samples, not just the median, and compute bands at query time.
- **Docs travel with the change:** update `CHANGELOG.md` and `ROADMAP.md` in the same change that
  alters behavior, not a follow-up.
