# Alerting operator guide — notifications and alert definitions

Heliograph's alerting is SmokePing's model, modernized: named alert
definitions inspect each target's rolling loss/latency history, and firing or
resolving is dispatched to one or more **notifiers** (log, Slack, Discord,
email, generic webhook). Alerts are dark until you attach one to a target and
enable a notifier — with no `alerts:` defined and no notifier flags/env vars
set, `smoked` behaves like a plain collector with no notifications.

This guide covers what [`README.md`](../README.md) and
[`federation.md`](federation.md) don't: how to define an alert, attach it to
the target tree, and configure each notifier channel end to end.

## How alerts attach to targets

Alerts are defined once, at the top level of `config.yaml`, then referenced by
name from any node in the target tree:

```yaml
alerts:
  loss:
    type: matcher
    matcher: CheckLoss(l=50,x=2)
    comment: sustained packet loss
    edgetrigger: true
    to: [log, email]
    priority: 1

targets:
  probe: FPing
  title: Network
  alerts: [loss]        # every descendant inherits this alert...
  children:
    router:
      host: 192.168.1.1
    isp:
      host: 8.8.8.8
      alerts: [loss, latency]   # ...or override/extend it here
```

`alerts:` (a list of alert names) inherits down the tree exactly like `probe`
or `vantages`: set it on a group node and every target under that group picks
it up, unless a descendant sets its own `alerts:` (which replaces, not merges,
the inherited list — set `alerts: []` explicitly to opt a subtree out of an
inherited alert). A target can carry more than one alert (e.g. a loss alert
and a separate latency alert), and each alert can be attached to any number of
targets.

`alertee:` works the same way but is additive on top of an alert's own `to:` —
it adds extra notifier recipients for a specific target/subtree without
having to redefine the alert itself:

```yaml
targets:
  probe: FPing
  alerts: [loss]
  children:
    payment-gateway:
      host: payments.internal
      alertee: [slack]   # this target ALSO pages Slack, on top of the alert's own `to:`
```

## Alert definition fields

```yaml
alerts:
  <name>:
    type: matcher | loss | rtt
    matcher: "CheckLoss(l=50,x=3)"   # type: matcher only
    pattern: ">50%,>50%"             # type: loss/rtt only
    comment: "free-text, included in the notification"
    edgetrigger: true | false        # default false
    to: [log, email]                 # notifier names; defaults to [log] if omitted
    priority: 1                      # 1 = highest; 0 (default) = unset, never inhibited
```

