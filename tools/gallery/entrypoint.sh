#!/bin/sh
# Mounts the fake drives and starts the fake Radarr/Sonarr/Jellyfin, points
# a fresh Coldarr at them, then warms the caches a long-running install
# would already have (quality cutoffs, Jellyfin links, orphan scan) so the
# first page load looks lived-in. /run/coldarr.ready appears once that's
# done - run.sh waits on it.
set -eu

CONFIG=/config/coldarr.yaml
COLDARR="coldarr --config $CONFIG"
URL=http://127.0.0.1:8478

rm -f /run/gallery.ready /run/coldarr.ready
mkdir -p /config
cp /gallery/coldarr.yaml "$CONFIG"

# The connections read like a typical compose setup's service names.
echo "127.0.0.1 radarr sonarr jellyfin" >>/etc/hosts

gallery --fixture /gallery/fixture.yaml --coldarr-config "$CONFIG" \
  --history-out /config/coldarr-history.json --ready-file /run/gallery.ready &

i=0
until [ -f /run/gallery.ready ]; do
  i=$((i + 1))
  if [ "$i" -gt 150 ]; then
    echo "gallery: fake drives/APIs never came up" >&2
    exit 1
  fi
  sleep 0.2
done

$COLDARR connections set radarr --url http://radarr:7878 --api-key 3f2a9c7e1b8d4f6a0c5e9b2d7a1f4c8e >/dev/null
$COLDARR connections set sonarr --url http://sonarr:8989 --api-key 9d4e1a7c3b6f2e8a5c0d9b4f7a2e6c1d >/dev/null
$COLDARR connections set jellyfin --url http://jellyfin:8096 --api-key 6b1f8e3a9c4d7b2e0f5a8c3d6e9b1a4f >/dev/null

(
  until curl -fs -o /dev/null "$URL/healthz"; do sleep 0.3; done
  jar=/tmp/gallery-cookies
  curl -fsS -o /dev/null -c "$jar" --data-urlencode "password=$COLDARR_PASSWORD" "$URL/login"
  # Coldarr answers an unauthenticated POST with a redirect to /login, and
  # a failed task with its page re-rendered around an error - so anything
  # but a clean 200 means the warm-up didn't take.
  post() {
    code=$(curl -sS -b "$jar" -o /tmp/gallery-post -w '%{http_code}' -X POST "$@")
    if [ "$code" != 200 ] || grep -q 'alert alert-error' /tmp/gallery-post; then
      echo "gallery: warm-up POST $* failed (HTTP $code):" >&2
      grep -o 'alert alert-error">[^<]*' /tmp/gallery-post >&2 || true
      exit 1
    fi
  }
  for app in radarr sonarr jellyfin; do
    post --data-urlencode "external_url=https://$app.example.com" "$URL/settings/connections/$app/external-url"
  done
  post "$URL/settings/scheduler/scan_cutoffs/run"
  post "$URL/settings/scheduler/refresh_links/run"
  post "$URL/settings/scheduler/scan_orphans/run"
  touch /run/coldarr.ready
  echo "gallery: ready - Coldarr on :8478, password '$COLDARR_PASSWORD'"
) &

exec $COLDARR serve
