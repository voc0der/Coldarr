#!/usr/bin/env python3
"""Local CodeQL scans. Only the explicit install command uses the network."""

import argparse
from collections import Counter
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile


ROOT = Path(__file__).resolve().parents[2]
ARCHIVE = "codeql-bundle-linux64.tar.zst"
LATEST_RELEASE_URL = "https://api.github.com/repos/github/codeql-action/releases/latest"
LANGUAGES = ("go", "javascript")
GO_PACKAGES = "./cmd/... ./internal/..."


def run(command, **kwargs):
    return subprocess.run(command, check=True, **kwargs)


def git(*args):
    return run(["git", "-C", str(ROOT), *args], stdout=subprocess.PIPE).stdout


def write_json(path, value):
    with tempfile.NamedTemporaryFile(mode="w", dir=path.parent, delete=False) as stream:
        temporary = Path(stream.name)
        try:
            stream.write(json.dumps(value, indent=2) + "\n")
            stream.close()
            temporary.replace(path)
        finally:
            temporary.unlink(missing_ok=True)


def state_directory():
    default = Path(os.environ.get("XDG_DATA_HOME", Path.home() / ".local/share")) / "coldarr-codeql"
    state = Path(os.environ.get("COLDARR_CODEQL_HOME", default)).expanduser().resolve()
    if state == ROOT or ROOT in state.parents or state in ROOT.parents:
        raise RuntimeError("COLDARR_CODEQL_HOME must be outside the repository.")
    state.mkdir(parents=True, exist_ok=True, mode=0o700)
    if state.stat().st_uid != os.getuid() or stat.S_IMODE(state.stat().st_mode) != 0o700:
        raise RuntimeError(f"Use a dedicated, owner-only state directory (0700): {state}")
    return state


def bundle(state, version):
    if not isinstance(version, str) or not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", version):
        raise RuntimeError("Invalid installed CodeQL version. Run python3 dev/codeql/scan.py install.")
    return state / "tools" / version / "codeql"


def installed_bundle(state):
    receipt = state / "tools/current.json"
    if not receipt.is_file():
        raise RuntimeError("CodeQL bundle is missing. Run python3 dev/codeql/scan.py install first.")
    metadata = json.loads(receipt.read_text())
    if not (bundle(state, metadata.get("version")) / "codeql").is_file():
        raise RuntimeError("Installed CodeQL executable is missing. Run python3 dev/codeql/scan.py install.")
    return metadata


def download(url, destination):
    run(["curl", "--disable", "--fail", "--location", "--silent", "--show-error", "--retry", "3",
         "--proto", "=https", "--proto-redir", "=https", "--tlsv1.2",
         "--output", str(destination), url], cwd=destination.parent)


def latest_bundle(temp):
    release_file = temp / "release.json"
    download(LATEST_RELEASE_URL, release_file)
    release = json.loads(release_file.read_text())
    tag = release.get("tag_name", "")
    match = re.fullmatch(r"codeql-bundle-v([0-9]+\.[0-9]+\.[0-9]+)", tag)
    if not match or release.get("draft") or release.get("prerelease"):
        raise RuntimeError("The latest official release is not a stable CodeQL bundle.")
    assets = [asset for asset in release.get("assets", []) if asset.get("name") == ARCHIVE]
    if len(assets) != 1:
        raise RuntimeError(f"The latest CodeQL release is missing {ARCHIVE}.")
    asset = assets[0]
    digest = re.fullmatch(r"sha256:([0-9a-fA-F]{64})", asset.get("digest") or "")
    url = f"https://github.com/github/codeql-action/releases/download/{tag}/{ARCHIVE}"
    if not digest or asset.get("browser_download_url") != url:
        raise RuntimeError("The latest CodeQL asset has no valid SHA-256 or official download URL.")
    return {"version": match[1], "url": url, "sha256": digest[1].lower()}


