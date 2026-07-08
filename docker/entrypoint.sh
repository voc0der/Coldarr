#!/bin/sh
# Creates/reuses a user matching PUID/PGID, then execs coldarr as that user.
# Mirrors the PUID/PGID convention used by Radarr/Sonarr/Jellyfin images so
# Coldarr can read the same bind-mounted paths without a permissions fight.
set -e

PUID="${PUID:-1000}"
PGID="${PGID:-1000}"

if [ "$(id -u)" != "0" ]; then
  # Already running as a non-root user (`docker run --user`, a compose
  # `user:` override, a rootless runtime, or a Kubernetes securityContext).
  # None of the steps below are reachable without root, and none of them
  # are needed either: file access is checked against the numeric uid/gid
  # we already have, not against named /etc/passwd|group entries, so as
  # long as that uid/gid already owns/matches /config there's nothing to
  # fix up. PUID/PGID are ignored in this mode - the caller already chose
  # the identity by starting the container this way.
  exec /usr/local/bin/coldarr "$@"
fi

if ! getent group "$PGID" >/dev/null 2>&1; then
  addgroup -g "$PGID" coldarr
fi
GROUP_NAME="$(getent group "$PGID" | cut -d: -f1)"

if ! getent passwd "$PUID" >/dev/null 2>&1; then
  adduser -D -H -u "$PUID" -G "$GROUP_NAME" coldarr
fi
USER_NAME="$(getent passwd "$PUID" | cut -d: -f1)"

# Docker creates a bind-mounted /config as root:root if the host directory
# doesn't already exist, which leaves the unprivileged user below unable to
# write coldarr.yaml, the encrypted connection store, or its key. /config
# is small (a handful of config/state files) and exclusively Coldarr's, so
# reclaiming it here is cheap and safe - unlike the media tier mounts,
# which are owned by Radarr/Sonarr/etc. and are never touched.
mkdir -p /config
if ! chown -R "$PUID:$PGID" /config; then
  echo "warning: could not chown /config to $PUID:$PGID - expect permission errors if its ownership doesn't already match" >&2
fi

exec su-exec "${USER_NAME}:${GROUP_NAME}" /usr/local/bin/coldarr "$@"
