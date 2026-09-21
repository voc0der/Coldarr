# syntax=docker/dockerfile:1
#
# Coldarr (built from this checkout) plus the gallery's fake drives and fake
# Radarr/Sonarr/Jellyfin, in one container so Coldarr sees the FUSE mounts.
# Build from the repo root:
#
#   docker build -f tools/gallery/Dockerfile -t coldarr-gallery .
#
# Run with FUSE access (see run.sh):
#
#   docker run --rm --device /dev/fuse --cap-add SYS_ADMIN \
#     --security-opt apparmor=unconfined -p 127.0.0.1:18478:8478 coldarr-gallery

FROM golang:1.27-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/coldarr ./cmd/coldarr

WORKDIR /src/tools/gallery
COPY tools/gallery/go.mod tools/gallery/go.sum ./
RUN go mod download
COPY tools/gallery/*.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/gallery .

FROM alpine:3.24
RUN apk add --no-cache ca-certificates curl tzdata
COPY --from=builder /out/coldarr /out/gallery /usr/local/bin/
COPY tools/gallery/fixture.yaml tools/gallery/coldarr.yaml tools/gallery/entrypoint.sh /gallery/
RUN chmod +x /gallery/entrypoint.sh

# Short settle/resolve timings so an apply lands in about a minute, and a
# scheduler tick long enough that no scheduled task fires mid-capture.
ENV COLDARR_PASSWORD=gallery \
    COLDARR_SETTLE_CHECK_INTERVAL=1s \
    COLDARR_SETTLE_STABLE_CHECKS=2 \
    COLDARR_SETTLE_MAX_WAIT=5m \
    COLDARR_JELLYFIN_RESOLVE_INTERVAL=1s \
    COLDARR_JELLYFIN_RESOLVE_TIMEOUT=1m \
    COLDARR_SCHEDULER_TICK_INTERVAL=24h

EXPOSE 8478
ENTRYPOINT ["/gallery/entrypoint.sh"]
