#!/usr/bin/env bash
# Runs every Go fuzz test in the repository, one at a time, each for FUZZTIME
# (a Go duration; default 10s). A failing input is saved under that test's
# testdata/fuzz/ directory: commit it with the fix, and it runs as a regular
# test case from then on.
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

fuzztime="${FUZZTIME:-10s}"
tests="$(grep -rEo --include='*_test.go' '^func Fuzz[A-Za-z0-9_]*\(f \*testing\.F\)' cmd internal |
  sed -E 's#^(.*)/[^/]+_test\.go:func (Fuzz[A-Za-z0-9_]*).*#./\1 \2#' | sort)"
if [ -z "$tests" ]; then
  echo "No fuzz tests found." >&2
  exit 1
fi

count=0
failed=()
while read -r pkg name; do
  count=$((count + 1))
  [ -n "${GITHUB_ACTIONS:-}" ] && echo "::group::$name ($pkg)"
  go test "$pkg" -run '^$' -fuzz "^${name}\$" -fuzztime "$fuzztime" </dev/null || failed+=("$name ($pkg)")
  [ -n "${GITHUB_ACTIONS:-}" ] && echo "::endgroup::"
done <<<"$tests"

if [ "${#failed[@]}" -gt 0 ]; then
  printf 'Failed: %s\n' "${failed[@]}" >&2
  exit 1
fi
echo "All $count fuzz tests passed, $fuzztime each."
