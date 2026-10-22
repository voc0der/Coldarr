# Private CodeQL scans

The harness scans the Go server and JavaScript sources on Linux x86_64.
Only this harness belongs in Git. Databases, source snapshots, caches, logs,
and SARIF reports live outside the checkout, under
`~/.local/share/coldarr-codeql/` (or `$XDG_DATA_HOME/coldarr-codeql/`).
The same harness runs in GitHub Actions for PR updates and pushes to `main`.
Detailed reports stay on the runner and are deleted after the job.

## Setup

Install Python 3.12+, Git, curl, zstd, bubblewrap (`bwrap`), and the project's
Go toolchain. Unprivileged user and network namespaces must be available.
Ubuntu 24.04's AppArmor restriction blocks them for bwrap unless you install
`apparmor-profiles` and load its `bwrap-userns-restrict` profile, as the CodeQL
workflow does. Where the namespaces are unavailable, the harness tests skip their
real-sandbox check, but `doctor` and scans still fail.
The sandbox expects system tools under `/usr`, as on Debian, Ubuntu, and Arch.
It mounts the selected Go toolchain separately, including installations made by
`actions/setup-go` outside `/usr`.
Allow several GB of disk space and about 4 GB of available RAM for analysis.

Coldarr is MIT-licensed open source, which the
[CodeQL license terms](https://github.com/github/codeql-cli-binaries/blob/main/LICENSE.md)
allow it to be analyzed under. Keeping analysis local does not change those terms.

```bash
python3 dev/codeql/scan.py install
go mod download
python3 dev/codeql/scan.py doctor
python3 dev/codeql/scan.py scan
```

The installer is the only harness command that accesses the network. Each run
checks GitHub's latest stable CodeQL bundle release and installs its Linux bundle,
including precompiled queries. It verifies the asset's SHA-256 from the official
release metadata before extraction and sends no repository data. An already
installed latest bundle is reused after its installation receipt is checked.
`go mod download` is a separate dependency setup step; skip it if the Go module
cache is already populated. It contacts module services for dependencies.
Scans never install dependencies or download query packs. The web UI's
JavaScript needs no build for analysis.

`doctor` checks network isolation, the CLI version, both query suites and their
suppression queries, and offline Go dependency resolution. It saves diagnostic
logs in its own private run directory.
Missing dependencies, query packs, or unavailable isolation cause a hard failure;
there is no fallback that enables network access.

## Run and inspect

```bash
python3 dev/codeql/scan.py scan
python3 dev/codeql/scan.py scan --fail-on-findings
python3 dev/codeql/scan.py scan --suite security-and-quality --threads 4 --ram 8192
python3 -m unittest discover -s dev/codeql -p 'test_*.py'
```

The default suite is `security-extended` for both languages. The Go extraction
forces a fresh build of `./cmd/... ./internal/...`; it does not execute
the application or tests. It scans production Go source, not Go test bodies.
JavaScript analysis includes the embedded UI sources. Go extraction includes
the templates and static assets needed by `go:embed`. The separate gallery Go
module is covered by Dependency Audit and Semgrep, not this production build.

The scan prints a unique run directory containing:

- `reports/go.sarif` and `reports/javascript.sarif`: SARIF 2.1.0 reports.
- `summary.json`: finding counts, rule IDs, messages, and source locations, with
  reviewed suppressions listed separately under `suppressed_findings`.
- `manifest.json`: completion status, Git commit, suite, installed CodeQL version,
  bundle URL/checksum, and hashes of the exact working-tree files scanned,
  including uncommitted changes.
- `source/`, `databases/`, and `logs/`: local source snapshot and diagnostics.

Reports use paths relative to the source root. View the SARIF locally or read
`summary.json`; the source snapshot preserves the code associated with a finding.
No file contents or snippets are added to SARIF, but messages, paths, databases,
and logs can still contain sensitive information. Keep the entire run directory
private. CodeQL findings need review and a zero count is not proof of security.

Exit status is **0** for a completed scan, **1** for unsuppressed findings when
`--fail-on-findings` is set, and **2** for setup or analysis failures. An interrupted
scan exits **130**. Failed runs retain diagnostics and are marked failed; they
never receive a complete summary. The project's `latest.json` pointer changes
only after both languages finish, so check the current exit status and run path
instead of treating an older successful report as the latest attempt.

## Reviewed false positives

Fix real findings. For a reviewed false positive or accepted risk, put the reason
in a comment, then `codeql[<rule id>]` alone on the line directly above the
flagged line. It covers only that next line:

```go
// cleanReturnTo rejects foreign origins; FuzzReturnTo checks the emitted Location.
// codeql[go/unvalidated-url-redirection]
http.Redirect(w, r, returnTo, http.StatusFound)
```

Each analysis also runs CodeQL's `AlertSuppression.ql` query, which the standard
suites leave out, so the harness reports these results as suppressed rather than
as findings, and `--fail-on-findings` ignores them. When Semgrep flags the same
line, its `nosemgrep: <rule id>` comment goes at the end of that line instead.

## Privacy and maintenance

The [CodeQL workflow](../../.github/workflows/codeql.yml) runs on GitHub-hosted
Ubuntu runners for PRs targeting `main` (including each new PR commit) and pushes
to `main`. It installs the latest bundle, runs the harness tests and both language
scans, then deletes its private state directory even if a step fails. PR jobs
use the proposed merge commit, as checked out by the PR event.

The job has read-only repository permissions and does not upload SARIF, databases,
artifacts, or caches. GitHub receives the usual CI logs, including progress and
finding counts; detailed finding messages and source snapshots remain on the
temporary runner. CI scans with `--fail-on-findings`, so unsuppressed findings fail
the check, as do installation, isolation, extraction, or analysis errors. Use the
local harness to inspect findings.

Every CLI invocation, Go build, and query runs inside bubblewrap with a new network
namespace, a clean environment, an isolated home and process namespace, and no
access to the host home or runtime sockets. Source and the Go dependency cache
are mounted read-only. The kernel blocks external networking; Go's proxy,
checksum lookup, toolchain download, and telemetry are also disabled. A connectivity
probe verifies that no external route exists before analysis starts.

Each scan snapshots Git-tracked and nonignored untracked working-tree files.
Ignored files, deployment `config/` and `data/`, `node_modules/`, `vendor/`,
coverage reports, and generated documentation under `site/` are excluded.
Symlinks and submodules are refused rather than following paths into unrelated
local data. Source changes
during snapshot creation can span multiple file versions; avoid editing during
that brief step. The checkout itself is never built or modified by the harness.

The state directory is owner-only (0700), with new files created under umask 077.
This protects against other ordinary users, not root, your own user account,
or backup/sync software. Choose an unsynced local disk. To use another location,
set `COLDARR_CODEQL_HOME` to an absolute path **outside** the repository.
Separate checkouts receive separate project/run directories. Runs are retained
until you delete them; remove old run directories when no longer needed.

To upgrade to the latest official
[bundle release](https://github.com/github/codeql-action/releases), rerun
`python3 dev/codeql/scan.py install`, then run `doctor` and a complete scan. No
source edits are needed. The installer records the selected bundle in `tools/current.json`
under the private state directory. Each version has its own installation;
failed downloads or checksum verification leave the previous selection intact.

Scans and `doctor` use the installed bundle entirely offline; they do not check
for updates. A scan selects its bundle once at startup, so a concurrent upgrade
does not change its analysis midway through. Upgrading does not delete old
installations or reports. Do not add upload steps or publish the private run
directory as a CI artifact.
