#!/bin/sh
#
# End-to-end check of the contrib/freebsd rc.d scripts on a real FreeBSD host: installs both
# services the way contrib/freebsd/README.md describes, runs them as the unprivileged `smoke`
# account under daemon(8), and drives start/status/restart/stop through service(8).
#
# It installs binaries and rc.d scripts, adds a user, edits /etc/rc.conf and starts services, so
# run it only as root on a throwaway FreeBSD VM (CI's freebsd-test job does), from the repo root.
set -eu

[ "$(uname -s)" = FreeBSD ] || { echo "freebsd-rc-test: FreeBSD only" >&2; exit 2; }
[ "$(id -u)" = 0 ] || { echo "freebsd-rc-test: run as root" >&2; exit 2; }

fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "ok: $*"; }

# poll SECONDS CMD...: succeed as soon as CMD does; fail once SECONDS have passed.
poll() {
	_left=$1; shift
	while ! "$@" >/dev/null 2>&1; do
		_left=$((_left - 1))
		[ "$_left" -gt 0 ] || return 1
		sleep 1
	done
}

dump() {
	_rc=$?
	if [ "$_rc" -ne 0 ]; then
		echo "---- daemon/smoke processes"
		# shellcheck disable=SC2009 # want the user/ppid columns pgrep cannot print
		ps -axww -o user,pid,ppid,command | grep -E '[d]aemon|[s]moke' || true
		echo "---- /var/log/messages (tail)"
		tail -n 40 /var/log/messages || true
	fi
	exit "$_rc"
}
trap dump EXIT

