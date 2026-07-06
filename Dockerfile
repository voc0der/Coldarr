# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/coldarr ./cmd/coldarr

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata su-exec

COPY --from=builder /out/coldarr /usr/local/bin/coldarr
COPY docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# /config holds coldarr.yaml, the encrypted connection store, its
# encryption key, and the move-history JSON. Media tier paths (hot/cold
# root folders) are separate bind mounts the operator adds at `docker run`
# / compose time - they must be mounted at the same paths Radarr/Sonarr
# see internally, or Coldarr's disk checks and path matching against Arr
# API data will disagree with reality.
VOLUME ["/config"]
WORKDIR /config

# PUID/PGID: match these to whatever user owns your media/config, the same
# way Radarr/Sonarr/Jellyfin images work. Defaults to 1000:1000.
ENV PUID=1000
ENV PGID=1000

EXPOSE 8478

# Only meaningful while running `serve` (the default CMD) - a one-shot
# `report`/`plan`/`apply` invocation just exits before this has a chance
# to matter.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD proto=http; if [ -n "${COLDARR_TLS_CERT_FILE:-}" ] && [ -n "${COLDARR_TLS_KEY_FILE:-}" ]; then proto=https; fi; wget -q --no-check-certificate -O- "${proto}://127.0.0.1:$(echo "${COLDARR_LISTEN_ADDR:-:8478}" | sed 's/.*://')/healthz" >/dev/null || exit 1

ENTRYPOINT ["/entrypoint.sh"]
CMD ["serve"]
