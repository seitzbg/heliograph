# Caddy with DNS-challenge provider plugins compiled in.
#
# The stock `caddy` image ships NO DNS provider modules, so DNS-01 (needed for
# certificates without an inbound port 80, behind NAT, or for wildcards) requires
# a custom build. This bakes in a set of common providers; the operator picks one
# at runtime via CADDY_ACME_DNS (see .env.example). To support another provider,
# add a `--with github.com/caddy-dns/<name>` line below and rebuild.
ARG CADDY_VERSION=2.11

FROM caddy:${CADDY_VERSION}-builder AS build
RUN xcaddy build \
	--with github.com/caddy-dns/cloudflare \
	--with github.com/caddy-dns/route53 \
	--with github.com/caddy-dns/digitalocean \
	--with github.com/caddy-dns/duckdns \
	--with github.com/caddy-dns/namecheap \
	--with github.com/caddy-dns/gandi

FROM caddy:${CADDY_VERSION}-alpine
COPY --from=build /usr/bin/caddy /usr/bin/caddy
