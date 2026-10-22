# Contributing

## Rules

- Keep branches, commits, and PRs focused. Do not mix unrelated local changes into the same PR.
- Use semantic names by default.

## Naming

- Branches: `fix/<scope>-<summary>`, `feat/<scope>-<summary>`, `refactor/<scope>-<summary>`
- Commits: `fix(scope): summary`, `feat(scope): summary`, `refactor(scope): summary`
- PR titles: `fix(scope): summary`, `feat(scope): summary`, `refactor(scope): summary`

## Before opening a PR

For Go changes, run the local check list in the
[development guide](index.md#testing) - `go build`, `go vet`, `gofmt -l`,
`go test -race`, and `golangci-lint`. Also run the applicable
[security checks](#security-checks); findings fail their CI jobs.

For documentation changes, run `zensical build --clean --strict` using the
[documentation setup](documentation.md). Preview changes in a browser as well.

Don't skip `golangci-lint` for Go changes. It is the one check with no `go vet` overlap -
it's what catches unused functions left behind by a refactor and errors
formatted with `%v` instead of `%w`, neither of which fails a build or a
test. The pinned command in the development guide needs no install and matches the
version CI uses; a distro package may drift from it in either direction.

## Security checks

Each of these workflows fails on findings. Fix a finding where you can; each
tool's way to record a reviewed false positive is described below. Renovate
merges its own PRs only once every check passes, so add any new workflow that
runs on PRs to the `workflow_run` list in
[renovate.yml](https://github.com/voc0der/Coldarr/blob/main/.github/workflows/renovate.yml).

| Workflow | Finds | Runs on |
| --- | --- | --- |
| Semgrep Security Analysis | Insecure code patterns, from Semgrep's `auto` rules | PRs, pushes to `main`, weekly |
| CodeQL | Go and JavaScript data-flow problems ([below](#codeql-scans)) | PRs, pushes to `main` |
| Dependency Audit | Known vulnerabilities in Go modules (govulncheck) and the documentation's Python packages (pip-audit) | PRs, pushes to `main`, three times a week |
| Gitleaks | Credentials in committed changes: a PR's or a push's own commits, or the whole history | PRs, pushes to `main`, weekly |
| zizmor | Workflow problems such as injection, broad permissions, persisted credentials, and unpinned actions | PRs and pushes that change `.github/`, weekly |
| Fuzz | Inputs that break a security property of a parser or validator ([below](#fuzz-tests)) | PRs that change Go code; three nights a week after `main` changes |
| ZAP | Missing security headers, weak cookies, and information leaks in the app as built | PRs that change the app or its image |
| Container Scan | Known vulnerabilities in the released image, amd64 and arm64 (Grype) | Three times a week |

CI's Docker build also scans the image a PR builds, and a release scans both
architectures before pushing either (see [Releasing](index.md#releasing)).
Scheduled runs catch advisories published since the code last changed. Actions
are pinned by commit and scanner images by digest; Renovate updates those pins
and the scanner versions set in the workflows.

**Semgrep.** For a reviewed false positive, use `nosemgrep: <rule id>` with the
reason above it. The workflow skips one rule, Django's CSRF token check, which
reads Coldarr's Go templates as Django ones: Coldarr rejects cross-origin form
posts with `net/http.CrossOriginProtection` instead (`internal/webui/security.go`).
Locally, with the workflow's image:

```sh
image="$(sed -n 's/^ *image: //p' .github/workflows/semgrep.yml)"
docker run --rm -v "$PWD:/src" \
  --env GIT_CONFIG_COUNT=1 --env GIT_CONFIG_KEY_0=safe.directory --env GIT_CONFIG_VALUE_0=/src \
  "$image" semgrep scan --config=auto --error \
  --exclude-rule python.django.security.django-no-csrf-token.django-no-csrf-token
```

**Dependency Audit.** govulncheck fails when the code calls a vulnerable function;
vulnerable modules the code never calls are only listed. It checks Coldarr and
the screenshot gallery's own module in `tools/gallery`. Standard-library results
depend on the Go release running the scan, so CI uses the release image's Go (the
Dockerfile's `golang` tag, at its latest patch), not the minimum in `go.mod`.
pip-audit fails on any known vulnerability in the packages
`requirements-docs.txt` installs. Update the dependency rather than ignoring an
advisory; Renovate opens security updates without the usual three-day wait.
Locally, govulncheck checks your own Go installation's standard library:

```sh
workflow=.github/workflows/dependency-audit.yml
govulncheck="golang.org/x/vuln/cmd/govulncheck@$(sed -n 's/^ *GOVULNCHECK_VERSION: //p' $workflow)"
go run "$govulncheck" ./...
(cd tools/gallery && go run "$govulncheck" ./...)
uvx --from "pip-audit==$(sed -n 's/^ *PIP_AUDIT_VERSION: //p' $workflow)" pip-audit -r requirements-docs.txt
```

**Gitleaks** scans a PR's own commits, and the commits a push to `main` added. The
weekly run, a manual run, and any change to the Gitleaks image, `.gitleaks.toml` or
`.gitleaksignore` scan the whole history instead: only a new rule or a narrower
exception can turn up something in a commit already scanned. Findings are redacted
in the log. Locally it reads only committed changes. The `safe.directory`
setting lets Git read a checkout owned by another user, as the container's user
often is; without it, Gitleaks scans nothing and still passes:

```sh
image="$(sed -n 's/^ *GITLEAKS_IMAGE: //p' .github/workflows/gitleaks.yml)"
scan() {
  docker run --rm -v "$PWD:/repo:ro" \
    --env GIT_CONFIG_COUNT=1 --env GIT_CONFIG_KEY_0=safe.directory --env GIT_CONFIG_VALUE_0=/repo \
    "$image" git /repo --redact --verbose "$@"
}
scan --log-opts=origin/main..HEAD   # this branch
scan --log-opts=HEAD                # everything
```

A reviewed placeholder goes in a `.gitleaks.toml` at the repository root, as an
allowlist matched by both file and exact value. Revoke a real credential first,
then remove it from the PR branch's commits. If it has already reached `main`, add
the finding's `Fingerprint` to a `.gitleaksignore` file at the repository root
instead.

**zizmor** runs the release that zizmor-action pins. Its online audits, which also
check the pinned actions for known vulnerabilities, need a GitHub token; without
one, zizmor runs offline. Silence a reviewed finding with
`# zizmor: ignore[<audit>]` on the flagged line and the reason above it. Use
[.github/zizmor.yml](https://github.com/voc0der/Coldarr/blob/main/.github/zizmor.yml)
only where that line can't take the comment, such as a `uses:` line that ends in
the version comment Renovate reads.

```sh
GH_TOKEN="$(gh auth token)" uvx zizmor .
```

**Container Scan**, CI's image scan and the release scans fail on high or critical
vulnerabilities that have a fix. A failing scheduled scan means the published
image needs rebuilding, which the next release does. If a release's scan fails,
nothing is pushed: publish a new release once the cause is fixed, or re-run the
failed release workflow if a newer base image alone fixes it. Record a reviewed
false positive as an `ignore` rule in a `.grype.yaml` at the repository root. To
scan the same way with [Grype](https://github.com/anchore/grype):

```sh
docker build --pull -t coldarr:local .
grype docker:coldarr:local --fail-on high --only-fixed
GRYPE_PLATFORM=linux/arm64 grype registry:ghcr.io/voc0der/coldarr:latest --fail-on high --only-fixed
```

Without `--pull`, an outdated cached base image can report vulnerabilities that
CI's build doesn't have. `GRYPE_PLATFORM` picks one architecture of a multi-arch
image, as the scheduled scan does for each.

**ZAP** runs its passive baseline scan against a disposable container of the image
built from the PR, with a random password, a fresh `/config`, and no connections
or media. It sees what an unauthenticated visitor sees: the sign-in page, the
redirects to it, and static assets, not signed-in pages or an OIDC provider. ZAP
and its rules are pinned (`-silent` stops add-on updates), so a result changes
only with the app or the pin. Any warning fails;
[dev/zap/rules.tsv](https://github.com/voc0der/Coldarr/blob/main/dev/zap/rules.tsv)
lists reviewed rules, where `IGNORE` silences a rule and `INFO` reports it without
failing, each with its reason. The scan needs Docker and pulls the ZAP image,
several GB unpacked:

```sh
docker build -t coldarr:local . && bash dev/zap/scan.sh coldarr:local
```

### Fuzz tests

Each `Fuzz…` test asserts a security property over inputs the fuzzer generates.
Its seeds also run with the regular tests.

| Test | Property |
| --- | --- |
| `FuzzReturnTo` | A post-login redirect stays on the site, as a browser reads the `Location` header |
| `FuzzUntrustedProxy` | Forwarding headers from a peer that isn't a trusted proxy change neither the request's host nor its scheme, and never reach the handler |
| `FuzzProxyCIDRs` | An accepted trusted-proxy list holds only valid, zone-free prefixes, and trusts a peer exactly when `netip` says one of them contains it |
| `FuzzEncryptedConnection` | A saved connection reads back unchanged, and changing any bit of its nonce, ciphertext or key stops it decrypting |
| `FuzzDecryptMalformed` | A corrupt connection store fails with an error, never a panic, and whatever does decrypt saves and reads back unchanged |

Pull requests that change Go code run each fuzz test for 10 seconds; the scheduled
run, three nights a week, gives each 2 minutes when `main` has changed since the
previous run. A failing input is saved under the test's `testdata/fuzz/`
directory, and in CI as the run's `fuzz-failures` artifact. Commit it with the
fix: from then on it runs as a regular test case.
[dev/fuzz/run.sh](https://github.com/voc0der/Coldarr/blob/main/dev/fuzz/run.sh)
finds every `func Fuzz…(f *testing.F)` under `cmd` and `internal`, so a new fuzz
test needs no registration.

```sh
bash dev/fuzz/run.sh                  # each fuzz test for 10s
FUZZTIME=2m bash dev/fuzz/run.sh      # as long as the scheduled run
go test ./internal/webui -run '^$' -fuzz '^FuzzReturnTo$' -fuzztime 1m
```

### CodeQL scans

The [CodeQL harness](https://github.com/voc0der/Coldarr/blob/main/dev/codeql/README.md)
scans Go and JavaScript locally on Linux, with network access blocked during
analysis. Install the latest bundle with `python3 dev/codeql/scan.py install`,
check prerequisites with `python3 dev/codeql/scan.py doctor`, and run
`python3 dev/codeql/scan.py scan`. The same harness runs in Actions for PR updates
and pushes to `main`. Databases and reports stay outside the checkout and are
never uploaded; CI deletes them after each job. Findings fail the check, as do
scan execution failures. Run locally to inspect detailed reports. Fix findings;
for a reviewed false positive, put the reason in a comment, then
`codeql[<rule id>]` alone on the line directly above the flagged line. When
Semgrep flags that line too, its `nosemgrep` comment goes at the end of the line.

GitHub's CodeQL default setup also scans this repository. It keeps its alerts in
the Security tab, and its own CodeQL check fails a PR that adds a high-severity
alert. Dismissing an alert there doesn't affect the harness, which reads only the
`codeql[...]` comments. The Security tab recognizes an alert by the first 100
characters of its line, ignoring spaces, so rewording or reformatting a dismissed
alert's line brings it back as a new alert. Add a comment above the line or after
its existing text instead.

## Coverage

The coverage badge in [README.md](https://github.com/voc0der/Coldarr/blob/main/README.md) is a static shields.io badge -
there is deliberately no CI job computing it. That means the number is only
as current as the last person who refreshed it, so refresh it in the same PR
as any change that moves coverage: new tests, a new package, or a chunk of
untested code.

```sh
scripts/coverage.sh
```

That runs `go test ./... -covermode=atomic -coverprofile=coverage.out`, reads
the total out of `go tool cover -func`, picks the shields.io colour band for
it, and rewrites the badge URL in `README.md` in place. Commit the README
change along with the rest of your work. `coverage.out` is gitignored - it
stays behind so you can dig into what's actually uncovered:

```sh
go tool cover -html=coverage.out
```

To find out whether the badge is stale without touching the file - worth a
run before you open a PR:

```sh
scripts/coverage.sh --check
```

It exits non-zero and prints both the current and the correct badge URL if
they differ.

The number is whole-module statement coverage from each package's own tests:
code counts as covered only when a test in its own package runs it. Don't
switch the script to `-coverpkg=./...` to make the badge look better - that
counts code merely executed by an unrelated package's test as covered,
without anything testing it.