- **`type: matcher`** — a hysteresis check: `CheckLoss(l=<percent>,x=<samples>)`
  raises once `x` consecutive rounds are all at or above `l`% loss, and clears
  once `x` consecutive rounds all drop back under it. `CheckLatency(l=<ms>,x=<samples>)`
  does the same for median RTT (note `l` is **milliseconds** here, unlike
  `CheckLoss`'s percent). `x` must be a whole number, 1–10000; `l` must be
  `> 0`. A round where the whole ping was lost (no RTT median) never by itself
  raises or clears a `CheckLatency` alert — it holds the previous state, since
  a fully-down host is what the loss alert is for.
- **`type: loss`** / **`type: rtt`** — SmokePing's shape-matching pattern DSL,
  a comma-separated sequence matched against the most recent samples,
  right-anchored (newest sample last):
  - `>50%`, `>=50%`, `<10`, `<=10`, `==0%`, `!=200` — a plain comparison against
    one sample using any of `>`, `>=`, `<`, `<=`, `==`, `!=` (loss values are
    percent, rtt values are **milliseconds**).
  - `*` — exactly one arbitrary sample (any value).
  - `*12*` — skip 0 to 12 arbitrary samples (lets the pattern "float" past
    noise between two hard comparisons).
  - `==U` / `!=U` — matches an *unknown* sample (a fully-lost round has no RTT
    median); valid only on `type: rtt`, since loss is always a known
    percentage.
  - Example: `>0%,*12*,>0%` fires if loss was non-zero this round and also at
    some point in the previous 12 rounds — SmokePing's classic "did this flap
    recently" shape.
  - A pattern needs at least one hard comparison token (all-wildcard patterns
    that would match any history are rejected at startup), and its total
    lookback (`*N*` skips included) is capped at 20000 samples.
  - The `S` startup-sentinel token from SmokePing is **not supported** —
    heliograph keeps durable history, so "already bad at boot" is answered
    from real samples instead (see `SeedWindow` in `internal/alert/engine.go`
    if you're curious how that warm-start works).
- **`edgetrigger`** — `true` notifies once on the transition into firing, then
  stays quiet while it continues to fire, and once more on resolution
  (SmokePing's default off-hours-friendly behavior). `false` (default)
  re-notifies every round the alert is active — appropriate for `log`, noisy
  for anything that pages a human.
- **`priority`** — inhibition between alerts *on the same target*. When two
  alerts are attached to one target and both are firing, only the one with the
  numerically lowest non-zero priority is actually delivered; the rest are
  suppressed for that round (SmokePing semantics — the Alertmanager analogue is
  an inhibit rule). `priority: 0` (the default) means "no priority": such an
  alert is never suppressed by another, and never suppresses anything else. Use
  this so a loss alert doesn't also spam a redundant latency alert while a host
  is fully down.
- **`to`** — which notifiers receive this alert's events; see below. Defaults
  to `[log]` if omitted.

A notifier name referenced by `to:` or `alertee:` that doesn't correspond to
an enabled notifier (a typo, or `slack` used without `-slack-webhook`) is
logged once as a warning at startup/reload — check `smoked`'s logs if a
notification you expect never arrives.

## Configuring notifier channels

Every notifier except `log` is off by default and turns on the moment its
required flags/env vars are set — there's no separate enable switch. Each
`SMOKED_*` env var has an equivalent `-flag`; env vars are generally more
convenient for a container deployment (see the example below).

### `log` — always on

Writes one line per firing/resolved event to stdout. Needs no configuration
and can't be disabled; every other notifier layers on top of it. Format:

```
[ALERT FIRING] router / loss — sustained packet loss (loss 100%, rtt --) @ 2026-08-30T14:02:11Z
```

### `slack` — Slack incoming webhook

```
SMOKED_SLACK_WEBHOOK=https://hooks.slack.com/services/T000/B000/XXXX
# or: -slack-webhook <url>
```

Create an [incoming webhook](https://api.slack.com/messaging/webhooks) for the
target channel and use its URL directly. Reference it as `slack` in `to:`/`alertee:`.

### `discord` — Discord webhook

```
SMOKED_DISCORD_WEBHOOK=https://discord.com/api/webhooks/XXXX/YYYY
# or: -discord-webhook <url>
```

Create a webhook from the target channel's Integrations settings. Reference it
as `discord` in `to:`/`alertee:`.

### `email` — SMTP

```
SMOKED_SMTP_ADDR=smtp.example.com:587     # host:port, required
SMOKED_SMTP_FROM=alerts@example.com       # required
SMOKED_SMTP_TO=you@example.com,oncall@example.com   # comma-separated, required
SMOKED_SMTP_USER=...                      # optional: enables authenticated submission
SMOKED_SMTP_PASS=...                      # optional: password paired with SMOKED_SMTP_USER
SMOKED_SMTP_INSECURE=1                    # optional: skip STARTTLS cert verification
```

(`-smtp-addr`, `-smtp-from`, `-smtp-to`, `-smtp-user`, `-smtp-pass`,
`-smtp-insecure` are the flag equivalents.) `smoked` fatals at startup if any
of addr/from/to is set without the other two — all three are required
together to enable the notifier. STARTTLS is negotiated automatically when the
server advertises it; plain auth (`AUTH PLAIN`) is used when a user/pass is
configured, and `smoked` refuses to start a send if AUTH is configured but the
server doesn't actually advertise it (rather than silently falling back to an
unauthenticated session — usually means STARTTLS didn't negotiate).

`SMOKED_SMTP_INSECURE=1` skips certificate verification during STARTTLS. It's
meant for an internal relay with a self-signed certificate (e.g. a local
Postfix/Exim relay with no real TLS cert) — don't set it against a
public/untrusted SMTP endpoint. Example for routing through an in-cluster
unauthenticated relay:

```yaml
env:
  - name: SMOKED_SMTP_ADDR
    value: "postfix.default.svc.cluster.local:25"
  - name: SMOKED_SMTP_FROM
    value: "alerts@example.com"
  - name: SMOKED_SMTP_TO
    value: "you@example.com"
  - name: SMOKED_SMTP_INSECURE
    value: "1"
```

Reference it as `email` in `to:`/`alertee:`.

### `webhook` — generic JSON POST

```
SMOKED_WEBHOOK_URL=https://example.com/hooks/heliograph
# or: -webhook <url>
```

POSTs a JSON body per event to any endpoint you control:

```json
{
  "target": "router", "vantage": "local", "alert": "loss",
  "comment": "sustained packet loss", "firing": true, "status": "firing",
  "loss_pct": 100, "rtt_ms": null, "when": "2026-08-30T14:02:11Z"
}
```

`rtt_ms` is `null` for a fully-lost round rather than an invalid number, and
`vantage` is omitted entirely when the event has no vantage label (rather than
sent as an empty string). Every request carries an `X-Idempotency-Key` header
that stays constant across the automatic retries of a single delivery, so a
receiver can dedupe a redelivered event. Each round's notification is a distinct
event with its own key, though — the key includes the round timestamp — so the
header does **not** coalesce the repeated notifications a level-triggered
(`edgetrigger: false`) alert emits every round; use `edgetrigger: true` if you
want one notification per firing/resolved transition.
Reference it as `webhook` in `to:`/`alertee:`.

## Delivery behavior (all non-log notifiers)

`slack`, `discord`, `email`, and `webhook` all share the same delivery
machinery: a bounded async queue (so a slow or unreachable endpoint never
blocks the probe/alert-eval loop), retry with exponential backoff (4 attempts
by default), and a permanent-failure fast path (an SMTP 5xx or an HTTP 4xx
gives up immediately rather than burning the full retry budget on something a
retry can't fix). On shutdown, `smoked` drains the queue for up to 5 seconds
before abandoning what's left.

Their counters are exposed on `/metrics` (Prometheus text format) so delivery
health is observable, not just logged:

```
heliograph_email_queued_total
heliograph_email_delivered_total
heliograph_email_retried_total
heliograph_email_dropped_total     # queue was full; event was discarded
heliograph_email_failed_total      # gave up after retries or a permanent failure
heliograph_email_queue_depth       # gauge

heliograph_webhook_queued_total{notifier="webhook|slack|discord"}
heliograph_webhook_delivered_total{notifier="..."}
heliograph_webhook_retried_total{notifier="..."}
heliograph_webhook_dropped_total{notifier="..."}
heliograph_webhook_failed_total{notifier="..."}
heliograph_webhook_queue_depth{notifier="..."}
```

`slack` and `discord` share the `heliograph_webhook_*` family with `webhook`
itself, distinguished by the `notifier` label; `email` has its own family.

## Testing a notifier end to end

The most reliable way to confirm a notifier is actually wired up correctly is
to make an alert fire on purpose, rather than waiting for a real outage:

1. Add a temporary target that always fails — e.g. an unroutable host or a
   TCP probe against a closed port — with the alert you want to test
   attached and a short `x` (so it fires within a couple of rounds instead of
   minutes).
2. Watch `/metrics` for that notifier's `_queued_total` / `_delivered_total`
   counters incrementing, and check the destination (inbox, channel, webhook
   receiver) for the notification.
3. Remove the temporary target once confirmed — its `RESOLVED` event fires
   automatically as soon as it's deleted (or immediately if you'd rather fix
   the target so it starts passing, then remove it once you've seen both the
   FIRING and RESOLVED messages).

This is also the fastest way to sanity-check `edgetrigger`/`priority`
behavior on a new alert definition before trusting it against a subtree of
real targets.
