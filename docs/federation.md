# Federation operator guide — provisioning a vantage

Federation lets the same targets be probed from several network locations
("vantages") and their per-vantage latency/loss overlaid on one graph — the
SmokePing "master + slaves" model, modernized.

- The **hub** (`smoked`) owns the config, stores every round, serves the
  dashboard, and probes as the vantage **`local`**.
- A **vantage** is a remote location. A headless **agent** (`smoke-agent`) there
  pulls a strict, hub-assigned target list, probes it with the same plugins as
  the hub, and pushes results back over HTTPS with a per-vantage API key.
- The hub **assigns** work: agents never run server-sent code, only a
  schema-validated target list. The hub is authoritative for each target's probe
  and host; an agent can't misattribute a result.

Federation is **dark until configured**: with no `vantages:` declared and no
agents connected, `smoked` behaves exactly as a single-node collector.

> Federation requires a database — run `smoked` with `-dsn` (TimescaleDB). The
> vantage key store lives there.

```
   config.yaml (targets w/ vantages:)
          │
  ┌───────┴───────────────┐         Internet
  │      HUB (smoked)      │            │
  │  probes as `local`     │      ┌─────┴─────────┐
  │  assignment builder    │◄─────┤ reverse proxy │  TLS (Let's Encrypt)
  │  vantage/key store     │ HTTP │  /agent/v1/*  │  + Basic Auth on dashboard
  │  :8087 (plain HTTP)    │      └─────┬─────────┘
  └───────▲────────────────┘            │ https + API key
          │ LAN dashboard        ┌───────┴──────────┐
          │ browser              │ smoke-agent (nyc)│  pull → probe → push
                                 └──────────────────┘
```

## Prerequisites

- A **hub** running `smoked -serve -dsn <TimescaleDB DSN>` (see the README).
- A **public HTTPS endpoint** in front of the hub — the bundled Caddy profile or
  your own reverse proxy. See the README's *Federation deployment (reverse
  proxy)* section; `smoked` never terminates TLS itself.
- One or more **remote hosts** to run agents on. Each needs the external probe
  binaries for the probe kinds it will run (e.g. `fping` for ICMP, with
  `CAP_NET_RAW`; `irtt` for the IRTT probe). Native probes (TCP, DNS, HTTP, SSH)
  need nothing extra. For ICMP without an external binary, use `Ping` instead of
  `FPing`: it needs either the `net.ipv4.ping_group_range` sysctl (unprivileged) or
  `CAP_NET_RAW` (raw-socket fallback) on the agent host.

## Step 1 — Declare which targets a vantage probes

In the hub's config, a target (or a subtree) lists the vantages that measure it
with `vantages:`. The list is **inherited down the target tree** and defaults to
`[local]` (the hub only). Listing a vantage other than `local` is what makes the
hub build an assignment for it.

```yaml
targets:
  children:
    cdn:
      # Everything under cdn is probed from the hub AND the nyc vantage:
      vantages: [local, nyc]
      children:
        cloudflare: { probe: HTTP, host: cloudflare.com }
        fastly:     { probe: HTTP, host: fastly.com }
    internal:
      # Only the hub probes these (inherits the default [local]):
      db: { probe: TCPConnect, host: 10.0.0.5, params: { port: "5432" } }
    remote-only:
      # Probed ONLY from nyc — the hub never measures it locally:
      vantages: [nyc]
      children:
        edge: { probe: HTTP, host: edge.internal }
```

Reload the hub after editing config (`SIGHUP`, or restart). The assignment the
agent pulls is versioned; the agent picks up changes within its poll interval.

## Step 2 — Provision a vantage key

Each vantage authenticates with its own API key (`smk_<id>_<secret>`). Only a
salted hash is stored; the plaintext is shown **once**. Provision it either way:

**CLI** (on the hub host — shell access there is already trusted):

```console
$ smoked vantage add nyc -dsn "$SMOKED_DSN"
vantage "nyc" key (shown once — store it now):

smk_1a2b3c4d5e6f_0123...deadbeef

# smoke-agent config for vantage "nyc"
hub: "https://your-hub.example"   # set to your https reverse-proxy endpoint
vantage: "nyc"
key: "smk_1a2b3c4d5e6f_0123...deadbeef"
```

`-dsn` defaults to `$SMOKED_DSN`. List and revoke:

```console
$ smoked vantage ls
NAME                     CREATED              LAST-SEEN
nyc                      2026-08-08 14:02     never

$ smoked vantage revoke nyc     # rotates: the old key stops working immediately
revoked vantage "nyc"
```

