# Running Heliograph on FreeBSD

Both binaries are pure Go and run natively on FreeBSD (`amd64`/`arm64`). This directory
ships `rc.d` service scripts so `smoke-agent` (a vantage) and `smoked` (the hub) run under
FreeBSD's service manager, supervised by [`daemon(8)`](https://man.freebsd.org/cgi/man.cgi?daemon)
(auto-restart on crash, output to syslog).

| Script | Runs | Use when |
| --- | --- | --- |
| [`smoke_agent`](smoke_agent) | `smoke-agent` | This host is a **vantage** pushing to an existing hub (the common case). |
| [`smoked`](smoked) | `smoked` | You run the **hub** (dashboard + API) natively on FreeBSD. Needs PostgreSQL/TimescaleDB. |

## Get the binaries

Download the FreeBSD release tarball from
[Releases](https://github.com/seitzbg/heliograph/releases)
(`heliograph_<version>_freebsd_<arch>.tar.gz`), verify it against `SHA256SUMS`, and unpack —
or build from source (`pkg install go && go build ./cmd/smoke-agent ./cmd/smoked`).

```sh
fetch https://github.com/seitzbg/heliograph/releases/download/vX.Y.Z/heliograph_X.Y.Z_freebsd_amd64.tar.gz
tar xzf heliograph_X.Y.Z_freebsd_amd64.tar.gz
install -m 0755 smoked smoke-agent /usr/local/bin/
pw useradd smoke -c "Heliograph" -d /nonexistent -s /usr/sbin/nologin -w no   # service account
```

ICMP: the native `Ping` probe needs a raw socket (run as root with `mode: privileged`); the
`FPing` probe works unprivileged — `pkg install fping` installs a setuid-root `fping(8)`. Other
probes (`DNS`, `HTTP`, `TCPConnect`, `NTP`, …) need no special privilege.

## Vantage agent (`smoke_agent`)

1. Onboard the vantage on the hub (dashboard **Vantages → Add vantage**, or
   `smoked vantage add <name>`) to get `<name>-vantage.tar.gz`. Copy its `agent.yaml`
   (hub URL, vantage name, and the mTLS client cert/key/CA) to the vantage host:

   ```sh
   mkdir -p /usr/local/etc/heliograph
   tar xzf <name>-vantage.tar.gz agent.yaml
   install -m 0640 -o smoke agent.yaml /usr/local/etc/heliograph/agent.yaml
   ```

2. Install and enable the service:

   ```sh
   install -m 0555 smoke_agent /usr/local/etc/rc.d/smoke_agent
   sysrc smoke_agent_enable="YES"
   sysrc smoke_agent_config="/usr/local/etc/heliograph/agent.yaml"
   service smoke_agent start
   ```

Knobs (set with `sysrc`): `smoke_agent_bin`, `smoke_agent_user` (default `smoke`),
`smoke_agent_spool` (default `/var/db/smoke-agent/spool`; `""` disables the on-disk spool),
`smoke_agent_flags` (extra flags). Logs go to syslog (`daemon` facility).

## Hub (`smoked`)

Requires a reachable PostgreSQL/TimescaleDB DSN. Copy the release tarball's `web/` directory
so the dashboard can be served:

```sh
mkdir -p /usr/local/share/heliograph
cp -R web /usr/local/share/heliograph/web

install -m 0555 smoked /usr/local/etc/rc.d/smoked
sysrc smoked_enable="YES"
sysrc smoked_dsn="postgres://smoke:smoke@db.example:5432/smoke?sslmode=disable"
service smoked start
```

The dashboard/API is **unauthenticated** — keep `smoked_addr` on loopback
(`sysrc smoked_addr="127.0.0.1:8087"`) or front it with a TLS + Basic-Auth reverse proxy.
Knobs: `smoked_bin`, `smoked_user`, `smoked_addr`, `smoked_webdir`
(default `/usr/local/share/heliograph/web`), `smoked_config`, `smoked_flags`
(default `-downsample`; add `-agent-addr :8443 -agent-hostname <host>` to accept remote vantages).
