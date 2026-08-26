# Build the static smoked + smoke-agent binaries, then ship them on a small base that carries
# the external tools some probes shell out to (fping; irtt is optional and skipped if absent).
# One image, two entrypoints: `smoked` is the default; a federation vantage overrides the
# entrypoint to `smoke-agent` (see the compose the Vantages panel generates). ICMP via fping
# needs CAP_NET_RAW at runtime (see compose).
# Build image pinned by digest for reproducibility. Bump the tag + digest together on a refresh
# (Renovate keeps these current — see renovate.json); a pinned Go toolchain trades automatic patch
# uptake for a reviewable, reproducible build. CODE_REVIEW M4/L7.
FROM golang:1.26-alpine@sha256:70b46548e42db77e0966aaf3619fd068734dc6c77584d526b91126504fd95816 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# VERSION defaults to "dev" so a build that doesn't pass it (e.g. a bare `docker build .`)
# doesn't claim a release; CI passes the real `git describe` (see .github/workflows/ci.yml). CODE_REVIEW M8.
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=${VERSION}" -o /out/smoked ./cmd/smoked \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=${VERSION}" -o /out/smoke-agent ./cmd/smoke-agent

# Runtime base pinned by digest to a SUPPORTED Alpine branch (3.20 reached end of
# normal security support 2026-04-01). Bump the tag + digest together on a refresh
# (get the digest with `docker buildx imagetools inspect alpine:<ver>`). CODE_REVIEW M4.
FROM alpine:3.22@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce
# Grant fping just CAP_NET_RAW via a file capability (needs NET_RAW in the
# container's bounding set — see compose cap_add), so the collector can drop to a
# non-root user instead of running as root for ICMP. libcap is only needed to run
# setcap, so install it in a throwaway virtual package and remove it after.
# Forward-pin openssl to the patched apk already published on v3.22/main (CVE-2026-14456, an
# unbounded-memory QUIC DoS): the alpine:3.22 base image still ships libcrypto3/libssl3 3.5.7-r0 and
# has not been rebuilt with 3.5.8-r0 yet, so bumping the base tag/digest alone doesn't fix it — verify
# with `docker run --rm alpine:3.22 apk policy libcrypto3`. Drop this once the base ships 3.5.8-r0 and
# bump the digest instead.
RUN apk add --no-cache --upgrade libcrypto3=3.5.8-r0 libssl3=3.5.8-r0 \
 && apk add --no-cache fping ca-certificates tzdata \
 && apk add --no-cache --virtual .setcap libcap \
 && setcap cap_net_raw+ep "$(command -v fping)" \
 && apk del .setcap \
 && adduser -D -H -u 10001 smoked \
 && mkdir -p /var/lib/smoke-agent/spool \
 && chown smoked /var/lib/smoke-agent/spool
COPY --from=build /out/smoked /usr/local/bin/smoked
COPY --from=build /out/smoke-agent /usr/local/bin/smoke-agent
COPY web /web
COPY config.example.yaml /etc/heliograph/config.yaml
# Ship the MIT license notice inside the image (the source is MIT; a distributed
# binary artifact should carry it too). CODE_REVIEW carried-forward.
COPY LICENSE /LICENSE
USER smoked
EXPOSE 8087
ENTRYPOINT ["smoked"]
CMD ["-serve", "-addr", ":8087", "-webdir", "/web"]
