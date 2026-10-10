# Caddy with DNS-challenge provider plugins compiled in.
#
# The stock `caddy` image ships NO DNS provider modules, so DNS-01 (needed for
# certificates without an inbound port 80, behind NAT, or for wildcards) requires
# a custom build. This bakes in a set of common providers; the operator picks one
# at runtime via CADDY_ACME_DNS (see .env.example). To support another provider,
# add a `--with github.com/caddy-dns/<name>@<version>` line below and rebuild.
#
# The base images are digest-pinned and every DNS plugin is version-pinned so this
# credential-bearing reverse proxy builds reproducibly — the exact binary can't drift between two
# builds of the same source. Bump tags + digests + plugin versions together on a refresh (Renovate
# tracks them — see renovate.json). caddy:2.11-builder / caddy:2.11-alpine. CODE_REVIEW M4/L7.
#
# The builder digest below (refreshed 2026-10) carries Go 1.27.2 and Caddy v2.11.7, clearing the
# stdlib net/http and crypto/tls CVEs (CVE-2026-78667, CVE-2026-78669, CVE-2026-97031) that the prior
# pin's Go 1.26.6 toolchain had. Caddy 2.11.7 itself now requires the patched golang.org/x/text,
# golang.org/x/crypto and google.golang.org/grpc versions that earlier --with floors forced, so those
# floors are gone. Its go.mod still requires golang.org/x/net v0.59.0, so the one --with line below
# forces Go's minimal-version-selection to v0.60.0 (CVE-2026-78669). Re-check that floor whenever
# Caddy releases a version that requires x/net v0.60.0 or later itself, and drop it then.
FROM caddy:2.11-builder@sha256:b25f47453fa02f7e66c0828b4b1343658808b9000950b6825c4a1cb0c988f5f8 AS build
RUN xcaddy build \
	--with github.com/caddy-dns/cloudflare@v0.2.4 \
	--with github.com/caddy-dns/route53@v1.6.2 \
	--with github.com/caddy-dns/digitalocean@v0.0.0-20250606074528-04bde2867106 \
	--with github.com/caddy-dns/duckdns@v0.5.0 \
	--with github.com/caddy-dns/namecheap@v1.0.0 \
	--with github.com/caddy-dns/gandi@v1.1.0 \
	--with golang.org/x/net/http2@v0.60.0

# Same digest as before (caddy:2.11-alpine hasn't been rebuilt upstream since 2026-06-24), so the
# c-ares/curl/libcurl/openssl packages it ships are stale relative to the Alpine 3.23 apk repo. The
# apk step below floors them at the first patched versions published on that same v3.23/main branch
# (CVE-2026-33630, CVE-2026-5773, CVE-2026-6276, and CVE-2026-14456 — an openssl QUIC DoS; the base
# still ships libcrypto3/libssl3 3.5.7-r0, verify with `apk policy libcrypto3`). These are `>=`
# floors, not exact `=` pins: Alpine's repo carries only the newest revision of a package, so an exact
# pin breaks every fresh build as soon as a newer revision is published. Drop this RUN once
# caddy:2.11-alpine (or a later minor) is rebuilt with these fixed already, and update the digest.
FROM caddy:2.11-alpine@sha256:5f5c8640aae01df9654968d946d8f1a56c497f1dd5c5cda4cf95ab7c14d58648
RUN apk update && apk add --no-cache --upgrade \
	'c-ares>=1.34.8-r0' \
	'curl>=8.20.0-r0' \
	'libcurl>=8.20.0-r0' \
	'libcrypto3>=3.5.8-r0' \
	'libssl3>=3.5.8-r0'
COPY --from=build /usr/bin/caddy /usr/bin/caddy