supervisor() { cat "/var/run/$1/$1.pid"; }   # daemon(8) writes its own pid there (-P)
child() { pgrep -P "$(supervisor "$1")"; }    # the binary daemon(8) supervises
user_of() { ps -o user= -p "$1" | tr -d ' '; }
cmd_of() { ps -ww -o command= -p "$1"; }
api_up() { fetch -qo /dev/null http://127.0.0.1:8087/api/targets; }
new_child() { _k=$(child "$1") && [ "$_k" != "$2" ]; }

# daemon(8) -T sends the child's output to syslog under the service name; checkable only if
# syslogd is running (it is on a stock FreeBSD install).
if pgrep -x syslogd >/dev/null; then
	have_syslog=1
else
	have_syslog=0
	echo "note: syslogd is not running; skipping the syslog-tag checks"
fi
logged() { [ "$have_syslog" = 0 ] || grep -q "$1" /var/log/messages; }

# ---- install, as contrib/freebsd/README.md documents
go build -o /usr/local/bin/smoked ./cmd/smoked
go build -o /usr/local/bin/smoke-agent ./cmd/smoke-agent
install -m 0555 contrib/freebsd/smoked contrib/freebsd/smoke_agent /usr/local/etc/rc.d/
mkdir -p /usr/local/share/heliograph
rm -rf /usr/local/share/heliograph/web
cp -R web /usr/local/share/heliograph/web
id smoke >/dev/null 2>&1 || pw useradd smoke -c Heliograph -d /nonexistent -s /usr/sbin/nologin -w no

# ---- smoked: no DSN, so it serves the built-in demo targets from memory
service smoked onestart
poll 30 api_up || fail "smoked API never came up on 127.0.0.1:8087"
poll 10 service smoked onestatus || fail "service smoked onestatus: not running"
fetch -qo - http://127.0.0.1:8087/api/targets | grep -q 'Cloudflare TCP :443' ||
	fail "/api/targets lacks the demo targets"
sup=$(supervisor smoked)
kid=$(child smoked)
[ "$(user_of "$sup")" = smoke ] || fail "daemon(8) runs as $(user_of "$sup"), want smoke"
[ "$(user_of "$kid")" = smoke ] || fail "smoked runs as $(user_of "$kid"), want smoke"
cmd_of "$kid" | grep -q -- '-serve -addr 127.0.0.1:8087 -webdir /usr/local/share/heliograph/web -downsample' ||
	fail "unexpected smoked argv: $(cmd_of "$kid")"
poll 10 logged 'smoked\[[0-9]*\]: .*serving' || fail "smoked output not in syslog under tag smoked"
ok "smoked onestart: API serves demo targets; daemon(8) and smoked both run as smoke"

kill "$kid"
poll 15 new_child smoked "$kid" || fail "daemon(8) did not restart the killed smoked"
poll 30 api_up || fail "API down after daemon(8) restarted smoked"
ok "smoked: daemon(8) restarted a killed child"

# v2.2.0 documented smoked_flags for smoked's own flags; it must still reach smoked, not daemon(8).
sysrc smoked_flags="-resolve-ips"
out=$(service smoked onerestart 2>&1) || fail "onerestart with smoked_flags failed: $out"
echo "$out" | grep -q 'smoked_flags is passed to daemon(8)' || fail "no smoked_flags warning: $out"
poll 30 api_up || fail "API down after onerestart"
[ "$(supervisor smoked)" != "$sup" ] || fail "onerestart kept the old supervisor pid $sup"
kid=$(child smoked)
cmd_of "$kid" | grep -q -- '-webdir /usr/local/share/heliograph/web -resolve-ips' ||
	fail "smoked_flags not passed to smoked: $(cmd_of "$kid")"
sysrc -x smoked_flags
ok "smoked onerestart: new supervisor; legacy smoked_flags reaches smoked with a warning"

# SMOKED_DSN travels through su -m in the environment, never argv. With a DSN at a closed port
# smoked waits (up to 60s) for the database, which leaves time to read its environment.
sysrc smoked_dsn="postgres://smoke:ci-secret@127.0.0.1:1/smoke?sslmode=disable"
service smoked onerestart
poll 15 child smoked || fail "no smoked child with a DSN set"
kid=$(child smoked)
procstat -e "$kid" | grep -q 'SMOKED_DSN=postgres://smoke:ci-secret@127.0.0.1:1/smoke' ||
	fail "SMOKED_DSN missing from smoked's environment: $(procstat -e "$kid")"
if pgrep -f ci-secret >/dev/null; then   # pgrep never matches itself
	fail "DSN password visible in a process's argv: $(pgrep -lf ci-secret)"
fi
sysrc -x smoked_dsn
ok "smoked: DSN reaches the child's environment and stays out of argv"

service smoked onestop
if service smoked onestatus; then fail "smoked still running after onestop"; fi
if pgrep -x smoked >/dev/null; then fail "a smoked process survived onestop"; fi
[ ! -e /var/run/smoked/smoked.pid ] || fail "daemon(8) left /var/run/smoked/smoked.pid behind"
ok "smoked onestop: supervisor and child gone, pidfile removed"

# ---- smoke_agent: no hub here, so point it at a closed port with a throwaway client cert. It logs
# the failed assignment pulls and keeps running, which is all the service plumbing needs.
etc=/usr/local/etc/heliograph
spool=/var/db/smoke-agent/spool
mkdir -p "$etc"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=ci-vantage \
	-keyout "$etc/client.key" -out "$etc/client.pem" 2>/dev/null
chown smoke "$etc/client.key"
chmod 0600 "$etc/client.key"
printf 'hub: https://127.0.0.1:9\nvantage: ci\n' > "$etc/agent.yaml"
chown smoke "$etc/agent.yaml"
chmod 0640 "$etc/agent.yaml"
sysrc smoke_agent_args="-client-cert $etc/client.pem -client-key $etc/client.key -ca-cert $etc/client.pem"

service smoke_agent onestart
poll 15 test -e "$spool/spool.lock" || fail "smoke-agent never opened its spool at $spool"
poll 10 service smoke_agent onestatus || fail "service smoke_agent onestatus: not running"
sup=$(supervisor smoke_agent)
kid=$(child smoke_agent)
[ "$(user_of "$sup")" = smoke ] || fail "daemon(8) runs as $(user_of "$sup"), want smoke"
[ "$(user_of "$kid")" = smoke ] || fail "smoke-agent runs as $(user_of "$kid"), want smoke"
[ "$(stat -f %Su "$spool/spool.lock")" = smoke ] || fail "spool.lock not written by smoke"
cmd_of "$kid" | grep -q -- "-config $etc/agent.yaml -spool-dir=$spool -client-cert $etc/client.pem" ||
	fail "unexpected smoke-agent argv: $(cmd_of "$kid")"
poll 10 logged 'smoke_agent\[[0-9]*\]: .*smoke-agent starting' ||
	fail "smoke-agent output not in syslog under tag smoke_agent"
ok "smoke_agent onestart: daemon(8) and smoke-agent run as smoke; spool written"

# smoke_agent_spool="" must reach smoke-agent as an explicit empty -spool-dir= (in-memory mode).
sysrc smoke_agent_spool=""
service smoke_agent onerestart
poll 15 child smoke_agent || fail "no smoke-agent child after onerestart"
kid=$(child smoke_agent)
cmd_of "$kid" | grep -q -- "-spool-dir= -client-cert" ||
	fail "smoke_agent_spool=\"\" not passed as -spool-dir=: $(cmd_of "$kid")"
sysrc -x smoke_agent_spool
ok "smoke_agent onerestart: smoke_agent_spool=\"\" passes an empty -spool-dir="

service smoke_agent onestop
if service smoke_agent onestatus; then fail "smoke_agent still running after onestop"; fi
if pgrep -x smoke-agent >/dev/null; then fail "a smoke-agent process survived onestop"; fi
[ ! -e /var/run/smoke_agent/smoke_agent.pid ] || fail "daemon(8) left the smoke_agent pidfile behind"
ok "smoke_agent onestop: supervisor and child gone, pidfile removed"