def install(state):
    if platform.system() != "Linux" or platform.machine() != "x86_64":
        raise RuntimeError("This bundle and network sandbox require Linux x86_64.")
    with tempfile.TemporaryDirectory(prefix="install-", dir=state) as temp:
        temp = Path(temp)
        print("Checking the latest official CodeQL bundle…", flush=True)
        metadata = latest_bundle(temp)
        version = metadata["version"]
        destination = bundle(state, version).parent
        destination.parent.mkdir(parents=True, exist_ok=True)
        if destination.exists():
            receipt = destination / "bundle.json"
            if not (destination / "codeql/codeql").is_file() or not receipt.is_file() or json.loads(receipt.read_text()) != metadata:
                raise RuntimeError(f"Existing bundle is incomplete or its checksum metadata differs: {destination}")
            write_json(state / "tools/current.json", metadata)
            print(f"Latest CodeQL {version} is already installed: {destination}")
            return
        archive = temp / ARCHIVE
        print(f"Downloading CodeQL {version} and its bundled queries…", flush=True)
        download(metadata["url"], archive)
        with archive.open("rb") as stream:
            digest = hashlib.file_digest(stream, "sha256").hexdigest()
        if digest != metadata["sha256"]:
            raise RuntimeError("CodeQL bundle checksum mismatch; nothing installed.")
        extracted = temp / "extracted"
        extracted.mkdir()
        # Use the data filter even for a checksum-verified vendor archive.
        with subprocess.Popen(["zstd", "--decompress", "--stdout", str(archive)], stdout=subprocess.PIPE) as decompressor:
            # Python's data filter rejects escaping paths/links and special files;
            # the rule does not recognize filter= (covered by malicious-archive tests).
            # nosemgrep: trailofbits.python.tarfile-extractall-traversal.tarfile-extractall-traversal
            with tarfile.open(fileobj=decompressor.stdout, mode="r|") as archive_file:
                archive_file.extractall(extracted, filter="data")
            if decompressor.wait():
                raise RuntimeError("Could not decompress the CodeQL bundle.")
        if not (extracted / "codeql/codeql").is_file():
            raise RuntimeError("The bundle did not contain the expected CodeQL executable.")
        write_json(extracted / "bundle.json", metadata)
        extracted.rename(destination)
        # Publish the selection only after verification and extraction succeed.
        write_json(state / "tools/current.json", metadata)
    print(f"Installed {destination}")


def snapshot(destination):
    """Copy working-tree content, including untracked source, but never ignored data."""
    paths = sorted(set(git("ls-files", "--cached", "--others", "--exclude-standard", "-z").split(b"\0")) - {b""})
    files = {}
    for raw in paths:
        relative = Path(os.fsdecode(raw))
        if relative.parts[0] in {".git", "data", "node_modules", "vendor", "coverage", "site", "config"}:
            continue
        source = ROOT / relative
        # Refuse symlinks, including parent-directory symlinks, rather than copying
        # a local secret or silently leaving source out of the scan.
        if any(part.is_symlink() for part in [source, *source.parents] if part != ROOT and ROOT in part.parents):
            raise RuntimeError(f"Source snapshot refuses symlinks: {relative}")
        if not source.exists():
            continue  # Tracked file deleted in the working tree.
        if not source.is_file():
            raise RuntimeError(f"Source snapshot requires ordinary files: {relative}")
        target = destination / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        content = source.read_bytes()
        target.write_bytes(content)
        target.chmod(0o700 if source.stat().st_mode & stat.S_IXUSR else 0o600)
        files[relative.as_posix()] = hashlib.sha256(content).hexdigest()
    if not files:
        raise RuntimeError("No source files found.")
    return files


