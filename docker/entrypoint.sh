#!/bin/sh
# Creates/reuses a user matching PUID/PGID, then execs coldarr as that user.
# Mirrors the PUID/PGID convention used by Radarr/Sonarr/Jellyfin images so
# Coldarr can read the same bind-mounted paths without a permissions fight.
set -e

PUID="${PUID:-1000}"
PGID="${PGID:-1000}"

if ! getent group "$PGID" >/dev/null 2>&1; then
  addgroup -g "$PGID" coldarr
fi
GROUP_NAME="$(getent group "$PGID" | cut -d: -f1)"

if ! getent passwd "$PUID" >/dev/null 2>&1; then
  adduser -D -H -u "$PUID" -G "$GROUP_NAME" coldarr
fi
USER_NAME="$(getent passwd "$PUID" | cut -d: -f1)"

exec su-exec "${USER_NAME}:${GROUP_NAME}" /usr/local/bin/coldarr "$@"
