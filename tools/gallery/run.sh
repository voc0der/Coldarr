#!/usr/bin/env bash
# Builds the gallery container from this checkout, starts it, and either
# captures every screenshot scene (default) or leaves it running to browse.
#
#   tools/gallery/run.sh                      # capture into assets/screenshots/
#   tools/gallery/run.sh --out /tmp/shots     # capture somewhere else
#   tools/gallery/run.sh --only plan history  # capture just these scenes
#   tools/gallery/run.sh --serve              # leave it running, print the URL
#
# Needs Docker (rootless is fine) with FUSE available on the host, and for
# capturing, Playwright: pip install playwright && playwright install chromium
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/../.." && pwd)
name=coldarr-gallery
port=${GALLERY_PORT:-18478}
out=$repo/assets/screenshots
serve=false
only=()

while [ $# -gt 0 ]; do
  case $1 in
    --serve) serve=true ;;
    --out) out=$2; shift ;;
    --port) port=$2; shift ;;
    --only) shift; while [ $# -gt 0 ] && [[ $1 != --* ]]; do only+=("$1"); shift; done; continue ;;
    -h|--help) sed -n '2,11p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done

version=$(git -C "$repo" describe --tags --abbrev=0 2>/dev/null || echo dev)
echo "building $name ($version)..."
docker build -q -f "$here/Dockerfile" --build-arg VERSION="$version" -t "$name" "$repo" >/dev/null

docker rm -f "$name" >/dev/null 2>&1 || true
docker run -d --name "$name" \
  --device /dev/fuse --cap-add SYS_ADMIN --security-opt apparmor=unconfined \
  -e TZ="${TZ:-UTC}" \
  -p "127.0.0.1:$port:8478" "$name" >/dev/null
$serve || trap 'docker rm -f "$name" >/dev/null 2>&1 || true' EXIT

for _ in $(seq 1 90); do
  docker exec "$name" test -f /run/coldarr.ready 2>/dev/null && break
  if [ "$(docker inspect -f '{{.State.Running}}' "$name")" != true ]; then break; fi
  sleep 1
done
if ! docker exec "$name" test -f /run/coldarr.ready 2>/dev/null; then
  echo "the gallery container never became ready:" >&2
  docker logs "$name" >&2
  exit 1
fi

if $serve; then
  echo "Coldarr gallery: http://127.0.0.1:$port  (password: gallery)"
  echo "stop it with: docker rm -f $name"
  exit 0
fi

capture_args=(--base "http://127.0.0.1:$port" --out "$out" --container "$name")
[ ${#only[@]} -gt 0 ] && capture_args+=(--only "${only[@]}")
python3 "$here/capture.py" "${capture_args[@]}"