**GUI** (the Vantages tab): set `SMOKED_ADMIN_PASSWORD` on the hub to enable the
admin panel. Also set `SMOKED_ADMIN_SESSION_KEY` to 32 random bytes encoded as 64 hex
characters (`openssl rand -hex 32`) if sessions should survive collector restarts; without it,
the hub uses a secure ephemeral signing key and logs sessions out on restart. A login lasts 12
hours by default — set `SMOKED_ADMIN_SESSION_TTL` to any Go duration (`24h`, `168h`, `30m`, …,
minimum `1m`) to change it. Log in, then
**add** (reveals the key once — as a copyable/downloadable
`agent.yaml` and a ready-to-run `docker-compose.yaml`, toggled in the dialog), **list**
(name, created, last-seen, target count), **regenerate** (rotate), and **revoke**.
The panel is reached through the proxy (behind the dashboard's Basic Auth, then
its own admin password) or on `http://localhost:8087/` on the hub.

## Step 3 — Run the agent at the vantage

Build the agent and give it a config file (the `add` output is a ready template —
just replace the `hub:` placeholder with your real HTTPS endpoint):

```console
$ go build ./cmd/smoke-agent
```

```yaml
# /etc/smoke-agent.yaml
hub: "https://smoke.example.com"     # your reverse-proxy endpoint (TLS)
vantage: "nyc"
key: "smk_1a2b3c4d5e6f_0123...deadbeef"
# optional (defaults shown):
# interval: 60s      # how often to pull the assignment
# timeout: 4s        # per-target probe timeout
# workers: 50        # max concurrent probes
# buffer: 100000     # store-and-forward capacity, in rounds
# flush_max: 5000    # max rounds per push
# insecure: false    # skip TLS verification (testing only)
```

```console
$ smoke-agent -config /etc/smoke-agent.yaml
```

Every flag can override the file: `-hub`, `-key`, `-vantage`, `-interval`,
`-timeout`, `-workers`, `-buffer`, `-flush-max`, `-insecure`, `-spool-dir`, plus
`-log-format` (text|json) and `-log-level`. The agent has **no listener** — it only makes
outbound HTTPS calls, so it works behind NAT. On push failure it retains rounds
in a bounded in-memory buffer and retries with backoff; when the buffer is full
it drops the oldest and logs the drop.

### Durable buffering (`spool_dir`)

By default the agent buffers unpushed rounds in memory only; a restart while the hub is
unreachable drops them. Set `spool_dir` (config) or `--spool-dir` (flag) to a writable,
durable directory to persist the buffer:

```yaml
spool_dir: /var/lib/smoke-agent/spool
```

- **Guarantee:** buffered rounds survive any restart, including `kill -9`/OOM/power loss,
  losing at most ~1 second of the most recently measured rounds. A few already-delivered
  rounds may be re-sent after a crash; the hub deduplicates on `(target, vantage, ts)`, so
  this is harmless.
- **Bounds:** on-disk data mirrors the in-memory buffer and stays within its ~256 MiB
  budget; dead segments are reclaimed automatically.
- **Exclusivity:** the directory is locked (`flock`); a second agent pointed at the same
  `spool_dir` refuses to start.
- **Failure handling:** if `spool_dir` is set but cannot be created/locked at startup, the
  agent exits with an error. A spool I/O error *after* startup (e.g. a full disk) is logged
  and the agent continues in memory-only mode rather than stopping.

## Step 4 — Verify

- `smoked vantage ls` shows a recent **LAST-SEEN** for the vantage once the agent
  has pulled its assignment.
- In the dashboard, open a target that lists more than one vantage. Its detail
  view overlays a **median line per vantage** (distinct colors) with a chip
  legend that doubles as a focus selector; the smoke band renders for the focused
  vantage. Single-vantage targets and the Graphs grid are unchanged.
- The hub logs `agent endpoints enabled at /agent/v1/assignment, /agent/v1/results`
  at start when `-dsn` is set.

## Security model

- **Per-vantage API key** over the proxy's TLS: salted-hash at rest, constant-time
  verify, individually revocable. A bad/absent/revoked key → `401`.
- **The hub is authoritative** for each target's probe/host; the agent only sends
  raw round-trip times for its assigned targets. Unassigned or malformed results
  are dropped and counted.
- **Dashboard/read API** carry no auth of their own — the reverse proxy gates them
  with **HTTP Basic Auth**; only `/agent/v1/*` is exempt (it uses the API key).
