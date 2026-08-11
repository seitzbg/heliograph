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
FROM caddy:2.11-builder@sha256:198d47eaee306d4d0c38a9960c89ff2c959aa29ad51d3e2dafa3e93ac961782a AS build
RUN xcaddy build \
	--with github.com/caddy-dns/cloudflare@v0.2.4 \
	--with github.com/caddy-dns/route53@v1.6.2 \
	--with github.com/caddy-dns/digitalocean@v0.0.0-20250606074528-04bde2867106 \
	--with github.com/caddy-dns/duckdns@v0.5.0 \
	--with github.com/caddy-dns/namecheap@v1.0.0 \
	--with github.com/caddy-dns/gandi@v1.1.0

FROM caddy:2.11-alpine@sha256:5f5c8640aae01df9654968d946d8f1a56c497f1dd5c5cda4cf95ab7c14d58648
COPY --from=build /usr/bin/caddy /usr/bin/caddy
