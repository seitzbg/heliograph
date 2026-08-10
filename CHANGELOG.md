# Changelog

All notable changes to **smokeping-modern** are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/) and the project aims to follow
[Semantic Versioning](https://semver.org/). Pre-1.0, minor versions may carry
breaking changes.

## [Unreleased]

### Added
- **Native ICMP `Ping` probe.** A new `Ping` probe kind sends and matches ICMP Echo itself via
  `golang.org/x/net/icmp` — no `fping` binary, no `setcap`. Per round it opens a socket
  **datagram-first** (an unprivileged `udp4`/`udp6` ICMP socket, gated by the kernel's
  `net.ipv4.ping_group_range`) and falls back to a raw `ip4:icmp`/`ip6:ipv6-icmp` socket
  (needs `CAP_NET_RAW`) when the datagram attempt fails; `mode: auto|unprivileged|privileged`
  can pin one path instead of trying both, and a failure that exhausts every attempt names both
  remedies in the error. Family (IPv4/IPv6) follows the target's resolved address. Params:
  `packetsize` (ICMP payload bytes, default `56`) and `interval_ms` (gap between successive
  sends within one round, default `50`). `Ping` coexists with `FPing` — the fping wrapper probe
  is unchanged and still available. See `docker-compose.yml` for the `ping_group_range` sysctl
  that enables the unprivileged path.
- **SmokePing RRD history import.** `smoked import smokeping <dir> --history` (needs `--dsn`/
  `SMOKED_DSN` and `rrdtool` on `PATH` or `--rrdtool`) reconciles the legacy config's targets
  against the RRD data directory (resolved as `--data`, else the sibling `<dir>/../data` or
  `<dir>/data` — linuxserver/SmokePing puts `config/` and `data/` as siblings) and, for every
  target that has a matching `.rrd`, extracts its full consolidated history (finest-resolution RRA
  first) and backfills median + loss into `samples`, then refreshes the hourly/daily continuous
  aggregates over the imported range. Import is idempotent (`ON CONFLICT DO NOTHING`, so a re-run
  adds 0 rows) and config stays the source of truth: a target with no `.rrd` is skipped and
  reported (`config-only`), an `.rrd` with no matching target is reported as an `orphan` and never
  imported. History from before smokeping-modern's own raw-sample retention window still renders
  on the dashboard, just from the aggregate (its smoke band collapses to the median line — there's
  no per-round distribution in an RRD's consolidated data to draw a band from). A dry-run
  `--report` mode prints the same target/matched/config-only/orphan counts without touching the
  DB, for previewing config-vs-RRD drift before running `--history`. This completes the SmokePing
  importer (slice B on top of slice A's config-only import below).
- **SmokePing config import.** `smoked import smokeping <dir>` reads a legacy SmokePing install's
  `Targets`/`Probes`/`Database` config and turns the target tree into a modern config fragment:
  by default it prints tidy YAML (or writes it with `--out FILE`) for review; `--apply` (with
  `--dsn`/`SMOKED_DSN`) merges it straight into the DB config fragment via the same
  `config.AppendImport` path as `config import`, so a re-run is idempotent (`unchanged`, not a
  duplicate). SmokePing probes map to their modern equivalent (FPing/FPing6 → FPing, DNS → DNS,
  TCPPing → TCPConnect); `speedtest`/`speedtestcli` and any unrecognized probe are skipped and
  reported, along with any per-probe param with no modern equivalent.
- **YAML → DB config import.** A `smoked config import <file>` subcommand and a Config-tab
  **Import YAML** button both merge a YAML (or JSON) config's `targets:` branches into the
  database fragment, additively. The merge is **atomic and idempotent**: an imported entry that
  is byte-for-byte identical to the stored one is skipped (reported as `unchanged`), a same-name
  entry with *different* settings is a conflict that imports nothing (`400` in the UI, non-zero
  exit for the CLI), and globals (`database`/`probes`/`alerts`) are ignored. A successful import
  hot-reloads with no SIGHUP. Completes **Config in a database** (all four slices shipped).
- **Target-management UI.** A login-gated **Config** tab (over the same admin session as Vantages)
  lists the database-managed targets and adds / edits / removes them via a modal — a probe dropdown,
  host, key/value params, and optional vantages/alerts. It read-modify-writes the whole fragment
  through the config CRUD API: a save validates + hot-reloads (a target appears/updates with no
  SIGHUP), a rejected edit shows the validation error, and a concurrent change reloads (`409`).
  YAML-defined targets are listed read-only (managed in files); reserved names (`__proto__` etc.)
  are refused. Single tab addition; the rest of the dashboard is unchanged.
- **Config CRUD API.** Admin-gated `GET`/`PUT /api/admin/config` (behind `SMOKED_ADMIN_PASSWORD`)
  reads and replaces the DB config fragment. A `PUT` carries the version it read; the hub
  **validates** the candidate config (builds a runtime from the YAML config + the proposed doc),
  **persists** it with optimistic concurrency, then **hot-reloads** — all atomically: an invalid
  doc is rejected (`400`, with the validation error) and never persisted, a stale version is
  rejected (`409`), and a good edit takes effect with no SIGHUP. The SIGHUP reload and the API now
  share one runtime-swap path. Foundation for the in-browser target-management UI.
- **Config in a database (groundwork).** With `-dsn` set, `smoked` now loads a DB-stored config
  fragment as an additional **live source**, concatenated with the YAML config (`default.yaml` +
  `conf.d/*.yaml`) on every boot and SIGHUP reload — the existing duplicate-branch guard applies
  across sources, and the fragment carries target branches only (globals stay in YAML). Stored as a
  single versioned `config_fragment` row (new `internal/configstore`) with optimistic-concurrency
  `Get`/`Set`. **Dark until configured:** no `-dsn`, or an absent/empty row, is byte-for-byte
  unchanged. Groundwork for the config CRUD API + in-browser target management still to come.
- **Federation operator guide** (`docs/federation.md`) — the end-to-end walkthrough for
  provisioning a vantage: declaring `vantages:` on targets, minting a per-vantage key
  (`smoked vantage add` or the GUI), running `smoke-agent` at the remote location, and reading the
  overlay, plus the security model, an upgrade note (the one-time continuous-aggregate rebuild
  drops daily rollups older than the 30-day raw retention), and troubleshooting. Linked from the
  README. Marks **Phase 4 — Federation complete.**
- **Bundled Caddy reverse proxy (federation deployment).** An opt-in `federation` Docker Compose
  profile adds a Caddy service that terminates TLS with an automatically issued and auto-renewed
  Let's Encrypt certificate (`DOMAIN` + `ACME_EMAIL` from `.env`) and reverse-proxies smoked with
  two auth models: the agent API (`/agent/v1/*`) by its per-vantage API key, and the dashboard +
  read API + admin panel behind **HTTP Basic Auth** (`DASH_USER` / `DASH_PASSWORD_HASH`) — since
  smoked's read API has no auth of its own. `smoked` never does TLS itself; both the API key and
  the Basic Auth ride inside Caddy's TLS, and over that real TLS the admin session cookie works
  remotely. Certificates use the HTTP-01 challenge by default, or **DNS-01** (`CADDY_ACME_DNS`) —
  useful behind NAT / for wildcards — with provider plugins for **cloudflare, route53,
  digitalocean, duckdns, namecheap, gandi** compiled into the Caddy image (`Caddy.Dockerfile` via
  `xcaddy`). The default `docker compose up` is unchanged and starts no proxy (federation stays
  dark). An external reverse proxy (Caddy/nginx) is documented as an alternative.
- **Federation groundwork (multi-vantage), hub side.** The collector now records which *vantage*
  produced each round (the hub probes as `local`); targets declare `vantages: [...]` (inherited
  down the tree, default `[local]`), and a pure builder projects each vantage's assignment plus a
  content-version hash. A TimescaleDB-backed API-key store lets an operator register vantages —
  `smoked vantage add/ls/revoke` mints a one-time key (salted-hash at rest, constant-time verify)
  — with a password-gated `/api/admin/vantages` API + session login (`SMOKED_ADMIN_PASSWORD`,
  fail-closed). Dark until an agent connects; federation requires `-dsn`. Still to come: the
  bundled reverse proxy.
- **Federation: agent endpoints + `smoke-agent` remote collector.** Agents authenticate with a
  per-vantage API key and pull `GET /agent/v1/assignment` (304-aware via `config_version`), then
  `POST /agent/v1/results`; the hub is authoritative for probe/host and ingests idempotently. The
  `smoke-agent` binary pulls its assignment, runs the same probe/scheduler pipeline, and pushes
  rounds through a bounded in-memory store-and-forward buffer. Storage reads and the continuous
  aggregates are now vantage-aware (default `local`), so multi-vantage data no longer conflates.
- **Per-vantage overlay graphs.** A target probed from more than one vantage overlays each
  vantage's median line (distinct colors) in the detail views, with a chip legend that doubles as
  a focus selector; the smoke band renders for the focused vantage. Single-vantage targets and the
  Graphs grid are unchanged.
- **Vantages admin panel.** A login-gated **Vantages** tab manages federation vantages over the
  password-gated admin API: add (one-time key + agent-snippet reveal), list (name, created,
  last-seen, target count), regenerate, and revoke. `/api/admin/vantages` now reports a
  per-vantage **target count**. The admin session cookie is `Secure`, so the panel logs in only
  over HTTPS (via the reverse proxy) or `localhost`, and it says so explicitly if the cookie
  can't be set. Disabled (with a notice) when `SMOKED_ADMIN_PASSWORD` is unset.
- **Config-tree menu** in the Graphs view: a left nav built from the target name paths
  (collapsible folders with worst-child status dots + subtree counts, a filter box, and a
  deep-linkable subtree scope that also scopes the unison Y-axis).
- **Drag-to-zoom on the detail graph.** Drag a time range on the enlarged (zoom) view and it
  refetches finer data for that span (raw ≤30h, hourly ≤10d, else daily); `GET /api/series` and
  `GET /api/rollup` accept optional `from`/`to`. A "reset zoom" restores the tier.
- **Unison Y-scaling** across the Graphs grid — small multiples share one Y-axis so a fast and a
  slow target are comparable (toggle in the legend).
- **Alert warm-start from the store.** At boot the alert engine seeds each alerted target's
  sample window from stored history, so a target that is already breaching fires on its first
  post-boot round instead of waiting for fresh samples to accumulate.
- **Tabbed dashboard with per-target drill-down.** An **Overview** tab (worst-targets +
  availability/coverage leaderboards) and a **Graphs** tab (per-target smoke grid). Clicking a
  target opens a SmokePing-style drill-down with all four ranges stacked — 3h/30h per-round
  smoke, 10d hourly band, 400d daily band — and clicking a graph zooms it. Hash-routed with
  working browser Back and deep-links (`#overview` / `#graphs` / `#target=<name>&range=<r>`).
  The dashboard app JS moved into a testable `web/dashboard.js`.
- **Daily rollup tier** for the long range: a `samples_daily` continuous aggregate feeds the
  400d view; `GET /api/rollup?res=1h|1d&window=<dur>` serves hourly or daily buckets, bounded
  to the window server-side.
- **Config directory:** `-config` also accepts a directory — `default.yaml` (database, probes,
  alerts, tree-wide target defaults) plus `conf.d/*.yaml` drop-in target branches, concatenated
  SmokePing-`@include`-style (a fragment may carry only `targets.children`; duplicate branches
  are a hard error). Ships `examples/config-dir/`.
- **Time-bounded raw series:** `GET /api/series?window=<dur>` reads the full window via a store
  `HistorySince`, so a long range (e.g. the 30h drill-down) is no longer truncated at the
  store's history cap.

### Changed
- **Async per-target scheduling.** The serving collector dispatches each target on its own
  cadence through a shared bounded worker pool, so a slow or stalled target no longer delays
  faster targets across ticks (within-round isolation already held). Serve mode also skips the
  synchronous warm-up rounds, which previously double-fired every target at startup.
- **SLA over the full window.** Availability is computed by a time-bounded store aggregate
  (`Availabler`/`AvailabilityAll`, unbounded by the history cap) with per-target coverage
  (measured vs expected rounds from each target's step); thin coverage renders as provisional
  rather than a clean bill of health.
- **Fewer store queries per refresh.** Bulk `LatestAll` / `AvailabilityAll` collapse
  `/api/targets`, `/api/charts`, `/metrics`, and `/api/sla` to one query each instead of one
  per target; the long-range rollup is windowed server-side rather than fetched whole and
  sliced in the browser.
- **Bulk, incremental Graphs grid.** The per-target smoke grid now refreshes through a single
  `GET /api/series/all?window=&since=` (store `SeriesAll`) instead of one `/api/series` per
  target every 5s. With a `since` watermark the server returns only rounds newer than the
  client already holds, so a refresh is one request + one query regardless of target count and
  transfers just the new rounds — not the whole window each tick (the last piece of the
  thousands-of-targets goal; the drill-down still uses single `/api/series`).
- **Durable webhook delivery.** Webhook notifications now go through a bounded worker pool with
  retry/backoff instead of one fire-and-forget goroutine per event. A full queue drops the
  newest event and counts it (rather than spawning unbounded goroutines); each delivery carries
  a stable `X-Idempotency-Key` so the receiver can dedupe retries and level-triggered repeats;
  failed deliveries are retried with exponential backoff; and shutdown drains the queue within a
  deadline. Delivery counters (`smokeping_webhook_queued_total`, `_delivered_total`,
  `_retried_total`, `_dropped_total`, `_failed_total`, `_queue_depth`) are exposed on `/metrics`.
- **Per-probe value validation.** Probe params are validated at config load — bool/int/port
  kinds, enums (e.g. DNS `protocol`), and a valid-record-type check for DNS `recordtype` — so a
  bad value is a loud config error instead of a silent runtime fallback; the published JSON
  Schema reflects the constraints.
- **Cleaner long-range graphs.** Hourly/daily views render as a soft translucent min→avg→max
  band (with the loss-tinted median) instead of the dense smoke stack, which showed as a
  near-black blob.
- **Hardened container.** Runs as a non-root user — `fping` gets `CAP_NET_RAW` via a file
  capability rather than root — and the TimescaleDB image is pinned to a specific release
  instead of a moving `latest-pg16` tag.

### Fixed
- **History import no longer writes rows with an invalid ping count or loss.** `smoked import
  smokeping <dir> --history` used to write a matched target's resolved `Pings` straight into
  `pgstore.ImportRow` with no validation: a target whose `Database` file was missing/unreadable
  (and with no probe/target `pings` override) resolved to `Pings=0`, which made raw `LossFraction`
  read as a false 0% and blanked the aggregate loss out to `NULL` via `NULLIF(pings,0)`.
  `runHistory` now validates each matched target's resolved ping count (1..`config.MaxPings`)
  *before* extracting or inserting anything; a target that fails is reported by name (hinting at
  the missing `Database` file), its rows are never written, the other matched targets still
  import, and the run exits with a distinct non-zero partial-failure code (rather than `0`) so a
  script can tell "some targets need attention" apart from a clean run. Each extracted sample's
  `loss` is separately checked against that target's pings; a sample with `loss<0` or `loss>pings`
  is dropped (with a warning) rather than the whole target failing — one bad round in years of RRD
  history shouldn't cost the rest of it. `pgstore.ImportSamples` also gained a defense-in-depth
  backstop (`pings<1`, `pings>MaxPings`, `loss<0`, or `loss>pings` now rejects the whole call) so
  no caller, present or future, can slip an invalid row past it.
- **Config import rejected valid fragments that relied on base-YAML inheritance.**
  `config.AppendImport` used to schema-validate the DB fragment in isolation — building a bare
  target tree and calling `Monitors()` on it *before* the fragment was ever composed with
  `default.yaml`. A target that inherited its `probe` (or referenced an alert) from the tree-wide
  YAML config, rather than setting it on the fragment itself, was wrongly rejected with e.g.
  `no probe set (and none inherited)`, even though the identical branch in a `conf.d/*.yaml`
  fragment resolves fine. `AppendImport` now does context-free validation only (the structural
  `database`/`probes`/`alerts` rejection, the additive merge, and the duplicate/idempotency
  logic) — schema validation happens once the fragment is actually composed with the base config,
  which the API's `ConfigImport` closure already does on every apply (`buildRuntime` →
  `AppendDBFragment` → `Monitors()`). `smoked config import` and `smoked import smokeping --apply`
  gained an optional `-config`/`--config DIR` flag to effective-validate the merged fragment
  against that base config before persisting; without it, an invalid fragment is now caught (and
  logged) at the daemon's next reload instead of being validated too strictly up front.
- **Agent flush no longer discards a whole oversized backlog on a 413.** A results batch that
  tripped the hub's 16 MiB body cap (`413`) used to be treated the same as a malformed (`400`)
  batch — dropped wholesale, even though every round in it except the one(s) that pushed it over
  the cap was perfectly valid and individually sendable. A high-ping-count assignment recovering
  from a hub outage with a large backlog (e.g. 5,000 buffered rounds serializing past 16 MiB)
  could lose the *entire* backlog to a single oversized batch. `pushError` now distinguishes a
  size rejection (`oversize()`, 413 only) from the broader `permanent()` (413 or 400); a new
  `Agent.sendBatch` helper, shared by `flushLoop` and `finalFlush`, retries a 413 by splitting
  the batch in half and recursing on each half — isolating the unsendable round(s) so every other
  round still reaches the hub — while a `400` (the hub's decoder rejected the request's shape, not
  its size) still drops the whole batch as before, and a transient error (5xx/429/auth) still
  propagates untouched so the caller retries the whole batch later. Only a round that alone
  exceeds the byte cap is ever dropped (and counted in the existing `rejected` metric).
- **Config reload race.** A concurrent SIGHUP reload and an API config-apply could leave the live
  runtime out of sync with the persisted config (a slow reload build swapping a stale runtime over
  a completed apply). Both writers now serialize the whole read/build/swap under one mutex.
- **Agent flush loop could wedge.** A results batch the hub permanently rejects (over the 16 MiB
  body cap or the 5,000-round limit) was retried forever, blocking every newer round behind it. The
  limits are now shared (`agentwire`), `FlushMax` is capped, the hub answers `413` for an over-cap
  body, and the agent **drops** a permanently-rejected batch (loudly, counted) instead of retrying.
- **Continuous aggregates now backfill on (re)create.** Enabling downsampling created the hourly/
  daily aggregates empty with only trailing refresh policies, so still-present raw history never
  materialized into the 10-day / long-range views; `EnableDownsampling` now runs a bounded initial
  refresh on first create or a shape-change recreate.
- **Startup validation.** A non-positive `-timeout` (which would cancel every probe immediately) is
  now rejected at the CLI boundary, and the YAML loader rejects a file with multiple documents
  instead of silently loading only the first.
- **Agent vantage transparency.** `smoke-agent` now logs the *hub-assigned* vantage (the hub
  derives it from the API key) on each assignment, and warns once when the agent's configured
  `vantage` label disagrees — so a key minted for one vantage can't quietly have its rounds
  logged under another. Internal robustness alongside it: `pgstore.Latest` binds the
  `DefaultVantage` constant instead of a hardcoded `'local'` SQL literal, and `VantageOf` reuses
  `VantageOrDefault` so the "empty ⇒ local" rule lives in exactly one place.
- **Unbounded `Ping.packetsize` could OOM or panic the collector.** The schema accepted any
  non-negative integer (e.g. `1073741824`), which reached `buildEcho`'s `make([]byte, packetsize)`
  on every send. A `maxPacketSize` (65500 bytes, under the ~65507-byte IPv4 ICMP payload ceiling
  and safe for IPv6) is now enforced in three places — the schema's `Validate` hook, the `Ping`
  factory's probe-level default, and `Measure`'s effective per-target value (a per-target override
  bypasses the first two) — so an oversized value is a loud config/measurement error instead of a
  crash. Hardening alongside it: the scheduler's per-probe goroutine (`runOne`) now recovers a
  panicking `Measure` call into a failed `Outcome` (error names the probe + target + recovered
  value) instead of taking down the whole daemon, so one misbehaving probe can no longer crash
  every other target's round.
- **Store read failures now surface as HTTP 503** on `/api/targets`, `/api/charts`, `/api/sla`,
  and `/metrics`, instead of a false-empty "0 targets" success that a Prometheus scrape could
  not distinguish from a healthy empty configuration. The base `Store.History`/`Keys` reads are
  now error-returning too, so a database failure on the no-window `/api/series` path (and the
  `Keys`/`History` fallbacks) is a 503 rather than a swallowed empty result.
- **Async result ordering:** a target stays in flight through its store-write + alert-eval
  callback, so a slow write can't let a later result for the same target overtake it and
  reorder alert history (and blocked callbacks stay bounded to one per target).
- **Ambiguous target identity rejected:** two config paths that flatten to the same name (a key
  containing `/`), or an empty name from a host on the root, are now a config error instead of
  silently merging two targets.
- **Dashboard routing/panels:** hash routing no longer double-decodes target names (names with
  `%` no longer break), and the Graphs grid removes panels for targets that disappear from
  `/api/targets` (e.g. after a SIGHUP removal).
- **`/api/sla?maxloss`** requires a finite percent in `[0,100]` (NaN/Inf/>100 rejected).
- **Config validation no longer needs the probe binary.** Probe config schemas moved to the
  registry (static, per kind), so a config lint validates every target's params even when the
  probe's external binary (fping, irtt) is absent — previously all target-var checks for that
  probe were silently skipped. The emitted `probe_config` schema now also lists target-scoped
  vars (settable at probe level as tree-wide defaults), matching what the runtime accepts.
- **HTTP url/method are validated at config time.** A malformed `urlformat` (bad scheme/host)
  or an unrecognized `method` is now a startup config error instead of phantom packet loss when
  request creation fails at runtime.
- **Empty target leaves rejected.** A non-nil but empty node (`x: {}`, or one that sets only
  defaults with no host and no children) is a config error, like a nil node — no longer silently
  ignored.
- **Probe accuracy nits.** The TCP-connect probe now applies its configured inter-attempt delay
  after a failed connect too (a down host is no longer flooded with back-to-back SYNs); and fping
  only treats its documented loss exit (code 1) as loss — any other non-zero exit (resolve
  failure, bad args, syscall error) is a probe error even when some samples came through, instead
  of being silently reported as success.
- **Round-weighted rollup summaries.** The long-range summary stats (median/loss averages) now
  weight each rollup bucket by the number of rounds it aggregates, so a sparse bucket no longer
  counts the same as a full one.
- **Strict alert numeric grammar.** Matcher and pattern thresholds are now validated at parse
  time instead of silently misbehaving: matcher args reject unknown keys (a typo no longer
  vanishes), duplicates, and non-finite values; `x` must be a whole number in `[1, 10000]`
  (`x=1.9` no longer truncates to 1); `CheckLoss` loss thresholds must be in `(0, 100]`; and
  loss/rtt pattern thresholds must be finite and in range (`>150%`, `>NaN`, negative values are
  rejected). A malformed alert is a clear startup error, not a permanently inert or always-on
  alert.
- **Timestamp-accurate graphs.** Charts now place each round/bucket by its wall-clock time
  against the selected range's fixed window, instead of spreading samples evenly by array
  index. Short data floats at its true position (10 min on the 3h axis is a right-hand sliver,
  not a full-width stretch), and a collector/DB outage wider than the target's cadence renders
  as a real blank span — the median line and smoke bands break across it instead of drawing a
  straight line over the gap.

#### Federation hardening (code review, 2026-08-08)
- **Pre-vantage databases upgrade cleanly.** Migration now `ADD COLUMN IF NOT EXISTS vantage`
  before building the `(target, vantage, ts)` index, so a 0.1-era install (created before the
  vantage column existed) no longer fails startup with “column vantage does not exist”; existing
  rows backfill to `local`. Covered by a pre-vantage-schema upgrade fixture.
- **Remote agents honor the hub's probe-level config.** The assignment now carries each target's
  effective `probes.<Kind>` config and folds it into the config-version hash, and the agent builds
  probes with it — so DNS `protocol: tcp`, HTTP `method: HEAD`, etc. take effect remotely, and a
  probe-config change bumps the version (no more stale 304s) instead of silently using defaults.
- **Remote-only targets are reachable in the UI/API.** `/api/targets` now lists the full
  configured catalog (with each target's vantages), and `Active`/`Steps` are built from all
  vantages — so a `vantages: [nyc]` target appears in the tree, its deep link resolves to its own
  vantage, and `/api/targets?vantage=nyc` / `/charts` / `/sla` no longer drop it. The menu shows a
  neutral “no data” dot rather than a false green for a target with no data in the viewed vantage.
- **Agent ingest validates samples and timestamps.** Ingested rounds with non-finite, negative, or
  absurdly large RTTs/durations, or timestamps beyond an allowed future skew / past horizon, are
  rejected — so clock skew, an agent bug, or a stolen key can't poison the latest/rollup data.
- **Alerts evaluate per vantage.** Remote rounds are alert-evaluated after durable ingest; alert
  windows, firing state, event identity, and webhook idempotency keys now include the vantage, and
  warm-start seeds each vantage from its own history — so a local outage and an nyc outage on one
  target fire independently instead of sharing (or deduping) state.
- **Rollup median weighting excludes lost rounds.** The continuous aggregates expose
  `median_rounds` (rounds that produced a median) alongside total `rounds`; the detail summary now
  weights the median by `median_rounds` and loss by total rounds, so a mostly-lost bucket's one
  slow surviving round no longer skews the long-range median average. (One-time aggregate rebuild.)
- **`/api/series?vantage=` honored without a window.** The no-window recent-tail path now reads the
  selected vantage (vantage-aware capped read) instead of always returning local data.
- **Agent shutdown drains its whole buffer.** The final flush loops over all buffered batches
  (bounded by a fresh deadline) instead of sending just one — a controlled shutdown after a hub
  outage no longer strands every buffered round past the first batch.
- **`smoke-agent` rejects invalid config.** Strict YAML decode (unknown keys error), all
  file+flag values merged before a single validation, and positive-bound checks — so
  `flush_max: -1` (which panicked the buffer) and other non-positive/negative values are refused;
  `agent.Options` is validated at the package boundary too.
- **Graceful shutdown no longer loses the final local rounds.** `PGStore.Add` bounds its write
  with a fresh timeout context, not the (already-cancelled) process context, so rounds draining
  through the dispatcher at shutdown still persist.
- **Config rejects unprovisionable / duplicate vantage names** at load time (same validator as the
  key store and read API), instead of leaving a typo'd remote-only target permanently dark.

Follow-up review pass:
- **Detail views keep your selected vantage.** The stacked/zoom graphs auto-refresh every 30s;
  the refresh no longer resets a multi-vantage target's focused vantage back to the default, so a
  chip you picked stays selected.
- **A folder of only no-data targets shows the neutral dot**, not a false green, matching its
  children (e.g. a subtree containing only remote-only targets before their agent reports).
- **`smoke-agent -insecure=false` overrides a config file's `insecure: true`** (a bool flag
  couldn't previously distinguish "false" from "unset"); a multi-document YAML config is now
  rejected rather than silently ignoring everything after the first document.
- **Bounded alert-pattern matching.** The `*N*`-skip matcher memoizes its search, so a pattern
  with many skips can't blow up exponentially on the alert-eval path.

### CI / ops
- **CI hardening.** The pipeline now also enforces `gofmt` and `go mod tidy` cleanliness, runs
  the test suite under the race detector, runs the checked-in Node frontend tests, and runs a
  (non-blocking) `govulncheck` dependency scan — previously it was Go vet/build/test only.
- **Compose publishes the API on localhost only.** `docker-compose.yml` binds the collector's
  published port to `127.0.0.1` instead of all host interfaces, with guidance to front it with a
  reverse proxy (TLS + auth + rate limiting) for access beyond the host — the API is
  unauthenticated and query-heavy.

### Changed (earlier)
- Dashboard refresh is lighter on the collector: an in-flight guard skips a refresh tick
  while the previous one is still running (no request pile-up on slow links / many targets),
  and the aggregate panels (worst-targets, availability) poll on a slower 15s cadence rather
  than every 3s — cutting their per-target store scans ~5×.

### Fixed
- Per-target polling intervals are honored while serving: each target now fires on its own
  `step` via a scheduler Planner, instead of every target polling at the single global
  `-step`. A config's `step: 60s` (or a per-branch override) is no longer silently ignored.
  (The `-rounds` foreground demo still runs synchronized rounds at `-step`.)
- Alert priority inhibition no longer emits an orphan RESOLVED: an alert whose FIRING was
  suppressed by a higher-priority one never sends a lone RESOLVED, while an alert that was
  delivered before being inhibited still gets its close-out (tracked via a delivered-view
  flag, carried across config reload).
- Reject non-positive/absurd `pings` and `step` at config load and on the CLI (a negative
  `pings` previously panicked the collector from a scheduler goroutine); `sample.Compute`
  also clamps defensively.
- `HTTP.insecure_ssl` and DNS `recordtype` now take effect per target, matching their
  advertised target scope (they were read only from probe-level config).
- pgstore reads `duration_ms` back, so `smokeping_probe_duration_seconds` is no longer
  always zero for TimescaleDB-persisted targets.
- SIGHUP reload no longer loses a round's alert-state update on the reload boundary: the
  round's evaluation and the reload's state-inheritance+swap are serialized, and evaluation
  runs against the live runtime.
- `/api/sla` reports the actual covered span (`covered_from`); the dashboard shows the real
  availability window instead of always claiming "last 24h" when history is shorter.

### Added
- Dashboard theme choice persists across reloads (localStorage), restored before first
  paint to avoid a flash; with no saved choice the page still follows the OS preference.
- Availability / SLA reporting (the tSmoke equivalent): `GET /api/sla?window=24h` reports
  per-target availability over a time window — rounds measured, rounds up, availability %,
  and mean loss. A round counts as "up" if it got at least one reply (loss < 100%); pass
  `maxloss` for a stricter threshold. Sorted worst-first; a dashboard "Availability" panel
  surfaces the lowest.
- Top-N "charts" (worst offenders): `GET /api/charts?by=loss|median|stddev&n=N` ranks
  targets by their most recent round (loss %, median latency, or per-round jitter/stddev);
  targets with no value for the chosen key (a fully-lost round has no median/stddev) are
  excluded from that chart rather than ranked as best. A "Worst targets" panel in the
  dashboard renders it with a Loss/Latency/Jitter toggle.
- Probe config as JSON Schema: `GET /api/probes/schema` emits each probe's config
  variables as JSON Schema (draft 2020-12) — probe-level and per-target vars as separate
  closed objects — generated from the same `VarSpec` source that drives runtime validation,
  so docs and external validators can't drift from what the collector accepts.
- Alert priority inhibition and per-target `alertee` (`internal/alert`, config): a per-alert
  `priority` (1 = highest) suppresses lower-priority alerts on the same target while it
  fires (Alertmanager-style inhibition); a per-target `alertee` list adds extra notifier
  recipients on top of each alert's own `to`, inherited down the target tree and deduped at
  dispatch. (A RESOLVED is delivered only when the matching FIRING was — see Fixed below.)
- Richer alert pattern DSL (`internal/alert`): `*N*` skip (0..N arbitrary samples
  between hard comparisons), bare `*` (one arbitrary sample), and `==U`/`!=U` for the
  unknown value (a lost round's missing rtt median). Patterns are right-anchored to the
  newest sample. Table-driven tests cover the two bugs the Perl original shipped —
  `*N*` alignment (documented patterns like `>0%,*12*,>0%` now match) and unknown-vs-0%
  loss. Degenerate patterns (no comparison, `U` on loss, `U` with `<`/`>`) are rejected
  at parse time.
- Structured logging (`log/slog`) with `-log-format` (text|json) and `-log-level`, plus a
  per-round `"round complete"` record.
- Operational metrics on `/metrics`: per-probe timing (`smokeping_probe_duration_seconds`)
  and round-level `smokeping_rounds_total`, `_round_duration_seconds`, `_round_targets`,
  `_round_errors`, `_last_round_timestamp_seconds`.
- SPA raw↔hourly resolution selector: the hourly view renders the downsampled min→max
  envelope with the avg median; degrades gracefully when the store has no rollup tier.

### Changed
- Config reload (SIGHUP) now preserves alert firing state and sample windows, so a reload
  no longer re-fires firing alerts or drops the history a hysteresis alert needs.

### Note
- The SmokePing `S` startup sentinel is intentionally not supported. It existed only
  because SmokePing's alert state was in-memory and lost on restart; with a durable store,
  "already bad at startup" is answered from real history (blueprint §07).

## [0.1.0] - 2026-08-03

First tagged release: a working, non-Perl SmokePing core. A fast parallel poller
with pluggable probes, TimescaleDB persistence with a downsampling tier, a canvas
smoke-graph dashboard, YAML config with inheritance, an alert engine, Prometheus
metrics, and one-command Docker bring-up. All three founding requirements met —
the smoke-graph look, a fast/parallel poller, and probes as plugins.

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

[Unreleased]: https://git.fiber.house/munro/smokeping-modern/-/compare/v0.1.0...main
[0.1.0]: https://git.fiber.house/munro/smokeping-modern/-/tags/v0.1.0
