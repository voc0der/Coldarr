#!/usr/bin/env bash
#
# Recompute test coverage and refresh the coverage badge in README.md.
#
# Coldarr has no CI coverage job on purpose - the badge is a plain static
# shields.io badge whose number lives in README.md, and this script is what
# keeps that number honest. Run it whenever a change moves coverage.
#
#   scripts/coverage.sh           recompute and rewrite the README badge
#   scripts/coverage.sh --check   recompute and fail if the badge is stale
#
# Leaves the profile at coverage.out (gitignored) so you can drill in with:
#   go tool cover -html=coverage.out
#
set -euo pipefail

cd "$(dirname "$0")/.."

check=0
case "${1:-}" in
	--check) check=1 ;;
	"") ;;
	*)
		echo "usage: scripts/coverage.sh [--check]" >&2
		exit 2
		;;
esac

go test ./... -covermode=atomic -coverprofile=coverage.out

pct=$(go tool cover -func=coverage.out | awk '/^total:/ {print $3}')
pct=${pct%\%}
if [ -z "$pct" ]; then
	echo "coverage: could not read a total from coverage.out" >&2
	exit 1
fi

# shields.io's own coverage colour bands.
color=$(awk -v p="$pct" 'BEGIN {
	if (p >= 90) print "brightgreen"
	else if (p >= 80) print "green"
	else if (p >= 70) print "yellowgreen"
	else if (p >= 60) print "yellow"
	else if (p >= 50) print "orange"
	else print "red"
}')

badge="https://img.shields.io/badge/coverage-${pct}%25-${color}"
current=$(grep -o 'https://img\.shields\.io/badge/coverage-[^"]*' README.md || true)

if [ -z "$current" ]; then
	echo "coverage: no coverage badge found in README.md" >&2
	exit 1
fi

echo
echo "total coverage: ${pct}%"

if [ "$current" = "$badge" ]; then
	echo "README badge already up to date."
	exit 0
fi

if [ "$check" -eq 1 ]; then
	echo "README badge is stale:" >&2
	echo "  have: $current" >&2
	echo "  want: $badge" >&2
	echo "Run scripts/coverage.sh to update it." >&2
	exit 1
fi

sed -i -E "s|https://img\.shields\.io/badge/coverage-[^\"]*|${badge}|" README.md
echo "README badge updated: $badge"
