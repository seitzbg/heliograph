# Build a static smoked binary, then ship it on a small base that carries the
# external tools some probes shell out to (fping; irtt is optional and skipped
# if absent). ICMP via fping needs CAP_NET_RAW at runtime (see compose).
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/smoked ./cmd/smoked

FROM alpine:3.20
RUN apk add --no-cache fping ca-certificates tzdata
COPY --from=build /out/smoked /usr/local/bin/smoked
COPY web /web
COPY config.example.yaml /etc/smokeping/config.yaml
EXPOSE 8087
ENTRYPOINT ["smoked"]
CMD ["-serve", "-addr", ":8087", "-webdir", "/web"]