- **Admin key-management** (CLI has none — it's hub-shell-trusted; the API + GUI
  panel require `SMOKED_ADMIN_PASSWORD`, fail-closed when unset). Session cookies are signed by
  the independent `SMOKED_ADMIN_SESSION_KEY`; omitting it uses a process-local key, while rotation
  invalidates every existing admin session. `SMOKED_ADMIN_SESSION_TTL` sets how long a login stays
  valid (default 12h, minimum 1m).
- `local` is reserved: you can't mint a `local` key, and a `local`-authenticated
  agent is rejected.

## Upgrading an existing deployment

The stored samples carry a per-vantage dimension. On the **first start after
upgrading** to a federation-capable build, `smoked` drops and recreates the
`samples_hourly` / `samples_daily` continuous aggregates so they group by vantage
(a TimescaleDB aggregate's shape can't be altered in place). This one-time rebuild
**irreversibly loses every daily rollup bucket older than the 30-day raw
retention window** — the raw rows behind them are already gone, so the long-range
(400-day) view keeps only the last ~30 days plus whatever accrues after. `smoked`
logs a warning when this runs. If that history matters, snapshot the database
before upgrading. New deployments are unaffected.

### Agent fingerprint migration

Each agent round now carries a **measurement fingerprint** so the hub can reject a
buffered result whose target was redefined (host/probe/params/pings/probe-config)
while the round sat in the agent's store-and-forward buffer. A round from an agent
old enough not to send one is still **accepted by default** so a rolling upgrade
never drops data — but until that agent is upgraded, one of its buffered rounds
*can* still be misattributed across a redefinition.

Watch the rollout with the per-vantage counter on `/metrics`:

```
heliograph_agent_missing_fingerprint_total{vantage="nyc"} 0
```

Once it stops rising for every vantage (all agents upgraded), start `smoked` with
`-require-fingerprint` to enforce strictly: a round with no fingerprint is then a
visible permanent drop (still counted by the metric above) rather than accepted.

### Stable target identity

A target's history is stored under a stable, server-managed `id` rather than its
position in the tree, so **moving or renaming** a node keeps its graph. The hub
mints the id (an opaque UUID) for any target created or imported through the admin
UI / `config import`; a target defined only in raw `config.yaml` has no minted id
and falls back to its tree path as identity, so renaming it there starts a fresh
graph. The id is not something you edit and the UI never exposes it. The id travels
in each vantage's assignment (`AssignmentTarget.id`), and a current agent echoes it
on every round so the hub attributes data to the same identity regardless of where
the target now sits.

**Rolling-upgrade behaviour.** A pre-identity agent doesn't understand the new
assignment field and reports a target by its current display path. The hub resolves
that path back to the stable id, so such an agent keeps delivering data across a
move without an upgrade. If an old display path is later **reused** by a different
new target, the hub disambiguates the two by the round's measurement fingerprint —
so a fingerprint-carrying agent is still attributed correctly. The one case that
cannot be resolved is a *pre-fingerprint* agent (old enough to send neither an id
nor a fingerprint) reporting a **reused** path: the token is genuinely ambiguous, so
that round is dropped rather than misattributed, and the new target's data from that
vantage pauses until the agent is upgraded.

The safe rollout is therefore to **upgrade a vantage's agent before moving a target
it measures**, and especially before reusing a freed-up path for a different target.
Watch the same `heliograph_agent_missing_fingerprint_total` counter above: while it
is nonzero for a vantage, that vantage still runs a pre-fingerprint agent for which
a path reuse can pause data.

## Troubleshooting

- **Agent gets `401`** — key wrong, revoked, or rotated (`vantage add`/`regenerate`
  mints a new one; update the agent). Confirm the `Authorization: Bearer smk_…`
  reaches the hub (the proxy must forward `/agent/v1/*` unauthenticated by Basic
  Auth).
- **Vantage never appears / `LAST-SEEN never`** — the agent can't reach the hub
  (DNS/cert/proxy), or no target lists that vantage. Check the agent log and that
  `vantages:` includes the name; reload the hub after config edits.
- **No overlay on a target** — that target's effective `vantages:` has only one
  entry, or the second vantage has no stored rows yet (give the agent a poll
  cycle).
- **Certificate won't issue** — with HTTP-01 the domain's DNS must point at the
  proxy and port 80 must be reachable; behind NAT use DNS-01 (`CADDY_ACME_DNS`,
  see `.env.example`).