def sandbox(state, work, command, version):
    """No host home, credentials, sockets, or network; no unsandboxed fallback."""
    if not shutil.which("bwrap"):
        raise RuntimeError("Install bubblewrap (bwrap); scans require its network isolation.")
    go_environment = json.loads(run(["go", "env", "-json", "GOMODCACHE", "GOROOT"], cwd="/", env={
        "PATH": os.environ.get("PATH", "/usr/bin:/bin"), "HOME": str(Path.home()),
        "GOENV": "off", "GOTOOLCHAIN": "local", "GOTELEMETRY": "off", "GOPROXY": "off", "GOSUMDB": "off",
    }, stdout=subprocess.PIPE, text=True).stdout)
    gomodcache = go_environment["GOMODCACHE"]
    goroot = Path(go_environment["GOROOT"]).resolve()
    if not Path(gomodcache).is_dir():
        raise RuntimeError("Go's module cache is missing. Run go mod download during setup.")
    if not (goroot / "bin/go").is_file():
        raise RuntimeError(f"Go's toolchain is missing from {goroot}.")
    for path in ("home", "tmp", "cache", "go-build", "go", "logs"):
        (work / path).mkdir(exist_ok=True)
    args = ["bwrap", "--unshare-all", "--die-with-parent", "--new-session", "--cap-drop", "ALL",
            "--ro-bind", "/usr", "/usr", "--symlink", "usr/bin", "/bin",
            "--symlink", "usr/lib", "/lib", "--symlink", "usr/lib64", "/lib64",
            "--ro-bind", "/etc", "/etc", "--proc", "/proc", "--dev", "/dev",
            "--tmpfs", "/tmp", "--dir", "/run",
            "--ro-bind", str(bundle(state, version)), "/tools/codeql",
            "--ro-bind", str(goroot), "/tools/go",
            "--ro-bind", gomodcache, "/gomodcache",
            "--bind", str(work), "/work",
            "--ro-bind", str(work / "source"), "/src",
            "--ro-bind", str(work / "source"), "/work/source",
            "--chdir", "/src", "--clearenv"]
    environment = {
        "PATH": "/tools/codeql:/tools/go/bin:/usr/bin:/bin", "HOME": "/work/home", "LANG": "C.UTF-8",
        "TMPDIR": "/work/tmp", "XDG_CACHE_HOME": "/work/cache",
        "GOROOT": "/tools/go", "GOPATH": "/work/go", "GOMODCACHE": "/gomodcache", "GOCACHE": "/work/go-build",
        "GOPROXY": "off", "GOSUMDB": "off", "GOTOOLCHAIN": "local", "GOTELEMETRY": "off",
        "GOENV": "off", "GOWORK": "off", "GOFLAGS": "-mod=readonly -buildvcs=false",
        "GOMAXPROCS": "2",
    }
    for key, value in environment.items():
        args += ["--setenv", key, value]
    return args + ["--", *command]


def execute(state, work, label, command, version):
    print(f"{label}…", flush=True)
    log = work / "logs" / f"{label}.log"
    args = sandbox(state, work, command, version)
    with log.open("w") as output:
        result = subprocess.run(args, stdout=output, stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL)
    if result.returncode:
        raise RuntimeError(f"{label} failed (exit {result.returncode}). See {log}")
    return log


def preflight(state, work, suite, version):
    if not (bundle(state, version) / "codeql").is_file():
        raise RuntimeError("CodeQL bundle is missing. Run python3 dev/codeql/scan.py install first.")
    # The probe must fail to connect. A sandbox creation error is also a hard failure.
    execute(state, work, "network-isolation", ["python3", "-c",
        "import errno, socket; s=socket.socket(); s.settimeout(2); "
        "result=s.connect_ex(('1.1.1.1',443)); "
        "assert result in (errno.ENETUNREACH,errno.EHOSTUNREACH), result; "
        "print('No external network route; isolated namespace verified')"], version)
    version_log = execute(state, work, "codeql-version", ["codeql", "version", "--format=json"], version)
    if json.loads(version_log.read_text())["version"] != version:
        raise RuntimeError(f"Expected installed CodeQL {version}; the executable does not match its receipt.")
    for language in LANGUAGES:
        execute(state, work, f"resolve-{language}", ["codeql", "resolve", "queries",
                query_suite(language, suite), suppression_query(language)], version)
    execute(state, work, "go-dependencies", ["go", "list", "-deps", *GO_PACKAGES.split()], version)


def query_suite(language, suite):
    return f"codeql/{language}-queries:codeql-suites/{language}-{suite}.qls"


def suppression_query(language):
    """Reads codeql[<rule id>] comments; the standard suites leave it out."""
    return f"codeql/{language}-queries:AlertSuppression.ql"


def suppressed(result):
    return any(entry.get("kind") == "inSource" and entry.get("status", "accepted") == "accepted"
               for entry in result.get("suppressions") or [])


def summarize(report):
    document = json.loads(report.read_text())
    runs = document.get("runs", [])
    if document.get("version") != "2.1.0" or not runs:
        raise RuntimeError(f"Missing SARIF runs or unexpected version: {report}")
    findings, reviewed = [], []
    for sarif_run in runs:
        components = [sarif_run.get("tool", {}).get("driver", {}), *sarif_run.get("tool", {}).get("extensions", [])]
        rules = {rule["id"]: rule for component in components for rule in component.get("rules", [])}
        for invocation in sarif_run.get("invocations", []):
            if invocation.get("executionSuccessful") is False:
                raise RuntimeError(f"SARIF records an unsuccessful analysis: {report}")
        for result in sarif_run.get("results", []):
            rule_id = result.get("ruleId", result.get("rule", {}).get("id", "unknown"))
            rule = rules.get(rule_id, {})
            location = next(iter(result.get("locations", [])), {}).get("physicalLocation", {})
            (reviewed if suppressed(result) else findings).append({
                "rule": rule_id,
                "level": result.get("level", rule.get("defaultConfiguration", {}).get("level", "warning")),
                "security_severity": rule.get("properties", {}).get("security-severity"),
                "message": result.get("message", {}).get("text", ""),
                "file": location.get("artifactLocation", {}).get("uri", ""),
                "line": location.get("region", {}).get("startLine"),
            })
    return {"count": len(findings), "levels": dict(Counter(f["level"] for f in findings)), "findings": findings,
            "suppressed": len(reviewed), "suppressed_findings": reviewed}


