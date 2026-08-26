# Deployment examples

Two ready-to-run Docker Compose stacks. Both use the prebuilt image
`ghcr.io/seitzbg/heliograph:latest` — no clone or build required; copy a directory,
optionally edit its `.env`, and `docker compose up -d`.

| | [`standalone/`](standalone/) | [`federation/`](federation/) |
|---|---|---|
| **Use it when** | one collector on one host (most people) | you want remote **vantages** measuring the same targets |
| **Services** | TimescaleDB + smoked | TimescaleDB + smoked + Caddy reverse proxy |
| **Exposure** | `127.0.0.1:8087` (loopback) | dashboard: public `https://<domain>/` via Caddy; agent API: smoked's own `https://<domain>:8443` |
| **Auth / TLS** | none — front it yourself if you expose it | dashboard: Let's Encrypt TLS + Basic Auth (via Caddy); agents: mutual-TLS client certificate on smoked's own `:8443` listener (no Caddy, no API key) |
| **Setup** | `.env` optional | `.env` required (DOMAIN, ACME_EMAIL, DASH_PASSWORD_HASH) |

**Start here if you're not sure — most deployments are standalone; it needs none of the
federation/mTLS setup below.** Federation only matters when a second machine needs to probe
your targets from its own network position.

## Standalone

```sh
cd standalone
cp .env.example .env          # optional: DB password, admin GUI
docker compose up -d
# -> http://localhost:8087/
```

The dashboard and read API are **unauthenticated**, so the collector is bound to
`127.0.0.1`. To reach it from elsewhere, put a reverse proxy (TLS + auth) in front —
or use the federation stack, which bundles one.

## Federation

```sh
cd federation
cp .env.example .env          # set DOMAIN + ACME_EMAIL
# dashboard password (bcrypt) — see .env.example for the $-escaping note:
export DASH_PASSWORD_HASH="$(docker run --rm caddy:2.11-alpine caddy hash-password --plaintext 'choose-a-password')"
docker compose up -d
# -> https://<your-domain>/
```

Then onboard a vantage from the **Vantages** admin tab: give it a name and it hands back a
ready-to-run `<name>-vantage.tar.gz` (agent.yaml with an embedded mTLS client certificate/key
+ the hub's CA cert, plus a docker-compose.yml and README) — no key to copy or mint. Unpack it
on the vantage host and `docker compose up -d`. (CLI equivalent: `smoked vantage add <name> -out
<name>-vantage.tar.gz`.) The full walkthrough — declaring `vantages:`, onboarding a vantage,
running an agent, reading the overlay — is in [`docs/federation.md`](../docs/federation.md).

> The example's Caddy uses the **HTTP-01** challenge (needs inbound ports 80 + 443). For
> **DNS-01** (behind NAT, or wildcard certs) you need a Caddy image built with your DNS
> provider's plugin — the repo's [`Caddy.Dockerfile`](../Caddy.Dockerfile) bundles several,
> and the repo's own [`docker-compose.yml`](../docker-compose.yml) wires it up under a
> `federation` profile.

## Measuring your own targets

Both stacks run a built-in **demo** target set by default. To watch your own, mount a YAML
config and point the collector at it — uncomment in either `docker-compose.yml`:

```yaml
    volumes:
      - ./config.yaml:/etc/heliograph/config.yaml:ro
    environment:
      SMOKED_CONFIG: /etc/heliograph/config.yaml
```

See [`config.example.yaml`](../config.example.yaml) for the target-tree format (and, for
federation, the per-target `vantages:` field).
