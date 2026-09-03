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
# The builder digest below (refreshed 2026-08) carries Go 1.26.6, clearing the stdlib
# net/http+x/net/idna and x/net/dns/dnsmessage CVEs (CVE-2026-39821, CVE-2026-46600) that the
# prior pin's Go 1.26.5 toolchain had. Caddy 2.11.4's own go.sum still pins vulnerable
# golang.org/x/net, golang.org/x/text, golang.org/x/crypto and google.golang.org/grpc versions
# (they're Caddy's compiled-in deps, not something a base-image bump alone fixes — no 2.12 release
# exists yet to pick up a bump upstream), so four extra --with lines below force Go's
# minimal-version-selection to the patched versions (CVE-2026-46600, CVE-2026-56852,
# GHSA-hrxh-6v49-42gf, and — added 2026-09 after Trivy flagged the built binary — CVE-2026-56854, an
# auth bypass in golang.org/x/crypto/ssh, plus CVE-2026-84304 in google.golang.org/grpc). Re-check
# these floors whenever Caddy releases a version that bundles the fix natively, and drop them then.
FROM caddy:2.11-builder@sha256:c7ae80243a530d532d20062d56d6198b3ab161eb6971d28716ef7ec55599fea4 AS build
RUN xcaddy build \
	--with github.com/caddy-dns/cloudflare@v0.2.4 \
	--with github.com/caddy-dns/route53@v1.6.2 \
	--with github.com/caddy-dns/digitalocean@v0.0.0-20250606074528-04bde2867106 \
	--with github.com/caddy-dns/duckdns@v0.5.0 \
	--with github.com/caddy-dns/namecheap@v1.0.0 \
	--with github.com/caddy-dns/gandi@v1.1.0 \
	--with golang.org/x/net/http2@v0.56.0 \
	--with golang.org/x/text/language@v0.39.0 \
	--with golang.org/x/crypto/ssh@v0.56.0 \
	--with google.golang.org/grpc@v1.83.2

# Same digest as before (caddy:2.11-alpine hasn't been rebuilt upstream since 2026-06-24), so the
# c-ares/curl/libcurl/openssl packages it ships are stale relative to the Alpine 3.23 apk repo. The
# apk step below pins them forward to the patched versions already published on that same v3.23/main
# branch (CVE-2026-33630, CVE-2026-5773, CVE-2026-6276, and CVE-2026-14456 — an openssl QUIC DoS; the
# base still ships libcrypto3/libssl3 3.5.7-r0, verify with `apk policy libcrypto3`). Drop this RUN
# once caddy:2.11-alpine (or a later minor) is rebuilt with these fixed already, and update the digest.
FROM caddy:2.11-alpine@sha256:5f5c8640aae01df9654968d946d8f1a56c497f1dd5c5cda4cf95ab7c14d58648
RUN apk update && apk add --no-cache --upgrade \
	c-ares=1.34.8-r0 \
	curl=8.20.0-r0 \
	libcurl=8.20.0-r0 \
	libcrypto3=3.5.8-r0 \
	libssl3=3.5.8-r0
COPY --from=build /usr/bin/caddy /usr/bin/caddy
