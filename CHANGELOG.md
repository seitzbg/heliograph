# Changelog

All notable changes to **Heliograph** are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/) and the project aims to follow
[Semantic Versioning](https://semver.org/) — 1.0.0 is the first stable release.

## [Unreleased]

## [2.0.0] - 2026-09-03

### Changed
- **The Graphs vantage selector caps at 4 simultaneous vantages.** The overlay palette has four
  distinct colors (plus the neutral color for `local`), so a 5th overlaid vantage would reuse a color
  and become indistinguishable. At the cap, the un-selected vantage chips are disabled with an
  explanatory tooltip ("deselect one to add …"); you can always toggle one off to free a slot.
  Vantages beyond four can still exist and be viewed — you just can't overlay more than four at once.

### Added
- **Copyable deployment examples under `examples/`.** Two ready-to-run Compose stacks using the
  prebuilt GHCR image — `examples/standalone/` (collector + TimescaleDB, the common single-host case)
  and `examples/federation/` (adds a Caddy reverse proxy for remote vantages) — each with its own
  `.env.example`, plus an `examples/README.md` that helps you pick. Federation is opt-in, so a
  standalone user is never faced with the TLS/ACME/Basic-Auth settings they don't need.
- **Opt-in mutual-TLS federation, replacing the per-vantage API key.** Start the hub with
  `-serve -agent-addr :8443 -agent-hostname <domain>` (requires `-dsn`; the listener runs only in
  serve mode) to run a dedicated, mutual-TLS listener for remote agents, entirely separate from the
  dashboard's HTTP server. The hub
  self-bootstraps its own CA (generated once, persisted in the database) and issues both its own
  server certificate — SAN taken from `-agent-hostname` — and a CA-signed client certificate for
  each vantage; the listener requires and verifies that client certificate before any request
  reaches a handler. The certificate's CommonName *is* the vantage's identity, checked against the
  registry on every request, so revoking a vantage takes effect immediately without waiting on
  certificate expiry.
- **One-click vantage onboarding from the dashboard.** The Vantages admin tab's **Add vantage**
  now downloads a ready-to-run `<name>-vantage.tar.gz` — `agent.yaml` (hub URL, vantage name, and
  the client certificate/key/CA embedded as PEM) plus a matching `docker-compose.yml` and
  `README.txt` — instead of revealing a key to copy. The CLI equivalent, `smoked vantage add
  <name> -out <name>-vantage.tar.gz`, mints the identical bundle (or prints the rendered
  `agent.yaml` to stdout, or `-json` for the raw PEMs).

### Removed
- **BREAKING: the per-vantage API key is gone.** `smoke-agent`'s `-key` flag and the config file's
  `key:` field no longer exist — replaced by `-client-cert`/`-client-key`/`-ca-cert` (flags, file
  paths) and `client_cert`/`client_key`/`ca_cert` (config, inline PEM). Every existing federated
  vantage must be re-onboarded with a certificate bundle before it can report to an upgraded hub:
  run `smoked vantage add <name>` (or use the dashboard's Add vantage) and redeploy the agent with
  the new `agent.yaml`. The old `Authorization: Bearer smk_...` agent auth path, and the reverse
  proxy's `/agent/v1/*` forwarding rule that carried it, are both removed — agents now connect
  directly to the hub's own `-agent-addr` mTLS listener instead of going through the dashboard's
  reverse proxy.

### Security
- **Minting a vantage now requires HTTPS.** `POST /api/admin/vantages` — the endpoint that issues a
  vantage's mTLS client identity, returned either as the downloadable `<name>-vantage.tar.gz` bundle
  or (JSON fallback) as raw PEMs — refuses to mint over a plaintext connection (`403`), so the
  embedded **private key never crosses the wire in the clear**. smoked serves plain HTTP behind a
  TLS-terminating reverse proxy, so it reads the client scheme from `X-Forwarded-Proto: https`
  (direct TLS on smoked itself is also honored); a **loopback** peer stays exempt (it never crosses
  the wire), and the `smoked vantage add` CLI — which writes the bundle to a file/stdout — remains
  the local/headless path. Operators fronting the dashboard with their own nginx must forward
  `proxy_set_header X-Forwarded-Proto $scheme;` (Caddy's `reverse_proxy` sets it automatically); see
  *Federation deployment (reverse proxy)* in the README and `docs/federation.md`.
- **Config reads redact credentials embedded in a probe URL.** The open config reads
  (`GET /api/admin/config`, its `?source=effective` variant, and `GET /api/admin/config.yaml`) strip
  HTTP `urlformat` userinfo, query strings, **and the URL path** for a non-admin reader — the path is
  a common credential carrier (Discord/Slack/PagerDuty webhook tokens live there), so a value like
  `https://%host%/hooks/TOKEN` is now shown as `https://%host%/[redacted]` rather than verbatim.
  A credential placed anywhere in a probe URL is no longer shown to everyone who can reach the
  dashboard. A logged-in admin still receives the real, editable config — redacting the editable
  source would let the next save persist the mask over the secret (CODE_REVIEW M11).

### Fixed
- **The Overview no longer reports a target as down from a vantage that doesn't measure it.** The hub
  probes every configured target locally, so a target scoped to remote vantages only (e.g. an internal
  resolver an off-LAN hub can't reach) recorded 100%-loss *local* rounds. Those made it dominate the
  "Worst targets" board and read as 0% availability, and turned its nav-tree dot red — a false outage
  for a target that is healthy from the vantages that actually measure it. `/api/charts` and `/api/sla`
  now rank only targets measured from the requested vantage (matching the Graphs grid), and a tree dot
  ignores a vantage the target isn't assigned to.
- **The Graphs grid shows the focused vantage's NTP clock stat.** On the multi-vantage grid, a panel
  focused on a remote vantage now displays that vantage's clock offset/stratum instead of the hub's —
  the companion stat follows the panel's plotted series, matching the detail view (CODE_REVIEW M2).
- **Remote-only targets appear in the Graphs grid for their vantage.** Selecting a remote vantage now
  surfaces targets measured only from that vantage (a site the hub can't reach directly), instead of
  leaving them reachable only in the nav tree and detail view (CODE_REVIEW M10).
- **Graphs overlay colors no longer collide when more than four vantages exist.** Overlay colors are
  assigned from the selected (≤4) set instead of the full vantage catalog, so two simultaneously-drawn
  overlays can't land on the same palette slot; a pre-existing saved selection of 5+ vantages is also
  clamped to the cap on load (CODE_REVIEW M12).
- **Federation `.env` bcrypt instructions no longer corrupt the hash.** The `DASH_PASSWORD_HASH`
  guidance dropped the "double every `$` to `$$`" step for the single-quoted value — single quotes
  already stop Compose from interpolating `$`, so doubling produced a literal `$$…` and broke the
  dashboard's Basic Auth. Paste the hash single-quoted, exactly as generated (CODE_REVIEW M13).
- **A failed Graphs refresh no longer blanks the last-known graphs.** A failed `/api/series/all`
  request is now distinguished from a successful one with no new rounds, so it leaves the cached
  series untouched instead of aging it out — a transient outage, or a background tab woken after more
  than the 3h window, keeps showing last-known data rather than collapsing to "collecting data…".
  Dashboard series fetches also carry a bounded timeout so a hung request can't wedge the refresh
  loop, and the visible view refreshes immediately on tab wake (`visibilitychange`/`pageshow`)
  (CODE_REVIEW M14).
- **The stacked-detail view no longer flashes blank on its 30-second refresh.** A periodic refresh
  now fetches the new data before repainting the existing canvases in place, instead of clearing the
  grid first, so a slow or hung read leaves the current graphs visible; same-target refreshes are
  serialized so sustained slow responses can't starve every generation and leave the panels blank
  (CODE_REVIEW M15).
- **Graph peaks are no longer clipped to the top of the frame.** The detail and grid y-axis
  auto-scale now folds the median line's own extremes into the range (not just the smoke-band
  quantiles), so a latency spike that rises above the band is drawn in full instead of being clamped
  flat against the top edge.
- **A stalled response body can no longer wedge the Graphs refresh.** The request abort timeout now
  stays armed through the body read (`json()`/`text()`/`blob()`), not just until the response headers
  arrive, so a connection that delivers headers and then stalls mid-body is aborted at the configured
  timeout instead of hanging the refresh loop (CODE_REVIEW M16). A completed response that reads no
  body now clears its abort timer immediately rather than holding it until the timeout fires
  (CODE_REVIEW L12).
- **The zoomed (fixed-range) detail view no longer starves its own refresh under slow responses.**
  The 30-second periodic refresh admits only one fixed-range refresh at a time, so sustained slow
  reads can't pile up; an explicit navigation or drag-zoom still supersedes an in-flight refresh
  (CODE_REVIEW M17).
- **The Overview and the logged-in Vantages table no longer overflow narrow viewports.** Overview
  moves its coverage figure to a second SLA row and the Vantages table scrolls inside its own panel,
  so both surfaces fit at 320–360 px wide instead of pushing the page sideways (CODE_REVIEW M18).
- **The dashboard and API refuse to mint an onboarding bundle when the hub runs no agent listener.**
  With no `-agent-hostname`/`-agent-addr` configured, Add — and, for an existing vantage, Regenerate —
  previously still downloaded a `<name>-vantage.tar.gz` whose embedded hub URL pointed at a
  placeholder no agent could reach. The Vantages tab now disables onboarding while `federation_ready`
  is false, and the mint endpoint rejects the request server-side with `409 Conflict` (naming the
  missing `-agent-hostname`/`-agent-addr` prerequisite) *before* issuing any certificate, so a dead
  bundle or half-registered vantage is never produced (CODE_REVIEW M19, M20).
- **The per-panel band-owner label on the Graphs grid can no longer contradict the graph.** When a
  panel's smoke band belongs to a vantage other than the toolbar's focused one, the panel carries a
  small text `band <vantage>` marker naming the actual band owner (drawn in the neutral median color,
  no swatch), instead of letting the shared legend imply the focused vantage (CODE_REVIEW L11).

### Docs
- Added an **alerting operator guide** ([`docs/alerting.md`](docs/alerting.md), linked from the
  README): how to define alert matchers and the pattern DSL, wire up each notifier
  (log/webhook/Slack/Discord/email), the idempotency-key behavior on retries, and the relevant
  `SMOKED_*` / `SMOKED_SMTP_*` environment variables.
- Clarified that a deep link survives a target move only while its path is still current; a dormant
  bookmark to a target's *old* path, first reopened after the move, can go blank or resolve to a
  reused path — prefer the app's id-based links for saved URLs (CODE_REVIEW L8).

## [1.0.16] - 2026-08-24

### Changed
- **The Config tab now defaults to your Effective config, not the DB fragment.** The Effective | DB
  source toggle now applies to both the Tree and YAML views (previously YAML-only), and the tab opens
  on **Effective** — the file+DB merged config the collector actually runs — so you see your real
  config instead of an empty database fragment. Effective is read-only (you can't edit file-defined
  targets from the browser); switch the source to **DB** to add or edit database-backed targets. New
  read-only endpoint `GET /api/admin/config?source=effective` (open read, same non-secret data as the
  effective YAML) backs the effective tree.

## [1.0.15] - 2026-08-24

### Added
- **View the config as YAML in the Config tab.** A Tree | YAML toggle sits beside the target tree; the
  YAML view is read-only and offers two sources — **DB** (just the targets stored in the database) and
  **Effective** (your YAML files merged with the DB targets, exactly as the collector runs it). It reads
  like a hand-written config file: durations render as `60s`, and unset/inherited fields are omitted
  rather than shown as `null`. Served by a new open read `GET /api/admin/config.yaml?source=db|effective`
  (alongside `GET /api/admin/config`); the config holds no secrets, so it needs no admin login, and every
  write endpoint stays gated.

### Fixed
- **The dashboard no longer serves stale JavaScript after an upgrade.** Static assets (`index.html`,
  `dashboard.js`, `smoke.js`, CSS) were served with only a `Last-Modified` header, so a browser could
  keep running the pre-upgrade dashboard against a freshly-deployed server until a hard refresh — which
  could leave panels stuck on "loading" after switching vantages. They now carry `Cache-Control:
  no-cache`, forcing the browser to revalidate (a cheap 304 when unchanged, a fresh copy the moment the
  file changes). Assets are still cached; they're just never served stale.

## [1.0.14] - 2026-08-24

### Changed
- **Tidier Graphs toolbar.** The reference key and the interactive controls are now separated into two
  zones (a divider between them), and the 8-chip loss legend is a single compact green→red strip
  (each bucket's threshold on hover) — so the vantage/cols/unison controls no longer fight the legend
  for space. It scales down cleanly too: the zones wrap and the divider drops on narrow screens.
- **The Vantages and Config admin pages are viewable without logging in.** Any user who can reach the
  dashboard (behind the reverse proxy's Basic Auth) can now see the vantage list and the DB config
  tree read-only; adding/revoking a vantage and editing/importing config still require an admin login.
  The vantage list never includes keys, and the config doc holds no secrets (notifiers are referenced
  by name; the actual credentials come from the environment), so nothing sensitive is exposed. The
  `GET /api/admin/vantages` and `GET /api/admin/config` endpoints are no longer admin-gated; every
  write endpoint still is.

## [1.0.13] - 2026-08-24

### Changed
- **Graphs grid hides targets a selected vantage doesn't measure.** When you view the grid from a
  vantage (or vantages) that doesn't probe some targets — e.g. a Comcast vantage that doesn't measure
  the hub's loopback or internal resolvers — those panels are now hidden with a "N targets not
  measured — hidden" note, instead of drawing a blank "collecting…" graph. Each shown panel's band
  follows the first selected vantage that actually measures it, so a shown panel never blanks either.
  Hidden targets stay in the left nav tree and the detail view.

## [1.0.12] - 2026-08-24

### Added
- **Vantages on the Graphs page.** A vantage control on the Graphs toolbar (shown only when a
  deployment measures from more than one vantage) picks which vantages draw on every panel: the
  focused one owns the min–max band + median, the rest overlay as median lines — so selecting one
  vantage is a clean single view and selecting all is a full fiber-vs-vantage overlay. The nav and
  panel status dots now reflect the **worst** vantage (even one you've hidden), so a target that is
  healthy locally but losing packets from another vantage still flags on the overview. Defaults to
  `local`; the selection is remembered per browser.

### Docs
- `config.example.yaml` now shows a `vantages:` example, so the reference config demonstrates
  measuring a subtree from both the hub and a remote vantage (federation), not just single-vantage.

## [1.0.11] - 2026-08-23

Stable target identity, documented and hardened. A target's history now follows a stable, server-
managed `id` rather than its position in the tree, so moving or renaming a node keeps its graph —
and this release closes the follow-up gaps in how that identity interacts with imports, the
dashboard, and remote vantages.

### Added
- **Stable target identity.** Every target created or imported through the admin UI carries an
  opaque, server-minted `id`; its history, rollups, and alert state are keyed by that id, so
  reorganizing the tree (moving a target into a different group, or renaming it) preserves the
  existing graph instead of starting a new one. A target defined only in `config.yaml` has no minted
  id and falls back to its tree path, so renaming it there still starts a fresh graph. The id is not
  something you edit; the UI never exposes it.
- **NTP clock stat in the detail views, per vantage.** The stacked and zoomed drill-downs now show a
  target's offset/stratum for the vantage in focus (not only the Graphs grid), so a remote vantage's
  NTP server shows its own clock reading in the detail view.

### Fixed
- **Imported targets keep their history.** Targets added via the admin config import, `smoked config
  import`, or `smoked import smokeping --apply` are now assigned a stable id at import time, so a
  later ordinary save no longer re-keys them and drops the history collected in between; re-running
  the same import stays an idempotent no-op even after an id has been minted.
- **The NTP clock stat follows the current server and never rolls backward.** The offset/stratum
  stat is bound to the exact endpoint it was measured against (host, port, and protocol version) and
  to its measurement time, so repointing a target at a different server — or a slow in-flight probe of
  the previous endpoint, or an out-of-order store-and-forward replay — can no longer show a superseded
  or older reading; a remote vantage's stat is only published for rounds that were durably committed.
- **Remote NTP stat survives a move.** A `smoke-agent` now looks up its companion clock stat by the
  target's stable id, so a moved (or newly created) NTP target still reports its offset/stratum
  upstream.
- **Assignment versioning and rolling upgrades track the stable id.** The per-vantage assignment
  version changes when a target's id changes, and the hub accepts a round reported under a target's
  current path from an agent predating stable identity. When an old display path is reused by a new
  target, the hub disambiguates the two by the round's fingerprint, so a fingerprint-carrying agent is
  still attributed correctly — only a pre-fingerprint agent reporting a reused path pauses until
  upgrade, rather than silently misattributing or dropping the data.
- **Deep links and Overview links follow a move.** Dashboard navigation, the URL hash, detail
  fetches, and the Overview worst-offenders / SLA boards are all keyed by the stable id, and a
  path-based `#target=` hash is rewritten to the id once the target catalog loads — so a link built
  or opened while its path is still current keeps working after the target later moves. A dormant
  bookmark to a target's *old* path, first reopened only after the move, is the exception: the server
  keeps no old-path alias, so it opens blank (or, if that path was since reused by another target,
  resolves to the new occupant). The app emits id-based links once loaded — prefer those for anything
  saved long-term.

### Docs
- Documented the stable-identity lifecycle end to end: the server-managed id and YAML path fallback in
  `config.example.yaml`, a "Stable target identity" rolling-upgrade section in `docs/federation.md`
  (reconciling how a pre-identity or pre-fingerprint agent behaves across a move or a reused path), and
  the shipped feature plus its residual follow-ups in `ROADMAP.md`.

## [1.0.10] - 2026-08-22

Close the remaining NTP review follow-ups. The clock offset/stratum stat is now tied to a target's
current identity and to the vantage that measured it, and it travels the federation wire, so a
remote vantage's NTP server shows its own clock reading rather than nothing — or the hub's.

### Added
- **NTP clock stat at remote vantages.** A round now carries its companion offset/stratum stat over
  the federation wire (`smoke-agent` fills it from the probe's latest-value registry), and the hub
  keeps it per vantage, so an NTP target measured at a remote vantage shows that server's clock
  offset and stratum. The wire fields are optional, so a mixed old-agent/new-hub fleet just shows no
  remote stat, exactly as before.

### Fixed
- **The NTP clock stat follows the current target, not a stale round.** `/api/targets` takes a
  target's probe identity from the live config, so a name reused for a different probe (or an
  in-place probe change) no longer shows the previous target's offset/stratum, and a remote
  vantage's panel is never decorated with the hub's own local clock reading.

### Docs
- Corrected the README probe contract and the `ntpprobe` comments: in `measure: offset` mode the
  signed clock offset is itself the graphed sample, and adding a probe also means registering it in
  the shared `internal/probe/allprobes` list (blank import + parity test), not just dropping in a
  file.
- Refreshed the dashboard screenshots from the v1.0.9 UI and tightened the README Features section.

## [1.0.9] - 2026-08-21

Harden the NTP probe and keep clock offset and latency apart everywhere. Every round now records
whether its samples are round-trip times or a signed clock offset, so offset data is no longer
stored, alerted on, charted, or scraped as if it were latency; a server that declares itself
unsynchronized no longer graphs as a healthy clock; the example config no longer contacts public
NTP by default; and NTP now works at remote vantages.

### Added
- **NTP at remote vantages.** `smoke-agent` registers the NTP probe — both binaries now pull the
  probe set from one shared list, so the hub and agent can't drift apart — so an NTP target assigned
  to a vantage is measured instead of dropped as `unknown probe "NTP"`. Offset-mode targets send
  their signed samples over the federation wire.
- **`heliograph_ntp_offset_seconds`** — an offset-mode target's clock offset gets its own Prometheus
  gauge, so `/metrics` no longer mislabels it as a round-trip time.
- **`interval_ms`** on the NTP probe, to pace the requests within a round.
- **A dedicated Probes section** in the README, describing what each of the eight probes measures.

### Changed
- **Safe NTP defaults.** The example config points its NTP target at loopback instead of public
  NTP; `pool.ntp.org` / Cloudflare are a commented, one-request-per-round opt-in, per the NTP Pool
  terms and vendor guidance. The probe paces its own requests and stops on a Kiss-o'-Death reply.
- **A metric kind is stored with every round.** rtt vs offset is now a column on the `samples`
  table and is carried through the continuous aggregates, the read API, alerts, charts, and
  federation, so the two meanings are never averaged into one series, rollup, or alert window. The
  signed-axis choice comes from the effective config (probe-level default plus per-target override),
  so it survives a restart and a target that has no data yet.

### Fixed
- **Clock offset is not latency.** A signed offset can no longer trip a latency alert, rank on the
  Latency/jitter charts, or export under the RTT median gauge.
- **An unsynchronized server no longer reads as a healthy clock.** Stratum 0 (Kiss-o'-Death),
  stratum 16+, a leap-indicator alarm (`LI=3`), and a reply that fails the origin/timestamp checks
  all record RTT reachability but no clock offset; the offset/stratum stat clears when a server
  stops answering or loses sync.
- **Offset request/response validation.** The NTP request stamps a transmit timestamp and requires
  the reply to echo it, and rejects zeroed or reversed server timestamps, before publishing an
  offset.
- **Microsecond offsets are readable** — axis labels and panel stats keep enough precision for
  sub-millisecond offsets instead of rounding to `0.00`, and an all-negative offset series reports
  its real maximum rather than a fabricated `0`.
- **The browser layout test** no longer leaves its self-compiled binary behind on a bare run.

### Upgrade note
Adding the metric column bumps the continuous-aggregate schema, so on the first start after
upgrading, the hourly/daily rollups are dropped and rebuilt from the last 30 days of raw samples —
**rollup buckets older than the 30-day raw retention are lost, once.** Existing NTP offset history
predates the metric column and backfills as `rtt`, so it drops off the now-offset panels; fresh
offset data accumulates from the upgrade.

## [1.0.8] - 2026-08-20

Graph the NTP clock **offset** as a smoke graph — the NTP probe's `measure: offset` mode plots the
server's clock offset over time (band = measurement jitter, line = the offset), which taught the smoke
renderer a signed, zero-baselined y-axis. Detail below.

### Added
- **NTP clock offset as a smoke graph.** The NTP probe gains `measure: rtt|offset` (per target). In
  `offset` mode the graphed series is the server's **clock offset** rather than the query RTT — the
  smoke band is the per-round measurement jitter and the median line is the offset over time, so drift,
  correction steps, and sync noise read at a glance (the RTT line was near-flat and dull). This taught
  the smoke renderer a **signed y-axis**: a series that can go negative gets a zero-centered range with
  a dashed zero baseline and no 1&nbsp;ms floor, while every latency graph (values ≥ 0) is byte-for-byte
  unchanged. On an offset panel the offset stat is dropped as redundant (it's the graph); RTT and
  stratum remain. Config with `params: { measure: offset }`.

## [1.0.7] - 2026-08-20

Adds the **NTP probe** — the eighth built-in probe and the last roadmap feature — which graphs an NTP
server's query round-trip time and shows its clock offset + stratum as stats. Plus a CI reliability
follow-up so a stalled package mirror fails fast instead of hanging. Detail below.

### Added
- **NTP probe.** A native SNTP probe (no external `ntpdate`/`chronyc`) that queries an NTP server over
  UDP/123 and graphs the request round-trip time as an ordinary smoke graph — RTT + loss like every
  other probe. It also surfaces the server's **clock offset** and **stratum**: these aren't latencies,
  so rather than distort a min→median→max plot they render as stats beside median/loss on the target's
  panel (offset in ms, plus the stratum). Config it with `probe: NTP` (params: `port`, default 123;
  `version`, 3 or 4). Unsynchronized servers (stratum 0/16+) still register reachability + RTT but no
  offset. The eighth built-in probe; the schema self-publishes at `/api/probes/schema`.

### Fixed
- **CI retries now recover from a *stalled* network install, not just a failed one.** Each `apt-get` /
  Playwright attempt is wrapped in `timeout`, so a hung package mirror becomes a failure the retry can
  act on instead of sitting until the job timeout. The affected job ceilings were widened to cover up to
  three timed-out retries. (Follow-up to the v1.0.6 CI hardening, which retried failures but not hangs.)

## [1.0.6] - 2026-08-19

Post-1.0.5 review follow-ups and CI reliability (#75–#77): the demo Compose stack honors
`SMOKED_ABSOLUTE_TIME`, the browser-layout regression now gates image publication and can no longer
false-pass, the Columns picker keeps an honest, screen-reader-legible selection when it wraps, and the
web suite is locale-independent. CI gains per-job timeouts and retries so a stalled package mirror
fails fast instead of hanging, and the README screenshots are refreshed to the shipped UI. Detail below.

### Fixed
- **The bundled Compose stack now honors `SMOKED_ABSOLUTE_TIME`.** The collector service interpolates
  `${SMOKED_ABSOLUTE_TIME:-1}` instead of a hard-coded `1`, so setting the documented value in `.env`
  (e.g. `0` for relative axes) takes effect on `docker compose up`. `.env.example` also now marks which
  `SMOKED_*` settings the default (non-`federation`) `up` reads, since several were mislabeled as
  proxy-only.
- **A failed browser-layout regression can no longer be published.** `build-image` now depends on the
  `layout-test` job, so the narrow-viewport check gates image publication rather than running beside it;
  the `AGENTS.md` local gate documents the browser test too.
- **The layout regression test can no longer false-pass on a port collision.** It binds a fresh per-run
  port and fails loudly if the collector exits before serving, instead of silently exercising whatever
  else answers on a fixed port. The bare `node web/layout.test.mjs` path now compiles the collector to a
  temp binary up front rather than `go run` — whose still-running compiler let a bind failure surface too
  late to catch — and the child is watched through the browser assertions as a backstop.
- **The Columns picker always shows its active selection.** A stored column count too wide for the
  current viewport stays visible and pressed — marked as wrapping to the count that fits — rather than
  leaving the control looking unselected. Assistive technology now hears an accessible label stating the
  effective wrapped count, so a screen reader is no longer told "6" while two columns render.
- **The `rangeLabels` unit test is locale-independent.** It no longer requires Latin letters in
  localized date labels, so the web suite passes on non-Latin runner locales.

### Changed
- **Playwright dev dependency bumped to 1.55.1** (from 1.55.0), clearing a high-severity npm advisory
  (GHSA-7mvr-c777-76hp) in the browser-test toolchain.
- **CI is hardened against stalled package mirrors.** Every job now has a `timeout-minutes` ceiling so
  a wedged step fails fast instead of hanging to GitHub's 6-hour default, and the flaky network installs
  (`apt-get`, Playwright/Chromium) retry with backoff — turning an intermittent mirror stall into a
  brief wait rather than a manual cancel-and-rerun.

### Docs
- **README dashboard screenshots regenerated from the v1.0.5 UI.** The Overview, Graphs, Config, and
  Vantages captures now show the shipped defaults — absolute clock-time axes, the one-column option, and
  the linked version footer — instead of the earlier interim UI. `docs/img/README.md` records the
  repeatable capture recipe so future refreshes stay consistent.

## [1.0.5] - 2026-08-19

Post-1.0.4 graph-axis and release-tooling polish (#72–#73): absolute clock-time x-axis labels become
a uniform server default (the per-browser toggle is gone), the footer build version links to its
source, and `:latest` is refreshed on release tags so a freshly cut version reaches the rolling image
immediately. Detail below.

### Changed
- **Absolute-time axis labels are now a server config default (on), not a per-browser toggle.** The
  dashboard labels every graph's x-axis with absolute clock time by default — uniformly across the
  grid, the per-target detail stack, and the zoom drill-down (a custom drag-zoom was already
  absolute). The legend's "absolute time" checkbox is removed; set `SMOKED_ABSOLUTE_TIME=0` (or
  `-absolute-time=false`) to switch the whole dashboard back to relative `-3h`/`now` labels. The
  effective setting is served at `GET /api/version`.
- **The footer build version links to its source** — the exact commit for a `main`/`latest` build,
  or the release page for a clean tag.
- **`:latest` is also refreshed on release tags**, so right after cutting `vX.Y.Z` the `:latest`
  image carries that clean version (a `git describe` on `main` runs before the tag exists, so a
  main-only `:latest` lagged one version).

## [1.0.4] - 2026-08-16

Post-1.0.3 UI and release-tooling work (#62–#70): the build version now shows in the dashboard
footer, the Graphs axes gain an absolute/relative time toggle, and the graphs-per-row picker is
smarter — a single-column option, `rem`-based (font-relative) sizing, and it stays visible on
narrower/high-DPI screens. It also completes the narrow-viewport overflow fix with a headless-browser
regression test, reconciles the admin session-lifetime docs, publishes the image as `:latest`, and
refreshes the README with full-width screenshots. Detail below.

### Added
- **Build version in the dashboard footer.** The footer now reads `Heliograph <version>` — the
  git-describe build string (latest tag + commits + short SHA for a `main`/`latest` build, or the
  exact tag for a release), served from a new `GET /api/version`.
- **Absolute-time axis toggle on the Graphs view.** An **"absolute time"** checkbox in the legend
  switches the per-graph x-axis from relative labels (`-3h` … `now`) to absolute wall-clock times —
  clock time (e.g. `08:02 PM`) for short ranges, calendar dates (`Aug 6`) for multi-day ones. It
  applies to the small-multiple grid and the per-target detail stack, and is persisted per browser;
  a drag-to-zoom range already shows absolute times regardless.

### Docs
- **README refreshed** — the verbose "What works today" section is now a concise, scannable
  **Features** list; the dark dashboard screenshots were regenerated at the new full-width layout
  (2560px, incl. the Graphs columns picker).
- **Smoke-graph demo goes full-width** — `web/smoke-poc.html` drops its fixed 1160px column so the
  proof-of-concept fills the window like the dashboard, and its README screenshot was regenerated to
  match (the graphs now span the full width instead of a narrow left column).

### Changed
- **The collector image is now published as `:latest`** (a continuous alias of the default-branch
  head, alongside the existing `:main` and per-release `:vX.Y.Z` tags). The docs and the generated
  agent Compose reference `ghcr.io/seitzbg/heliograph:latest`.
- **The Graphs "Columns" picker adapts to the window and offers a single-column view.** It now
  includes a **`1`** option (one full-width graph) and hides the counts that can't fit at the current
  width (they would only wrap to fewer). The picker shows whenever at least two columns fit — on a
  laptop where `Auto` gives two, `1` is still a distinct choice — and hides only when one column is
  all that fits. It re-evaluates on window resize.
- **Graph minimum width is font-relative (`rem`), not a fixed pixel count.** The per-graph minimum is
  now `22.5rem` (== 360px at the default 16px root) so graphs scale with the reader's font-size
  setting instead of staying a fixed size.

### Fixed
- **Narrow/phone-width Graphs no longer force a horizontal page scroll.** The mobile
  `.graphs-layout` column is now `minmax(0, 1fr)` (not a bare `1fr`, whose min-content floor let the
  grid outgrow the viewport before the per-graph `min(360px, 100%)` clamp could apply), and the
  top-bar tab strip scrolls inside its own width instead of pushing the page. Verified in a headless
  browser at 320px (document width == viewport, grid within its container). Completes the 1.0.3
  narrow-overflow fix, which only addressed the per-graph track.
- **Admin session-lifetime docs/UI now match the feature.** The login modal no longer hard-states a
  12-hour session (it says "the hub's configured lifetime, 12 hours by default"); `.env.example` and
  the federation guide now document `SMOKED_ADMIN_SESSION_TTL` alongside the password and signing key.

### Tests
- **Browser-layout regression test** (`web/layout.test.mjs`, new CI `layout-test` job) renders the
  real dashboard against the demo collector in headless Chromium and asserts the Graphs view does
  not overflow narrow (320/360px) viewports — the class of bug the DOM-less unit tests can't see,
  which shipped once. Playwright is the first (dev-only) npm dependency; the existing unit tests
  still run under plain `node`.

## [1.0.3] - 2026-08-15

The next release after v1.0.2, gathering the post-1.0.2 UI and hardening work (#50–#60): a
full-width dashboard with a per-row **Columns** control, a classic top bar with global admin
login/logout, in-place Config-tab group creation and rename, a configurable admin session lifetime
(`SMOKED_ADMIN_SESSION_TTL`) signed by a key that is now independent of the login password, notifier
hardening (permanent-`4xx` abandonment and no redirect-following), application-versioned continuous
aggregates, and the build-once-promote supply-chain pipeline. Detail below.

### Changed
- **Webhook/Slack/Discord notifiers now abandon permanent (HTTP 4xx) failures** instead of retrying
  them through the whole backoff budget. A 4xx (bad payload, auth, wrong URL) can never succeed on
  retry, so it is counted as failed on the first attempt — mirroring the email notifier's 5xx /
  AUTH-not-offered handling. The state-dependent 408 Request Timeout, 421 Misdirected Request,
  423 Locked, 425 Too Early, and 429 Too Many Requests stay transient (retried); 409 Conflict does
  not, because replaying the same payload cannot resolve an application conflict.
- **Rename a Config-tab target or group in place.** The Edit form's Name field is now editable; a
  changed name rekeys the node, preserving its complete imported schema and visible sibling position
  even when sibling weights tie (previously rename meant remove + re-add).
- **Clearer, scrollable Config modals.** Each field's label now sits above a full-width control
  instead of labels and inputs flowing inline and wrapping, and a modal taller than the viewport
  scrolls internally so Save stays reachable; save errors show inline in the open modal.
- **Classic top bar.** Navigation and controls moved into a single bar across the top — brand ·
  tabs · theme · admin — with the title/description below it, instead of a hero block with the
  theme control floating in the corner and login buried inside tabs.
- **Full-width dashboard layout.** The page content no longer stops at a fixed 1240px centered
  column — it fills the browser window (side gutters scale from 22px up to 48px on wide screens),
  so wide and ultrawide displays use the whole width instead of showing large empty margins.
  Paragraph text keeps its own readable-width cap.

### Added
- **Configurable admin session lifetime** via `SMOKED_ADMIN_SESSION_TTL`. A login stays valid for
  12 hours by default; set the env var to any Go duration (`24h`, `168h`, `30m`, …, minimum `1m`) to
  lengthen or shorten both the signed token's expiry and the cookie's `Max-Age`. An unparseable or
  sub-minute value is rejected at startup.
- **Choose how many graphs per row** on the Graphs tab. A **Columns** control in the legend
  (`Auto · 2 · 3 · 4 · 6`) sets the grid width: `Auto` fits as many as the viewport allows (the
  default), while a fixed count caps the columns but never shrinks a graph below a ~360px minimum —
  when the chosen count won't fit, the grid wraps to fewer columns instead of squeezing. The choice
  persists per browser (`localStorage`).
- **`SMOKED_WEBHOOK_URL` environment fallback** for the generic `-webhook` notifier, matching the
  existing `SMOKED_SLACK_WEBHOOK` / `SMOKED_DISCORD_WEBHOOK` / `SMOKED_SMTP_*` env support.
- **Create target groups from the Config tab.** A new **"+ Add group"** button opens a modal that
  creates a folder (site) plus one or more child targets in a single step — e.g. `Google` → `ICMP`,
  `DNS` — with optional group-level Vantages/Alerts inherited by every child. A group can't be empty
  (the config validator rejects a node with no host and no children), so it is created with its first
  targets; add more later with the folder's `+`.
- **Global admin login/logout in the top bar.** One shared login modal — plus an "Admin · Log out"
  indicator — replaces the separate password forms that lived inside the Config and Vantages tabs;
  when signed out those tabs point to the top-bar control. New `POST /api/admin/logout` (clears the
  session cookie, which is HttpOnly so JS can't) and `GET /api/admin/session` (a whoami probe that
  drives the bar's Log in / Admin state on every tab).

### Fixed
- **Webhook/Slack/Discord notifiers no longer follow redirects.** The delivery client now returns
  `http.ErrUseLastResponse` on a 3xx, so a redirect is evaluated as the non-2xx response it is
  (transient, retried) instead of being chased to its final status. Previously the default client
  followed a `302`→`200` and recorded a phantom successful delivery (never retrying a misdirected
  endpoint), and a `307`/`308` re-POSTed the alert body to the redirect target — an untrusted host.
- **Graphs grid no longer overflows narrow viewports.** The per-graph minimum width is clamped to
  the container (`min(360px, 100%)`) in both the CSS and the columns helper, so a window narrower
  than the minimum collapses to a single full-width column instead of forcing a horizontal scroll.
  The restored **Columns** choice is also validated against the offered options, falling back to
  `Auto` for any stale or hand-edited `localStorage` value.
- **Config edits and tree operations are non-destructive.** The form now changes only the fields it
  owns, preserving imported `title`, `ip`, `pings`, `step`, `alertee`, weight, children, and unknown
  future fields. Dropping on a sibling folder moves into it; rename keeps tied-weight order; a
  hostless group whose last child moves/removes is pruned rather than sent as invalid config; and
  imported node keys containing `/` are rejected because browser paths use `/` structurally.
- **Admin sessions use an independent signing secret.** Set `SMOKED_ADMIN_SESSION_KEY` to 32 random
  bytes encoded as 64 hex characters (for example, `openssl rand -hex 32`) for restart-persistent
  sessions. Without it, the collector generates a secure process-local key and warns that restart
  will log admins out. The signing key is no longer derived from `SMOKED_ADMIN_PASSWORD`, so a stolen
  cookie cannot be used as a fast offline password verifier.
- **Admin/login probes no longer paint stale state.** Auth probes are generation-ordered;
  network/5xx responses preserve the last confirmed state; logout claims success only after the
  cookie-clearing 204; and older in-flight probes cannot overwrite a newer login/logout result.
- **Remote ingest closes the reload gap for legacy agents.** Every accepted report now carries the
  hub snapshot's computed target identity even when the agent omitted a fingerprint, so the locked
  commit rejects a target redefined during validation. Commit-time drops are included in response
  accounting, while valid idempotent replays remain accepted.
- **Bulk graph reads use the configured target catalog.** `/api/series/all` feeds its indexed lateral
  top-N query from live configured targets (filtered by vantage), eliminating both raw-history
  `DISTINCT target` scans and excluding historical rows for removed targets.
- **Continuous aggregates have an application-owned schema version.** Startup validates the exact
  relation identity, complete ordered columns/types, Timescale source/settings, bucket, grouping,
  and aggregate expressions before adopting an older or restored view, then records the version
  against its relation OID. A same-named but semantically stale view is rebuilt.
- **Image promotion is preventive and verified per tag.** CI first copies the scanned OCI archive to
  a staging tag with `skopeo --preserve-digests`, verifies and attests it, then promotes from its
  immutable digest reference. Every branch/release/SHA tag is preservation-gated and inspected; a
  staging or attestation failure occurs before those release aliases change.
- **The Config/Vantages status line stays live without crossing routes.** Those tabs probe
  `/api/targets` on entry and every 15s, and a late response is ignored after the user navigates away.

## [1.0.2] - 2026-08-14

First **published** 1.0.x release. It gathers all the post-1.0.0 hardening and UI work (#34–#49):
the Config-tab tree with drag/keyboard reorder and WAI-ARIA semantics, author-defined menu order
(`weight`), display-name + resolved-IP graph titles, one-click vantage deployment, Slack/Discord
notifiers, SMTP-notifier hardening, the build-once-promote supply-chain pipeline, and a batch of
data-path and probe-timing fixes. The `v1.0.1` tag was cut from the same tree but never published
— its release pipeline hit a TimescaleDB test-timing flake; **v1.0.2 fixes that flake (#49) and
supersedes v1.0.1** with no functional difference. Detail below.

### Added
- **Config-tab tree: keyboard control, ARIA semantics, cross-folder drag, and add-into-folder (#37 follow-ups).** The Config tab's target tree now implements the WAI-ARIA `tree` pattern — `role="tree"`/`treeitem`/`group`, `aria-expanded` on folders, `aria-selected` on the active node, and a single roving `tabindex`. It is fully keyboard-operable: Up/Down move between visible rows, Left collapses a folder (or steps to its parent), Right expands (or steps into the first child), Home/End jump to the ends, and **Alt+Up / Alt+Down reorder the focused node among its siblings** (persisting via the same weight path as drag). Folders can now be **collapsed/expanded** (a twist chevron, or the arrow keys). A node can be **dragged into a different folder**, not just reordered within its current one — dropping onto a folder moves it inside, onto a row moves it beside — and both affected sibling groups are re-sequenced; dropping a folder into its own descendant is refused. Each folder gains a **"+"** affordance that adds a new target **into** that folder (a nested path), alongside the unchanged top-level add. The move/add/reorder/guard logic lives in pure, unit-tested helpers (`moveNode`, `addNodeAtPath`, `moveInList`, `cfgVisibleRows`, `cfgTreeKey`). (The same-parent folder-drop edge case was corrected in Unreleased.)
- **Config-tab tree UI for database targets, with drag-to-reorder.** The Config tab now renders database-managed targets as a nested tree (YAML targets are not shown here; they remain file-managed). Each target can be dragged to reorder among siblings (a reorder updates `weight` via the admin API, persisted without SIGHUP), edited, or removed at any depth with a path-aware form showing probe/host/params/vantages. Top-level add is unchanged. Cross-folder move and keyboard-accessible reorder are explicit follow-ups.
- **Author-defined menu order (`weight`).** Any config node (YAML or DB fragment) can carry an
  optional `weight:` (int); siblings sort by `(weight, name)` instead of strict A–Z — a negative
  weight pins a node to the top of its group, unset/`0` preserves the alphabetical default. Honored
  by the config flatten, `/api/targets`, the dashboard menu, and the grid. (Drag-to-reorder in the
  Config tab lands in a follow-up.)
- **One-click vantage deployment from the browser.** The Vantages panel's reveal-key dialog now presents the
  agent setup as two copyable/downloadable files behind a toggle: **`agent.yaml`** (the per-vantage
  `hub`/`vantage`/`key`, with `hub` prefilled to the hub you're viewing and a durable `spool_dir`) and a
  ready-to-run **`docker-compose.yaml`** that mounts it and runs the agent. **Copy** and **Download** act on
  whichever file is shown, and the key lives only in `agent.yaml`. The published image now ships **both** the
  `smoked` and `smoke-agent` binaries (the compose overrides the entrypoint to `smoke-agent`), so bringing a
  vantage up needs only those two files and `docker compose up -d`.
- **Display name + IP in the graph title.** A target can carry a `title:` (a display-name override, shown
  in the graph header instead of its tree-path key) and, with `-resolve-ips` / `SMOKED_RESOLVE_IPS=1`, its
  IP: a pinned `ip:` field, else a literal-IP `host`, else the hostname resolved at config-load (best-effort,
  concurrent, refreshed on SIGHUP reload). The header then reads `<probe> <title-or-name> (<ip>)`. Both fields
  are display-only — kept out of the measurement fingerprint, so editing a label or pinned IP never resets a
  target's stored series. Opt-in; default titles are unchanged. (`title`/`ip` on `config.example.yaml` nodes,
  `/api/targets`, and the dashboard grid/detail/zoom headers.)
- **Native-`Ping` compare targets in the demo config.** The demo (`config.example.yaml` and the built-in
  target set) now probes Cloudflare/Google DNS with **both** `FPing` and the native `Ping` probe against
  the same host, so the two ICMP probes render side by side for comparison.
- **`AGENTS.md` setup guide.** An agent-/contributor-facing build/test/run reference (prerequisites, the
  full local test gate mirroring CI, run/verify, container build, layout, conventions), and `.gitignore`
  now excludes local review/tooling artifacts (`CODE_REVIEW.md`, `.playwright-mcp/`).
- **Slack and Discord alert notifiers.** Alongside the log + generic-webhook notifiers, alerts can now
  fan out to **Slack** (`-slack-webhook` / `SMOKED_SLACK_WEBHOOK`) and **Discord** (`-discord-webhook` /
  `SMOKED_DISCORD_WEBHOOK`), referenced from an alert as `to: [slack]` / `to: [discord]`. Both reuse the
  webhook delivery pool (bounded queue, retry/backoff, graceful drain) and post a human-readable
  message; their delivery counters are exported under a `notifier` label.
- **Email (SMTP) alert notifier.** Alerts can fan out to email via `-smtp-addr` / `-smtp-from` /
  `-smtp-to` (plus `-smtp-user` / `-smtp-pass` for authenticated submission — STARTTLS when the server
  offers it), or the matching `SMOKED_SMTP_*` env vars, referenced from an alert as `to: [email]`.
  Async bounded-queue delivery with a graceful drain and `heliograph_email_*` metrics. `-smtp-insecure`
  (or `SMOKED_SMTP_INSECURE=1`) skips STARTTLS cert verification for an internal relay with a
  self-signed cert; a relay that advertises no STARTTLS is handled too. (Live-verified end to end.)
- **README screenshots.** Dashboard (Overview + per-target Graphs) and the smoke-graph renderer are
  now shown in the README (`docs/img/`).
- **Project `LICENSE` (MIT).** The v1.0.0 source shipped with no license, leaving downstream use,
  modification, and redistribution rights unstated; MIT makes them explicit.
- **`smoked -serve` reads `SMOKED_DSN` / `SMOKED_CONFIG` / `SMOKED_DOWNSAMPLE` from the environment.**
  Only the `import`/`vantage` subcommands previously defaulted `-dsn` to `SMOKED_DSN`; the serve path
  required the DSN — with its password — on the command line. These three flags now default to their
  matching env vars (an explicit flag still wins), so Compose/K8s can supply them via `environment:`
  and keep the DB password out of the command list. The bundled `docker-compose.yml` and the README
  Compose example now do exactly that instead of carrying a `command:` list.

### Changed
- **CI publishes the exact image it scanned (build-once-promote).** The collector image was built
  twice — once in `build-image` (scanned + SBOM'd, read-only) and again in `publish-image` (pushed +
  attested) — so the blocking Trivy scan gated a *different* build than the one released, and a cache
  miss or moving `apk` repo could make them differ. `build-image` now builds **one** OCI archive,
  scans and SBOMs that exact artifact, and hands it to `publish-image`, which pushes it with
  `skopeo` and checks the published digest before attaching the provenance attestation. The
  Unreleased follow-up turns that post-push detection into a pre-promotion `--preserve-digests`
  gate and verifies every tag. The read-only PR build / write-only non-PR publish split is
  unchanged. (CODE_REVIEW M1.)
- **Trivy vulnerability suppressions use `.trivyignore.yaml` with real expiry.** The allowlist was a
  flat `.trivyignore` documenting a `CVE-… exp:<date>` syntax that Trivy silently ignores — a
  suppression a maintainer added would never actually expire (or, for the pseudo-syntax, never apply).
  It is now `.trivyignore.yaml` in Trivy's documented format (`vulnerabilities: [{id, statement,
  expired_at}]`), wired via `--ignorefile` into all three scans (collector, Caddy, scheduled `:main`),
  and a CI lint rejects any entry missing `expired_at` (no silent forever-suppressions). (CODE_REVIEW L4.)
- **Hardened the email/SMTP alert notifier.** Each delivery attempt now runs under a bounded
  per-transaction deadline (dial + STARTTLS + auth + data, default 10s) instead of risking an
  indefinite stall on an unresponsive relay, and failed sends are retried with exponential
  backoff up to a configurable attempt limit (default 4, mirroring the webhook delivery pool),
  interrupted early on shutdown. A relay that doesn't advertise `AUTH` when credentials are
  configured now returns an explicit error instead of silently falling back to an
  unauthenticated session. (`EmailConfig.Timeout`/`MaxAttempts`/`BaseBackoff`; new
  `heliograph_email_retried_total` metric.)
- **Band panels explain the "collecting…" placeholder.** A long-range band panel with too little history
  to draw (fewer than 2 buckets) now says what it's waiting for — *"collecting… — daily band appears once
  history spans 2 days"* (and the hourly equivalent) — instead of a bare "collecting…". On a fresh
  deployment the 400-day daily band is empty for up to a day simply because one UTC day is a single bucket;
  the new copy makes that read as "filling in", not "broken". Raw (per-round) panels are unchanged.
- **Demo target labels name the probe method, not "DNS".** The compare targets were named `DNS (ICMP)` /
  `DNS (native Ping)` — misleading, since those are ICMP pings to a DNS *server's* IP, not DNS queries.
  They're now grouped by provider and named by method: under `Cloudflare` / `Google`, the leaves are
  `ICMP (FPing)`, `ICMP (native)`, `TCP :443`, and `DNS query` (the only actual DNS-protocol probe).
  The Compose demo also runs with `-resolve-ips`, so each target's IP shows in its graph title (e.g.
  `Cloudflare/ICMP (FPing) (1.1.1.1)`, `Websites/cloudflare.com (104.16.…)`) rather than being baked into
  the folder name. Renaming changes a target's stored identity, so the demo's renamed series start fresh.
  (`config.example.yaml`, the bundled `docker-compose.yml`, the built-in demo set, and the README sample.)
- **Probe badge now precedes the graph title** — e.g. `[HTTP] cloudflare.com (HTTP TTFB)` instead of
  the badge trailing the name — in the per-target grid and the detail/zoom titles.
- **Graphs grid defaults to per-panel Y-axis auto-scaling.** The small-multiples grid previously shared
  one Y-axis (unison) by default, which flattened low-latency panels against the tallest target. Each
  panel now auto-scales to its own data by default; the **unison scale** toggle (top of the grid) still
  turns on a shared axis for cross-target comparison.
- **Renamed to Heliograph.** The project's outward identity is now **Heliograph** — the GitHub repo
  (`seitzbg/heliograph`), the container image (`ghcr.io/seitzbg/heliograph`), CI badges, and docs.
  The internal naming now follows suit: the Go module path is **`github.com/seitzbg/heliograph`** (was
  `smokeping-modern`), the Prometheus `/metrics` are exported under the **`heliograph_*`** prefix (was
  `smokeping_*` — **breaking** for any dashboard/alert scraping the old names), and the image's baked
  config lives at **`/etc/heliograph/config.yaml`** (was `/etc/smokeping/`). The daemon binary stays
  **`smoked`**, and the `smoked import smokeping` subcommand keeps its name — it reads data from the
  original SmokePing tool, so that reference is deliberate.
- **CI and container registry moved to GitHub.** The project now lives on GitHub; CI runs as GitHub
  Actions workflows (`.github/workflows/ci.yml` for test → build → scan, `image-scan.yml` for the
  scheduled re-scan) replacing the former `.gitlab-ci.yml`, and the collector image is published to
  GHCR (`ghcr.io/seitzbg/heliograph`) instead of the git.fiber.house registry. The main blocking test
  jobs — Go tests, frontend, `govulncheck`, and the TimescaleDB integration suite — and their
  blocking/report-only semantics were preserved. The runner model changed from GitLab's digest-pinned
  job containers to GitHub-hosted `ubuntu-latest`; remaining image-scan/provenance/pinning gaps are
  tracked in `CODE_REVIEW.md`.
- **Hardened the GitHub Actions supply chain.** Every action is pinned to a commit SHA (with a version
  comment; Renovate keeps them current), the syft/trivy scanner images are digest-pinned, and
  `packages: write` was narrowed from the workflow default to the single publishing job (`publish-image`)
  — the default token is now read-only — and checkout no longer persists the `GITHUB_TOKEN` in
  `.git/config` (`persist-credentials: false`). The `ubuntu-latest` runner drift is documented as an
  accepted hosted-CI tradeoff. (CODE_REVIEW L1/M2/M3 + CodeRabbit.)
- **Supply-chain release gates completed.** The finished-image Trivy scan is now **blocking** (the base
  is CVE-clean, so a newly-disclosed fixable HIGH/CRITICAL reds CI — suppress with a time-bound
  `.trivyignore.yaml` entry, or bump the base pin). Publishing is split into a non-PR `publish-image` job
  that holds the ONLY write scopes (`packages`/`attestations`/`id-token`), so PR builds can never
  publish; it attaches a signed **build-provenance attestation** (a verifiable source-ref → image-digest
  mapping) to the pushed image. CI also builds + scans the bundled **Caddy** reverse-proxy image
  (`Caddy.Dockerfile`). Only enabling the Renovate bot (a repo GitHub-App install) remains. (CODE_REVIEW
  M2 remainder + CodeRabbit.)
- **README: prebuilt-image quick start binds loopback.** The Docker quick start published the
  unauthenticated dashboard/read API on all interfaces (`-p 8087:8087`); it now binds
  `127.0.0.1:8087:8087`, matching the Compose file and the README's own loopback-only guidance.
  Also added a copy-pasteable Docker Compose example (with a single `SMOKE_DB_PASSWORD` variable shared
  by the DB and the collector's DSN) and code-review acknowledgements. (CODE_REVIEW M1 + CodeRabbit.)
- **Container supply chain hardened: all images digest-pinned, plugins version-pinned, SBOM + image
  scanning, and a refresh config.** Every base image (Go build, Alpine runtime, Caddy builder/runtime,
  TimescaleDB, and the CI `golang`/`node`/`docker` images) is now pinned by digest, and every bundled
  Caddy DNS-provider plugin is pinned to a version — so builds are reproducible and a moving upstream
  can't change a credential-bearing binary without a reviewed change. A new `image-scan` CI stage
  generates an SPDX SBOM (artifact) and runs a finished-image vulnerability scan (Trivy) on top of the
  existing Go-module `govulncheck`; it also re-scans the current `:main` on scheduled pipelines to
  catch newly-disclosed CVEs against pinned images. A `renovate.json` keeps the digests and plugin
  versions current so the pins don't go stale. (The finished-image Trivy scan is now blocking — see
  "Supply-chain release gates completed" above.) CODE_REVIEW M4/L7.
- **Spool recovery streams each segment instead of loading it whole and copying every body.** On
  agent restart, recovery read each segment fully into memory and copied every decoded body before
  deciding which to keep, so a segment full of dead or budget-evicted rounds still cost a full copy
  of every body — pushing the transient footprint well above the live budget on constrained vantages.
  It now streams frame-by-frame and unmarshals only the records it retains; torn-tail/corruption/
  contiguity/eviction behavior is unchanged.
- **`/api/series/all` is now bounded in the query, not only the response.** The store previously
  ranked the ENTIRE windowed set with a `row_number()` window and only then filtered to the newest N
  per target, so the database's scan+sort scaled with every row in the window even though the JSON
  response was capped. `SeriesAll` now reads each target's newest rounds through an indexed per-target
  `LIMIT` — a `CROSS JOIN LATERAL` walking the `(target, vantage, ts)` index — under a global budget
  of `min(perTarget, budget/targets)`. This bounded the per-target fetch but still discovered targets
  from raw history; the Unreleased configured-catalog follow-up removes those remaining `DISTINCT`
  scans. Measured on 1.5M rows / 300 targets, the initial lateral change cut a 48-hour bulk read from
  ~584 ms to ~142 ms and its buffer reads ~4.7×. The response carries
  `truncated` only when a target genuinely had older rounds dropped (fixing a false flag at the exact
  per-target cap).
- **Startup and reload now warn about alert recipients with no enabled notifier.** An alert `to` or a
  target `alertee` referencing an unknown notifier (a typo, or `webhook` without `-webhook`) was only
  noticed when an event was dispatched — invisible until the incident whose notification got silently
  dropped. `buildRuntime` now logs the unresolved recipients at startup and on every reload.
- **Runtime container image moved off end-of-support Alpine 3.20 to a digest-pinned Alpine 3.22.**
  Alpine 3.20 left normal security support on 2026-04-01; the runtime stage now pins a supported
  branch by digest (bump the tag + digest together on a refresh). CODE_REVIEW M4.
- **CI: the TimescaleDB integration (`db-test`) and vulnerability scan (`govulncheck`) are now
  blocking**, so "green CI" proves the database-backed paths and the vuln scan actually passed
  rather than skipped. `db-test` waits for the database with an explicit readiness loop (two
  consecutive `pg_isready` successes) instead of a fixed sleep, so making it blocking doesn't add
  startup flakiness. CODE_REVIEW M6.
- **CI now installs `rrdtool`, so the SmokePing RRD-extraction path is actually exercised.** The
  `rrd`-extraction and `import … --history` tests skip without the binary, and neither CI image
  installed it — the most bug-prone importer code (RRA stitching / tier selection) ran in no job.
  `rrdtool` is now installed in the blocking `go-test` job (RRA stitching) and the `db-test` job
  (the `--history` end-to-end path).
- **The bundled Caddy image's vulnerability scan is now blocking.** CI already built and scanned the
  Caddy reverse-proxy image (`Caddy.Dockerfile`), but the Trivy step ran with `continue-on-error:
  true`, so a fixable HIGH/CRITICAL in that build could not fail the pipeline. It now mirrors the
  collector's finished-image gate exactly — the same digest-pinned Trivy image, the shared
  `.trivyignore` mount, and `--severity HIGH,CRITICAL --ignore-unfixed --exit-code 1` — and reds CI
  on a real finding instead of only reporting it. (CODE_REVIEW M2.)
- **`smoked`/`smoke-agent` report their real build version instead of a hard-coded `1.0.0`.** Both
  binaries defined `var version = "1.0.0"` with no build-time override wired up, so every build —
  including a bare local `go build` — claimed to be the 1.0.0 release regardless of what was
  actually compiled. `version` now defaults to `dev` (an unversioned build must not claim a
  release), and the Dockerfile accepts a `VERSION` build-arg injected via `-ldflags
  -X main.version=...`; CI computes it from `git describe --tags --always --dirty` and passes it to
  both the PR-build and publish `docker/build-push-action` steps, so `smoked -version` /
  `smoke-agent -version` on a published image reports the actual commit/tag it was built from.
  (CODE_REVIEW M8.)

### Security
- **Go toolchain bumped 1.26.5 → 1.26.6** for the standard-library fixes in GO-2026-6089 (`net/http`),
  GO-2026-6090 (`crypto/tls`), and GO-2026-6218 (`net/url`), which govulncheck flagged as reachable from
  the collector's HTTP server, the HTTP/DNS probes, and the agent client. No source changes; `govulncheck
  ./...` is clean on 1.26.6.

### Fixed
- **Sequential probes' inter-attempt delays no longer overrun the round deadline.** `TCPConnect` and `SSH`
  sleep `interval_ms` between their N attempts, but that sleep happens *outside* the per-attempt contexts,
  while the per-ping budget divided the *whole* remaining round budget by `pings` — so the real total was
  `pings × (budget/pings) + (pings-1) × interval`, which overruns the round deadline. With a large
  `interval_ms` the parent deadline fired mid-sleep and the loop bailed after the **first** attempt, breaking
  the guarantee that a dead host is probed all N times. The new `probe.PerPingBudgetWithDelay` reserves the
  mandatory `(pings-1)` delays from the budget *before* dividing, and clamps the effective interval so the
  delays can never consume more than half the round budget (bounding `interval_ms` against `step`/`pings`,
  which the probe can't validate at construction). `HTTP`/`DNS` (no inter-attempt sleep) and `pings=1`/
  no-deadline callers are unchanged. Regression-tested end-to-end (a large `interval_ms` still probes all N
  within the deadline) plus a unit test of the budget/clamp math.
- **Native `Ping` honors a `timeout_ms` larger than 1 s in its send schedule.** `spreadInterval` reserved a
  hardcoded 1 s tail for replies regardless of the configured per-reply timeout, so a `timeout_ms` > 1 s was
  accepted but the send schedule could push the last echo so late that less than the requested window
  remained before the round deadline — `replyDeadline` then clamped the last echo's wait below the
  configured timeout, causing avoidable loss on a high-latency target. The effective reply timeout is now
  passed into `spreadInterval` and reserved as the tail (still capped at half the round budget for a short
  `step`); the default 1 s behavior is unchanged. Regression-tested (a `timeout_ms` > 1 s against a short
  step keeps the full window in the schedule).
- **Email notifier no longer retries permanent SMTP failures.** A failed send that can never succeed
  on retry — a 5xx server rejection (unknown recipient, relay denied; `net/smtp` surfaces these as a
  `*textproto.Error` with `Code >= 500`) or the AUTH-not-advertised misconfiguration — is now abandoned
  on the first attempt and counted `failed`, instead of burning the full retry/backoff budget (~41.5s)
  on a doomed send. During an incident burst that wasted budget tied up a worker and filled the queue,
  dropping other alerts. Transient failures (4xx replies, connection errors) still retry with backoff as
  before. The AUTH misconfiguration is matched robustly on a wrapped sentinel (`errors.Is`), not the
  message string; the operator-facing error text is unchanged.
- **Target status dot no longer flips orange on a single dropped ping.** The nav-tree status dot keyed on
  the **last round's** loss, so one lost ping (1 of 20 = 5%) painted a target "degraded" (orange) until the
  next clean round — even though its long-run loss was ~0 and the drill-down graph showed nothing. The dot
  now keys on a **windowed recent average** (`recent_loss_pct`, the last 30 min, from the bulk availability
  scan already used by `/api/sla`), so it only goes orange on **sustained** loss. A real outage is still
  immediate — a fully-errored or ≥50%-loss last round shows red at once (the blackhole demo target stays
  red). Falls back to the last round when a store can't aggregate the window.
- **First-start no longer crashes when the database is still coming up.** On a fresh `docker compose up`
  the collector could exit immediately with `configstore: migrate: ... connect: connection refused`,
  leaving TimescaleDB running but the app dead until a `down`/`up` — because TimescaleDB's first-run init
  brings up a temporary socket-only server (TCP `listen_addresses=''`) while it initializes, so the
  compose healthcheck's socket `pg_isready` reports **healthy before TCP 5432 is up**, and
  `depends_on: service_healthy` then released `smoked` into a refused connection it treated as fatal.
  Fixed at two layers: the collector now **waits for the database to accept a connection** at startup
  (bounded retry with backoff via `pgstore.WaitReady`, logging progress) instead of crashing on the first
  refused connection — robust under any orchestration, not just the bundled compose; and the compose
  healthcheck now checks **TCP** (`pg_isready -h 127.0.0.1`), which stays unhealthy until the real listener
  is up. Reproduced on a real host (forcing the healthcheck into the init window made `smoked` exit 1) and
  verified fixed. Regression-tested (`retryUntilReady` / `WaitReady`).
- **Native `Ping` probe no longer freezes for the whole round budget on a lost ping.** The probe had no
  per-reply timeout: its receiver waited until all N replies arrived *or* the read deadline, which was set
  to `ctx.Deadline()` — the entire round budget (60s at the default step). A single genuinely-lost ping can
  never let the round see all N replies, so it blocked for the full budget. Live docker5 data made it stark:
  every native round with any loss took **exactly 60s**, versus FPing's ~10s (fping bounds each reply with
  `-t`), and each 60s round overran its 60s step so the scheduler skipped the next slot — native collected
  ~14% fewer rounds than FPing to the same host, visible as gaps in its series precisely when there was loss.
  The receiver now bounds the trailing wait to **one reply window after the actual last send** (the native
  analog of fping `-t`, default 1s, capped by the round budget — measured from the real last-send time so
  send jitter can't close the window before the last echo is out), so a lossy round finishes ~10s like FPing
  and never skips a slot. A new `timeout_ms` probe var overrides it. (Loss *accuracy* was already correct — native and FPing agree to within
  noise; this fixes round cadence and worker-hold time, not the loss count.)
- **HTTP/DNS/TCP/SSH probes bound each ping to a fair share of the round budget.** The per-ping round
  budget (below) is `min(timeout × pings, step)`, but the four sequential probes still ran all N pings
  under that one shared deadline. Against a hung or blackholed host the *first* connect/query consumed
  the whole budget and the loop then bailed — so the round made **one** real attempt instead of N and
  tied up a worker for the entire `step`. This is the demo's `Unreachable/blackhole` target (TCPConnect
  to `192.0.2.1:9`): with `step=60s, pings=20` a round held a worker ~60 s for a single dropped SYN.
  Each ping now runs under an **even, fixed share** of the round budget (`budget/pings`, always
  ≤ the configured `-timeout`), computed once and applied uniformly — a fast early ping can't hand its
  unused time to a later one and push it past `-timeout`. So a dead host is probed all N times (correct
  loss) with each attempt failing fast, while a slow-but-responding endpoint still answers within its
  share. Behavioral tests cover the SSH, HTTP, and DNS probes against in-process hung endpoints (plus a
  fast-early/hung-later cap regression); the shared `probe.PerPingBudget` / `probe.AttemptContext` helpers
  are unit-tested; `pings=1` and no-deadline callers are unchanged. Builds on the per-ping round budget below.
- **Native `Ping` probe spreads its sends like `FPing` (was a 50 ms burst).** The `Ping` probe sent its
  N echoes 50 ms apart — a ~1 s burst per round — the same pattern fixed for `FPing`: it inflates loss on
  a marginal link and spikes the instantaneous ICMP rate to a single destination (noticeable with two
  compare targets pointed at the same host, e.g. 1.1.1.1). The sends are now spread across the round
  budget (`min(timeout × pings, step)`), capped so a fast link's round stays short; `interval_ms` remains
  an optional override. Peak rate to a destination drops from ~20 pings/s (for 1 s) to ~2 pings/s.
- **Smoke graphs no longer draw past the plot frame, and the median line is lighter.** A value above the
  graph's auto Y-max is clamped to the top, but the *width* of the line/tick drawn there spilled ~1px
  past the top frame; the plot is now clipped to its rect. The median line is also thinner (previously a
  1.4px neutral base with a 2.2px loss-coloured line stacked on top → 1.0px + 1.5px).
- **FPing over-reported loss on marginal links (ping burst).** The FPing probe sent all N pings in a
  ~1 s burst (`fping -p 50`), which correlated-drops on a lossy link: measured **~85% loss** on a flaky
  2.4 GHz Wi-Fi link where SmokePing (spread-out pings) and fping's own default spacing both see
  ~17–20%. The pings are now **spread across the round budget** (SmokePing-style) with an explicit
  per-reply timeout (`fping -t`), both sized to fit N pings in the target's `step`. `period_ms` /
  `timeout_ms` remain as optional overrides. A good link's loss/latency is unchanged; a round now takes
  the spread time (e.g. ~10 s for 20 pings at a 60 s step) instead of ~1 s. Builds on the per-ping
  round budget below.
- **HTTP/DNS/TCP/SSH probes no longer report a slow endpoint as packet loss.** These probes measure
  their N pings sequentially, but the scheduler applied a single flat `-timeout` (default 4s) to the
  whole round, so an endpoint that responds slowly had its later pings guillotined by the shared
  deadline and counted as loss. This showed up starkly against **www.cloudflare.com**, which
  Cloudflare's bot management tarpits to 200–1000 ms for any Go HTTP client (regardless of
  User-Agent — it's a TLS/client fingerprint, not the UA): the probe reported ~65% "loss" on a target
  that is 30 ms / 0% loss for `curl`. `-timeout` is now a **per-ping** budget — a target's round
  budget is `timeout × pings`, capped by its `step` — so N sequential slow-but-responding pings all
  fit and the endpoint reports honest latency instead of false loss. `pings=1` targets are unaffected.
- **Spurious "bulk series truncated" warning under many targets.** `pgstore.SeriesAll` logged
  `bulk series truncated; oldest rounds omitted` (and reported `truncated=true`) on **every** Graphs-grid
  refresh once there were ≥16 targets — because the fair-share per-target cap (`global_budget/targets`)
  dropped below the 20k ceiling, which was mistaken for actual truncation even when no target exceeded
  its cap and nothing was dropped. Truncation is now reported only when a target's rounds were actually
  clipped. Regression-tested (`TestPGStoreSeriesAllNoFalseTruncation`).
- **Rootless Podman no longer fails to start the collector (`ping_group_range` sysctl).** The Compose
  stack set `net.ipv4.ping_group_range: "0 2147483647"` to enable the native `Ping` probe's
  unprivileged datagram socket. Under **rootless** Podman the container runs in a user namespace that
  maps only the ~65k GIDs from `/etc/subgid`, so the out-of-range upper bound was rejected at container
  start with `write /proc/sys/net/ipv4/ping_group_range: invalid argument` (Docker rootful was
  unaffected). The range is now scoped to the collector's GID (`"0 10001"`, matching the Dockerfile's
  `smoked` user), which is valid inside a rootless userns and still enables the datagram socket.
  Verified rootless: the container starts, and both `FPing` (via `CAP_NET_RAW`) and native `Ping` (via
  the datagram socket, zero caps) return RTT as the non-root uid 10001.
- **The MIT `LICENSE` is now included in the collector image (`/LICENSE`).** The source is MIT-licensed
  but the distributed image carried no license notice; the Dockerfile now copies it in.
- **Minting the reserved vantage name `local` now returns a clear 409, not a generic 503.** It passed
  the name-shape check (it is a valid name) and the store's reserved-name rejection was funneled into
  `store unavailable`, misrepresenting an operator mistake as an outage; the Vantages UI compounded it
  with an "allowed-but-hinted" confirm that led straight into the error. The API maps the reserved and
  invalid-name cases to 409/400, and the UI blocks `local` up front with the reason.
- **`/api/series/all` now bounds the total rounds it serializes across all targets.** Rows were capped
  per target (20k) but not globally, so a bulk request over many targets could materialize a very
  large response (an authenticated memory/DoS path). The response is trimmed to a global budget —
  keeping each target's newest rounds so every target stays represented — and carries `rounds` +
  `truncated` so a client can narrow the window or use `/api/rollup`.
- **Downsampling backfills the daily aggregate when it is missing even if hourly exists.** The backfill
  decision keyed off only `samples_hourly`, so a partial schema (hourly present, `samples_daily`
  dropped/absent) recreated the daily view empty and skipped the one-time backfill, leaving daily
  history incomplete until trailing refreshes caught up. It now keys off both aggregates.
- **Agent no longer discards buffered rounds on a malformed hub success response.** `PushResults`
  accepted any 2xx and ignored the response-decode error, so an empty `200`, a `204`, HTML from a
  proxy/maintenance page, or truncated JSON made the flush loop commit and reclaim the batch even
  though the hub never acknowledged storing it — silent, irreversible measurement loss. It now
  requires `200` with a single well-formed JSON object whose `accepted + dropped` accounts for the
  batch, and treats any malformed/inconsistent success as a transient error so the batch stays
  buffered for retry.
- **A local probe crossing a config reload is no longer stored under the redefined target.** The
  completion path wrote the sample to the store before the fingerprint check (which lived in alert
  evaluation), so a round measured under definition A that finished after a SIGHUP/API reload
  redefined the target to B was persisted and could surface as B's latest value — the remote ingest
  path already gated storage on the fingerprint. Local storage and alert evaluation now run under one
  runtime snapshot held against the reload swap, dropping an obsolete-identity round from both.
- **Checksum corruption in the active agent spool segment now fails startup instead of being
  silently truncated.** Recovery treated any incomplete decode of the active segment as a crash-torn
  tail and truncated to the last good frame — so a CRC mismatch (storage corruption, partial media
  failure, accidental edit) silently dropped the bad frame and every valid frame after it, with no
  operator signal. Recovery now distinguishes a genuinely short/torn final frame (truncated and
  recovered, the expected crash artifact) from a checksum mismatch (`errBadFrame`), which fails
  loudly in any segment — closing the gap that made the frame CRC pointless for the active segment.
- **Agent spool recovery rejects a corrupt/torn frame length instead of allocating up to ~4 GiB.**
  `streamSegment` read a `uint32` payload length from the segment file and only checked that it
  wasn't absurdly small (`< 8`) before `make([]byte, frameHeader+payloadLen)` — a torn write or bit
  of corruption near the top of the u32 range could demand up to ~4 GiB, OOM-crash-looping the
  agent on `openSpool` and blocking recovery of every valid buffered round in that segment. A valid
  frame can never exceed one segment (`segmentMaxBytes`, 64 MiB), so a length above that is now
  rejected as `errBadFrame` before any allocation — mirrored in `decodeFrame` for defense in depth.
  Regression-tested (`TestStreamSegmentRejectsOversizedFrameLength`).
- **SmokePing importer now inherits probe target-variables down the target tree.** Previously only
  the `probe` was inherited from ancestor `+`/`++` folders; a probe's target-variables (`lookup`,
  `port`, `recordtype`, `pings`, …) were read only from a target's own inline fields and the Probes
  file. The standard SmokePing idiom — e.g. `lookup=` set once on a `+ DNS` folder, or `pings=` on a
  subtree — therefore imported children with those settings **silently missing** (a DNS target that
  couldn't resolve; a wrong `pings` denominator skewing `--history` loss). The importer now
  accumulates inheritable fields down the tree (nearest-set-wins), matching SmokePing's grammar,
  while per-node keys (`host`/`menu`/`title`/`remark`) still never inherit.
- **Agent ingest bounds a round's `pings` by the assigned monitor, not the global ceiling.** The hub
  validated a submitted round's self-reported `pings` only against the global `MaxPings` (10000), then
  allocated a `pings`-length sample array per round — so an authenticated vantage could submit a full
  5000-round batch of `pings=10000, rtts=[]` (a ~340 KB body) and force ~400 MB of allocation, a
  memory-amplification vector. Ingest now drops any round whose `pings` exceeds the target's assigned
  `pings`, which a legitimate agent never exceeds.
- **RRD history import no longer truncates archives longer than ~360 days.** The coarsest stitching
  tier stopped at a hard-coded 360-day look-back and was only clamped *up* to the RRD's true oldest
  data, never *down* — so an install whose coarsest AVERAGE RRA retains more than ~360 days had
  everything older silently dropped by `smoked import … --history`, despite the "full consolidated
  history" promise. The coarsest tier now reaches down to the RRD's oldest AVERAGE data; standard
  installs (coarsest RRA = exactly ~360 days) are unaffected.
- **Documentation corrected to the stable 1.0.** The README dropped the "working codename" / "MVP
  scaffold" framing and the claim that DB-sourced config was "not built yet" (it ships in 1.0);
  clarified that the `127.0.0.1:8087` bind is loopback-only (not LAN-reachable); listed the shipped
  packages/commands (federation, agent, importer, configstore, vantage, `smoke-agent`) in the layout;
  and removed local-only planning-path references. The bundled reverse proxy's docs no longer claim
  rate limiting it does not configure.

## [1.0.0] - 2026-08-11

First stable release — a modern, non-Perl reimplementation of SmokePing in Go on TimescaleDB:
a fast, highly parallel poller with pluggable probes (FPing, native ICMP `Ping`, TCPConnect,
DNS, HTTP, SSH, IRTT), client-side canvas smoke graphs, a hysteresis alert engine, multi-vantage
**federation** (a hub that assigns work to `smoke-agent` remote collectors, with a crash-safe
on-disk store-and-forward spool), config-in-a-database with an in-browser target manager, a
SmokePing → TimescaleDB migrator (config + RRD history), and Prometheus `/metrics` + SLA
reporting. Everything built to date ships in this single 1.0 line — the earlier plan to tag the
single-node core **v1.0** and federation **v2.0** separately was collapsed, since nothing had
been released yet. Full detail below.

### Added
- **`-require-fingerprint` strict ingest mode.** Off by default (a pre-fingerprint agent's rounds
  are still accepted so a rolling upgrade doesn't drop data). When enabled, the hub drops any agent
  round that carries no measurement fingerprint as a visible permanent drop, closing the residual
  misattribution path for not-yet-upgraded agents. An operator flips it on once
  `heliograph_agent_missing_fingerprint_total` shows every vantage's agent upgraded.
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
  imported. History from before heliograph's own raw-sample retention window still renders
  on the dashboard, just from the aggregate (its smoke band collapses to the median line — there's
  no per-round distribution in an RRD's consolidated data to draw a band from) — which is why
  `--history` requires the continuous aggregates already enabled (`smoked -downsample`) and refuses
  to import at all otherwise (see Fixed below). A dry-run `--report` mode prints the same
  target/matched/config-only/orphan counts without touching the DB, and works with or without
  downsampling enabled, for previewing config-vs-RRD drift before running `--history`. This
  completes the SmokePing importer (slice B on top of slice A's config-only import below).
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
  deadline. Delivery counters (`heliograph_webhook_queued_total`, `_delivered_total`,
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
- **Ingest no longer treats an unconfirmed PostgreSQL batch as durable.** All three batch write
  paths (`Add`, `AddResults`, `importBatch`) checked each row's `Exec` but discarded the batch's
  finalization (`Close`) error. pgx runs `SendBatch` in an implicit transaction and reports a
  commit/finalization failure from `Close`, *after* the per-row command tags — so a batch that
  didn't actually commit could be reported as newly-inserted, and the hub would then evaluate
  alerts and reply success over unpersisted rounds. A shared `drainBatch` helper now checks the
  finalization error; on any error the ingest handler answers 503 and the agent retries (idempotent
  via `ON CONFLICT`), preserving store-before-alert.
- **The agent's byte estimator no longer undercounts JSON-escaped strings.** It counted the raw
  lengths of the target/error/fingerprint fields, but `encoding/json` expands `&`, `<`, `>`, and
  control bytes to 6-byte `\u00xx` sequences — so an adversarial (e.g. all-`&`) target could let a
  batch estimated under the cap marshal to ~6× larger, recreating the pre-413 OOM the byte bound
  was meant to prevent. String fields are now counted at worst-case escaped width and each flush
  batch is packed under a budget with headroom below `agentwire.MaxResultsBytes`.
- **The in-memory store's replay-dedup index is now bounded.** `MemStore` caps history per
  `(vantage,target)` but had added every agent-result key to its replay index permanently, so a
  long-lived in-memory `ResultIngester` grew with the total number of rounds ever seen. The key is
  now evicted when its round ages out of the capped history, so the index tracks the retained data.
- **The agent buffers and flushes by bytes, not just round count, so a constrained vantage can't
  OOM.** A round may carry up to `MaxPings` (10 000) RTTs, so bounding only by round count let a
  count-selected flush batch (default `flush_max` 5 000) marshal a multi-hundred-MB request body
  the hub would only 413 *after* the agent had already built it — OOM-killing a low-memory vantage
  before the recursive 413 split could run — and let a prolonged outage grow the buffer without a
  practical memory bound. Both the store-and-forward buffer and each flush batch are now bounded by
  estimated serialized bytes (batch under `agentwire.MaxResultsBytes`; buffer under a 256 MiB
  default) as well as round count; the recursive 413 split remains as a fallback for estimation
  variance.
- **Replayed agent rounds are no longer double-counted for alerting.** The store insert is
  idempotent (`ON CONFLICT (target,vantage,ts) DO NOTHING`), but the alert side effect was not: the
  hub evaluated alerts over the entire submitted batch, including rows that conflicted and were not
  inserted. A replayed round — an HTTP retry, or the agent's deliberate resend when a split batch's
  later half fails transiently — therefore re-advanced the alert window, so one lost round replayed
  twice could satisfy an `X=2` consecutive-loss matcher and emit a false FIRING (or a duplicate
  notification). `AddResults` now returns the subset of rounds that were *newly* persisted (Postgres
  via each insert's `RowsAffected`; the in-memory store via a `(vantage,target,ts)` dedup set), and
  the hub evaluates alerts only over that subset.
- **Reload alert-state reconciliation now honors alert attachment and delivery routing.** Beyond the
  target/matcher identity checks added earlier, a reload no longer inherits a `(target, alert)`
  firing state for an alert that is not attached to that target in the new config (so detaching an
  alert and later re-attaching it can't resurrect its old firing state), and it resets the
  delivered/`visible` bit when an alert's `To` recipients or a target's `alertee` change — so a
  newly added recipient gets a coherent FIRING/RESOLVED lifecycle instead of inheriting a stale
  "already delivered" flag. Matcher firing state still survives a recipient-only edit.
- **`smoked import … --history` counts RRD extraction failures.** A corrupt or unreadable RRD was
  logged and skipped without counting toward the run's partial-failure total, so a migration that
  imported no history for some targets could still report `0 failed` and exit `0`. Extraction
  failures now count and the run exits with the partial-failure status.
- **The `--history` aggregate preflight now verifies both continuous aggregates.** It checked only
  `samples_hourly`; a schema with the hourly view present but `samples_daily` absent (an interrupted
  downsampling init or a manually dropped view) passed the fail-before-write check, inserted raw
  rows, then failed mid-import on the daily refresh. It now requires both views before writing.
- **Alert reload state is now reconciled by identity, not names.** A live config reload
  (`SIGHUP` or the admin config API) inherited each target's sample window and firing/visible state
  by target *name* + alert *name*, so it could carry stale state across an incompatible redefinition
  and miss seeding a newly-attached alert. Four cases are fixed: reusing a target name for a target
  whose measurement identity changed (host, probe, params, pings, or probe-level config — the same
  `federation.Fingerprint` the ingest path keys on) no longer inherits the old window or firing state
  (it's seeded fresh from history for the new identity); changing a matcher while keeping the alert
  name no longer inherits hysteresis for the old semantics (matchers now carry a `Key()` identity,
  and firing/visible state is inherited only when the key is unchanged); attaching an alert to a
  previously-unalerted target now seeds its window from durable history during the swap (via the
  existing warm-start), so an already-breaching target fires immediately instead of after X fresh
  rounds; and a round (local or ingested) that finishes measuring *during* the swap of a redefined
  target is dropped rather than evaluated against the new identity (jobs and ingested rounds carry
  the fingerprint, and `eval` skips a mismatch). The common unchanged-config reload behaves exactly
  as before — hysteresis preserved, no re-fire. (CODE_REVIEW #4.)
- **Fingerprinted agent results can no longer be attributed to a redefined target.** An agent's
  store-and-forward buffer keyed rounds only by target *name*, and the hub, on ingest, looked the
  name up in the *current* assignment and stamped that target's current host/probe onto the old
  round — so a round measured under one identity and replayed (up to 30 days later) after the
  operator changed the target's host/probe/params/pings/probe-config while keeping its name was
  silently stored **and alerted** as a measurement of the new target. The hub now computes a stable
  per-target *measurement-identity* fingerprint (`federation.Fingerprint` — a sha256 over
  probe/host/params/pings and the effective `probes.<Kind>` config, using the same canonical
  encoding as `ConfigVersion`), stamps it on each assignment target, and the agent echoes it back
  opaquely on every round (carried through `scheduler.Job`→`Outcome`→`RoundReport`, so it survives a
  replay). On ingest the hub recomputes the fingerprint from the target's *current* config and drops
  any round whose fingerprint differs — counted in the response `dropped` and logged with a
  `fingerprint_mismatch` breakdown — so a stale round carrying a fingerprint can never be
  misattributed. **Compatibility:** a round with *no* fingerprint (from a not-yet-upgraded,
  pre-fingerprint agent) is still accepted so a rolling upgrade doesn't drop data — meaning the
  original misattribution is still possible for such an agent until it is upgraded. Those rounds are
  counted per vantage on `/metrics` (`heliograph_agent_missing_fingerprint_total`) and warned once per
  vantage, so an operator can watch a rollout finish; a later release may require a fingerprint.
- **`EnableDownsampling`'s one-time backfill could fail on ordinary refresh-policy contention.**
  `backfillAggregates` ran its two `CALL refresh_continuous_aggregate(...)` statements via a raw
  `pool.Exec`, with no retry — unlike `RefreshAggregates`, which already wraps the same kind of
  `CALL` in `execWithRetry` because a background continuous-aggregate refresh policy job can hold
  the same aggregate's refresh lock at the moment the CALL runs, and TimescaleDB aborts the loser
  with SQLSTATE `55P03` (`lock_not_available`) instead of queuing it. Un-retried, that ordinary
  contention was an intermittent `TestDailyRollup`/`TestRollupMedianRoundsExcludesLostRounds`
  flake — and, because `EnableDownsampling` runs at cold start, could make the daemon fail to
  boot outright. `backfillAggregates` now routes both `CALL`s through the same `execWithRetry`
  helper.
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
- **History import could silently lose old history to retention before it was ever aggregated.**
  When the hourly/daily continuous aggregates weren't enabled yet (production init doesn't create
  them; only `smoked -downsample` does), `--history` used to print a warning and import the raw
  rows anyway, then skip the aggregate refresh entirely. If the operator enabled downsampling
  later, `EnableDownsampling`'s one-time backfill only reaches back 30 days (matching the raw
  retention policy it installs at the same time) — so any imported history older than that window
  was never materialized into `samples_daily`, and the raw rows behind it were eventually deleted
  by the retention policy, silently losing the "full consolidated history" the importer promises.
  `runHistory` now checks for the continuous aggregates *before* extracting or inserting anything
  and refuses to import at all when they're absent — a clear, actionable error names
  `smoked -downsample` and confirms no rows were written — forcing the correct order (enable
  downsampling, then import). Separately, `pgstore.RefreshAggregates` itself could leave the daily
  bucket containing the newest imported sample completely unmaterialized: TimescaleDB's
  `refresh_continuous_aggregate` can return zero rows for a bucket when the refresh window's upper
  bound lands exactly at (or only seconds past) that bucket's newest raw timestamp, rather than
  materializing it. `RefreshAggregates` now widens both bounds out to whole UTC days before
  refreshing, guaranteeing the daily aggregate's bucket at each end is unambiguously covered.
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
  against that base config before persisting; without it, an invalid fragment is instead validated
  when the config is next built — on a running daemon, that's a SIGHUP reload, which rejects it and
  logs why; on a cold start, `buildRuntime` failing on it makes the daemon **fail to boot**. Prefer
  `-config`/`--config` to catch a bad fragment up front instead of at the next build.
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
- **Agent on-disk store-and-forward.** The smoke-agent can now persist its
  store-and-forward buffer to disk (opt-in via `spool_dir` / `--spool-dir`). Buffered
  rounds survive an agent restart, including a hard crash (`kill -9`/OOM/power loss),
  losing at most ~1s of the most recent rounds. Implemented as an append-only, CRC-framed,
  size-rolled segment log mirroring the in-memory buffer, with an atomic head watermark and
  crash-safe replay on startup; a shared spool dir is refused via `flock`, and a runtime
  spool I/O error degrades to memory-only rather than stopping the agent. When `spool_dir`
  is unset, behavior is unchanged (in-memory only).
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
- pgstore reads `duration_ms` back, so `heliograph_probe_duration_seconds` is no longer
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
- Operational metrics on `/metrics`: per-probe timing (`heliograph_probe_duration_seconds`)
  and round-level `heliograph_rounds_total`, `_round_duration_seconds`, `_round_targets`,
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
  `heliograph_probe_median_seconds`, `heliograph_probe_loss_ratio`, `heliograph_probe_up`.
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

[Unreleased]: https://github.com/seitzbg/heliograph/compare/v2.0.0...main
[2.0.0]: https://github.com/seitzbg/heliograph/compare/v1.0.16...v2.0.0
[1.0.16]: https://github.com/seitzbg/heliograph/compare/v1.0.15...v1.0.16
[1.0.15]: https://github.com/seitzbg/heliograph/compare/v1.0.14...v1.0.15
[1.0.14]: https://github.com/seitzbg/heliograph/compare/v1.0.13...v1.0.14
[1.0.13]: https://github.com/seitzbg/heliograph/compare/v1.0.12...v1.0.13
[1.0.12]: https://github.com/seitzbg/heliograph/compare/v1.0.11...v1.0.12
[1.0.11]: https://github.com/seitzbg/heliograph/compare/v1.0.10...v1.0.11
[1.0.10]: https://github.com/seitzbg/heliograph/compare/v1.0.9...v1.0.10
[1.0.9]: https://github.com/seitzbg/heliograph/compare/v1.0.8...v1.0.9
[1.0.8]: https://github.com/seitzbg/heliograph/compare/v1.0.7...v1.0.8
[1.0.7]: https://github.com/seitzbg/heliograph/compare/v1.0.6...v1.0.7
[1.0.6]: https://github.com/seitzbg/heliograph/compare/v1.0.5...v1.0.6
[1.0.5]: https://github.com/seitzbg/heliograph/compare/v1.0.4...v1.0.5
[1.0.4]: https://github.com/seitzbg/heliograph/compare/v1.0.3...v1.0.4
[1.0.3]: https://github.com/seitzbg/heliograph/compare/v1.0.2...v1.0.3
[1.0.2]: https://github.com/seitzbg/heliograph/compare/v1.0.0...v1.0.2
[1.0.0]: https://github.com/seitzbg/heliograph/releases/tag/v1.0.0
[0.1.0]: https://github.com/seitzbg/heliograph/releases/tag/v0.1.0
