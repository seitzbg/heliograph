# Running Heliograph on FreeBSD

Both binaries are pure Go and run natively on FreeBSD (`amd64`/`arm64`). This directory
ships `rc.d` service scripts so `smoke-agent` (a vantage) and `smoked` (the hub) run under
FreeBSD's service manager, supervised by [`daemon(8)`](https://man.freebsd.org/cgi/man.cgi?daemon)
(auto-restart on crash, output to syslog) as an unprivileged service account.

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

## How the scripts run the services

rc.subr starts each service as `${name}_user` (default `smoke`) through `su(1)`, the same way
ports such as `sysutils/node_exporter` do. Both `daemon(8)` and the binary it supervises therefore
run as that account; nothing stays running as root. Consequences:

- Everything the service reads (`agent.yaml`, a `smoked_config` file, the web directory) must be
  readable by that account.
- The pidfile is `/var/run/<name>/<name>.pid`, in a directory the service account owns; the script
  creates it on start. It holds the `daemon(8)` supervisor's pid, so `service <name> stop` stops the
  supervisor, which stops the binary without restarting it. `ps` shows the supervisor as
  `daemon: <name>[<child pid>]`.
- Output goes to syslog tagged with the service name (`smoked` / `smoke_agent`), facility `daemon`;
  on a stock install it lands in `/var/log/messages`.
- Extra command-line flags go in `smoked_args` / `smoke_agent_args`. Do **not** use
  `smoked_flags` / `smoke_agent_flags`: rc.subr passes `${name}_flags` to `daemon(8)`, not to the
  binary. (v2.2.0 documented the `_flags` names. A value left there is still used as the `_args`
  value, with a warning, until you rename it.)

ICMP: the native `Ping` probe needs a raw socket (run as root with `mode: privileged`); the
`FPing` probe works unprivileged — `pkg install fping` installs a setuid-root `fping(8)`. Other
probes (`DNS`, `HTTP`, `TCPConnect`, `NTP`, …) need no special privilege. **To use the native
`Ping` probe under these services, also set `smoke_agent_user="root"` (or `smoked_user="root"`)** —
`mode: privileged` alone does not grant raw-socket access to the default `smoke` account. The
`FPing` probe needs no such override.

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
   service smoke_agent status
   ```

Knobs (set with `sysrc`): `smoke_agent_bin`, `smoke_agent_user` (default `smoke`),
`smoke_agent_spool` (default `/var/db/smoke-agent/spool`, created on start and owned by the service
account; `""` disables the on-disk spool, even if `agent.yaml` sets `spool_dir`),
`smoke_agent_args` (extra `smoke-agent` flags).

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
service smoked status
```

The dashboard/API is **unauthenticated**, so `smoked_addr` defaults to loopback
(`127.0.0.1:8087`). To reach it beyond localhost, set `smoked_addr` (e.g. `sysrc
smoked_addr=":8087"`) and front it with a TLS + Basic-Auth reverse proxy — never expose it
directly. The DSN is passed via the environment (`SMOKED_DSN`), so the password stays out of `ps`;
if the database is on another host, use an encrypted DSN (`sslmode=require` or `verify-full`, with
the appropriate root/verify cert) rather than the `sslmode=disable` shown above. Without
`smoked_dsn`, `smoked` keeps the built-in demo targets in memory.

Knobs: `smoked_bin`, `smoked_user` (default `smoke`), `smoked_addr`, `smoked_webdir`
(default `/usr/local/share/heliograph/web`), `smoked_config`, `smoked_args`
(default `-downsample`; `""` for none; add `-agent-addr :8443 -agent-hostname <host>` to accept
remote vantages).