def scan(state, args):
    project = state / "projects" / hashlib.sha256(os.fsencode(ROOT)).hexdigest()[:12]
    runs = project / "runs"
    runs.mkdir(parents=True, exist_ok=True)
    prefix = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ-")
    work = Path(tempfile.mkdtemp(prefix=prefix, dir=runs))
    (work / "source").mkdir()
    manifest = {"status": "running", "repository": str(ROOT), "commit": git("rev-parse", "HEAD").decode().strip(),
                "suite": args.suite, "languages": list(LANGUAGES), "network": "isolated"}
    print(f"Private run directory: {work}", flush=True)
    try:
        # Select once so an installer running concurrently cannot change this scan.
        metadata = installed_bundle(state)
        version = metadata["version"]
        manifest.update(codeql=version, bundle=metadata)
        manifest["files"] = snapshot(work / "source")
        write_json(work / "manifest.json", manifest)
        preflight(state, work, args.suite, version)
        if args.command == "doctor":
            manifest["status"] = "preflight-passed"
            print(f"Ready with CodeQL {version}: both query suites and suppression queries resolve, Go dependencies are cached, and network access is blocked.")
            return 0
        (work / "databases").mkdir()
        (work / "reports").mkdir()
        summary = {}
        common = [f"--threads={args.threads}", f"--ram={args.ram}", "--common-caches=/work/cache/codeql"]
        for language in LANGUAGES:
            database = f"/work/databases/{language}"
            build = [f"--command=go build -a -p {args.threads} {GO_PACKAGES}"] if language == "go" else ["--build-mode=none"]
            execute(state, work, f"create-{language}", ["codeql", "database", "create", database,
                    f"--language={language}", "--source-root=/src", *common, *build], version)
            execute(state, work, f"analyze-{language}", ["codeql", "database", "analyze", database,
                    query_suite(language, args.suite), suppression_query(language), "--format=sarifv2.1.0",
                    f"--output=/work/reports/{language}.sarif", f"--sarif-category={language}",
                    "--no-download", "--no-sarif-add-file-contents", "--no-sarif-add-snippets", *common], version)
            summary[language] = summarize(work / "reports" / f"{language}.sarif")
        manifest["status"] = "complete"
        write_json(work / "summary.json", summary)
        # Updated only when both analyses succeed, never to a partial report.
        write_json(project / "latest.json", {"run": str(work)})
        for language, result in summary.items():
            print(f"{language}: {result['count']} findings ({result['suppressed']} suppressed after review)")
        print(f"Reports: {work / 'reports'}\nSummary: {work / 'summary.json'}")
        return 1 if args.fail_on_findings and any(r["count"] for r in summary.values()) else 0
    except BaseException as error:
        manifest["status"] = "failed"
        manifest["error"] = str(error)
        (work / "summary.json").unlink(missing_ok=True)
        raise
    finally:
        write_json(work / "manifest.json", manifest)


def positive(value):
    number = int(value)
    if number < 1:
        raise argparse.ArgumentTypeError("must be a positive integer")
    return number


def main():
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("install", "doctor", "scan"))
    parser.add_argument("--suite", choices=("security-extended", "security-and-quality"), default="security-extended")
    parser.add_argument("--threads", type=positive, default=2)
    parser.add_argument("--ram", type=positive, default=4096, help="CodeQL memory budget in MB (default: 4096)")
    parser.add_argument("--fail-on-findings", action="store_true", help="exit 1 when a completed scan has findings")
    args = parser.parse_args()
    try:
        state = state_directory()
        if args.command == "install":
            install(state)
            return 0
        return scan(state, args)
    except (OSError, RuntimeError, ValueError, tarfile.TarError, subprocess.CalledProcessError) as error:
        print(f"CodeQL: {error}", file=sys.stderr)
        return 2
    except KeyboardInterrupt:
        print("CodeQL: interrupted; run marked failed.", file=sys.stderr)
        return 130


if __name__ == "__main__":
    sys.exit(main())
