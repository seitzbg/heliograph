# Build a static smoked binary, then ship it on a small base that carries the
# external tools some probes shell out to (fping; irtt is optional and skipped
# if absent). ICMP via fping needs CAP_NET_RAW at runtime (see compose).
# Build image pinned by digest for reproducibility. Bump the tag + digest together on a refresh
# (Renovate keeps these current — see renovate.json); a pinned Go toolchain trades automatic patch
# uptake for a reviewable, reproducible build. CODE_REVIEW M4/L7.
FROM golang:1.26-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/smoked ./cmd/smoked

# Runtime base pinned by digest to a SUPPORTED Alpine branch (3.20 reached end of
# normal security support 2026-04-01). Bump the tag + digest together on a refresh
# (get the digest with `docker buildx imagetools inspect alpine:<ver>`). CODE_REVIEW M4.
FROM alpine:3.22@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce
# Grant fping just CAP_NET_RAW via a file capability (needs NET_RAW in the
# container's bounding set — see compose cap_add), so the collector can drop to a
# non-root user instead of running as root for ICMP. libcap is only needed to run
# setcap, so install it in a throwaway virtual package and remove it after.
RUN apk add --no-cache fping ca-certificates tzdata \
 && apk add --no-cache --virtual .setcap libcap \
 && setcap cap_net_raw+ep "$(command -v fping)" \
 && apk del .setcap \
 && adduser -D -H -u 10001 smoked
COPY --from=build /out/smoked /usr/local/bin/smoked
COPY web /web
COPY config.example.yaml /etc/smokeping/config.yaml
USER smoked
EXPOSE 8087
ENTRYPOINT ["smoked"]
CMD ["-serve", "-addr", ":8087", "-webdir", "/web"]
