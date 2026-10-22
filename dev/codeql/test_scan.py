import argparse
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import patch

import scan


def sandbox_available():
    """Whether bwrap can create the namespaces scans use on this host."""
    if not shutil.which("bwrap"):
        return False
    probe = subprocess.run(["bwrap", "--unshare-all", "--die-with-parent", "--ro-bind", "/", "/", "true"],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    return probe.returncode == 0


class HarnessTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.root = self.base / "repo"
        self.root.mkdir()
        subprocess.run(["git", "init", "-q", str(self.root)], check=True)
        (self.root / ".gitignore").write_text(".env\nnode_modules/\n")
        (self.root / "app.go").write_text("before")
        (self.root / "deleted.go").write_text("delete me")
        subprocess.run(["git", "-C", str(self.root), "add", "."], check=True)
        subprocess.run(["git", "-C", str(self.root), "-c", "user.name=Test", "-c",
                        "user.email=test@example.invalid", "commit", "-qm", "fixture"], check=True)
        self.root_patch = patch.object(scan, "ROOT", self.root)
        self.root_patch.start()
        self.addCleanup(self.root_patch.stop)

    def metadata(self, version="9.8.7", content=b"expected archive"):
        return {"version": version, "sha256": scan.hashlib.sha256(content).hexdigest(),
                "url": f"https://github.com/github/codeql-action/releases/download/codeql-bundle-v{version}/{scan.ARCHIVE}"}

    def release(self, metadata):
        return {"tag_name": f"codeql-bundle-v{metadata['version']}", "draft": False, "prerelease": False,
                "assets": [{"name": scan.ARCHIVE, "digest": f"sha256:{metadata['sha256']}",
                            "browser_download_url": metadata["url"]}]}

    def installed(self, state, metadata):
        executable = scan.bundle(state, metadata["version"]) / "codeql"
        executable.parent.mkdir(parents=True)
        executable.touch()
        scan.write_json(executable.parent.parent / "bundle.json", metadata)
        scan.write_json(state / "tools/current.json", metadata)

    def test_snapshot_uses_working_tree_and_excludes_local_data(self):
        (self.root / "app.go").write_text("after")
        (self.root / "deleted.go").unlink()
        (self.root / "new.jsx").write_text("new")
        (self.root / ".env").write_text("private")
        (self.root / "data").mkdir()
        (self.root / "data/secret").write_text("private")
        (self.root / "node_modules").mkdir()
        (self.root / "node_modules/dependency.js").write_text("dependency")
        (self.root / "site").mkdir()
        (self.root / "site/bundle.js").write_text("generated")
        destination = self.base / "snapshot"
        destination.mkdir()
        before = scan.git("status", "--porcelain")
        files = scan.snapshot(destination)
        self.assertEqual(set(files), {".gitignore", "app.go", "new.jsx"})
        self.assertEqual((destination / "app.go").read_text(), "after")
        self.assertEqual(scan.git("status", "--porcelain"), before)

    def test_snapshot_rejects_symlinks(self):
        (self.root / "secret.go").symlink_to(self.base / "secret")
        with self.assertRaisesRegex(RuntimeError, "symlinks"):
            scan.snapshot(self.base / "snapshot")

    def test_state_cannot_be_inside_git_checkout(self):
        with patch.dict(os.environ, {"COLDARR_CODEQL_HOME": str(self.root / ".codeql")}):
            with self.assertRaisesRegex(RuntimeError, "outside"):
                scan.state_directory()

    def test_state_does_not_change_permissions_on_existing_directories(self):
        existing = self.base / "shared"
        existing.mkdir(mode=0o755)
        existing.chmod(0o755)
        with patch.dict(os.environ, {"COLDARR_CODEQL_HOME": str(existing)}):
            with self.assertRaisesRegex(RuntimeError, "owner-only"):
                scan.state_directory()
        self.assertEqual(existing.stat().st_mode & 0o777, 0o755)

    def test_checksum_failure_does_not_install_bundle(self):
        state = self.base / "state"
        previous = self.metadata("1.0.0")
        self.installed(state, previous)
        metadata = self.metadata()

        def fake_download(url, destination):
            if url == scan.LATEST_RELEASE_URL:
                scan.write_json(destination, self.release(metadata))
            else:
                destination.write_bytes(b"corrupt download")

        with patch.object(scan, "download", side_effect=fake_download), patch.object(scan.platform, "machine", return_value="x86_64"):
            with self.assertRaisesRegex(RuntimeError, "checksum mismatch"):
                scan.install(state)
        self.assertFalse(scan.bundle(state, metadata["version"]).exists())
        self.assertEqual(scan.installed_bundle(state), previous)
        self.assertEqual(list(state.glob("install-*")), [])

    def test_latest_release_requires_stable_official_asset_and_checksum(self):
        metadata = self.metadata()
        release = self.release(metadata)
        invalid = [
            {**release, "tag_name": "v4.1.0"},
            {**release, "prerelease": True},
            {**release, "draft": True},
            {**release, "assets": []},
            {**release, "assets": [{**release["assets"][0], "digest": None}]},
            {**release, "assets": [{**release["assets"][0], "browser_download_url": "https://example.invalid/bundle"}]},
        ]
        for document in invalid:
            with self.subTest(document=document), patch.object(scan, "download", side_effect=lambda url, dest: scan.write_json(dest, document)):
                with self.assertRaises(RuntimeError):
                    scan.latest_bundle(self.base)

    def test_installer_rechecks_latest_even_with_an_existing_installation(self):
        state = self.base / "state"
        older = self.metadata("1.0.0")
        latest = self.metadata()
        self.installed(state, latest)
        self.installed(state, older)
        downloads = []

        def fake_download(url, destination):
            downloads.append(url)
            scan.write_json(destination, self.release(latest))

        with patch.object(scan, "download", side_effect=fake_download):
            scan.install(state)
        self.assertEqual(downloads, [scan.LATEST_RELEASE_URL])
        self.assertEqual(scan.installed_bundle(state), latest)

    @unittest.skipUnless(shutil.which("zstd"), "zstd is required for bundle extraction")
    def test_install_upgrades_using_release_checksum_and_preserves_previous_bundle(self):
        state = self.base / "state"
        previous = self.metadata("1.0.0")
        self.installed(state, previous)
        archive = io.BytesIO()
        with tarfile.open(fileobj=archive, mode="w") as tar:
            executable = b"#!/bin/sh\nexit 0\n"
            entry = tarfile.TarInfo("codeql/codeql")
            entry.size = len(executable)
            entry.mode = 0o755
            tar.addfile(entry, io.BytesIO(executable))
        compressed = subprocess.run(["zstd", "--compress", "--stdout"], input=archive.getvalue(), check=True, capture_output=True).stdout
        latest = self.metadata(content=compressed)

        def fake_download(url, destination):
            if url == scan.LATEST_RELEASE_URL:
                scan.write_json(destination, self.release(latest))
            else:
                self.assertEqual(url, latest["url"])
                destination.write_bytes(compressed)

        with patch.object(scan, "download", side_effect=fake_download):
            scan.install(state)
        self.assertEqual(scan.installed_bundle(state), latest)
        self.assertTrue((scan.bundle(state, previous["version"]) / "codeql").is_file())
        self.assertEqual((scan.bundle(state, latest["version"]) / "codeql").read_bytes(), executable)

    @unittest.skipUnless(shutil.which("zstd"), "zstd is required for bundle extraction")
    def test_install_rejects_archive_traversal_links_and_special_files(self):
        state = self.base / "state"
        previous = self.metadata("1.0.0")
        self.installed(state, previous)
        escaped = self.base / "escaped"
        traversal = tarfile.TarInfo("../../../escaped")
        symlink = tarfile.TarInfo("codeql/link")
        symlink.type = tarfile.SYMTYPE
        symlink.linkname = str(escaped)
        hardlink = tarfile.TarInfo("codeql/link")
        hardlink.type = tarfile.LNKTYPE
        hardlink.linkname = str(escaped)
        device = tarfile.TarInfo("codeql/device")
        device.type = tarfile.CHRTYPE
        for entry in (traversal, symlink, hardlink, device):
            with self.subTest(name=entry.name, type=entry.type):
                archive = io.BytesIO()
                with tarfile.open(fileobj=archive, mode="w") as tar:
                    tar.addfile(entry)
                compressed = subprocess.run(["zstd", "--compress", "--stdout"], input=archive.getvalue(), check=True, capture_output=True).stdout
                latest = self.metadata(content=compressed)

                def fake_download(url, destination):
                    if url == scan.LATEST_RELEASE_URL:
                        scan.write_json(destination, self.release(latest))
                    else:
                        destination.write_bytes(compressed)

                with patch.object(scan, "download", side_effect=fake_download):
                    with self.assertRaises(tarfile.FilterError):
                        scan.install(state)
                self.assertFalse(escaped.exists())
                self.assertFalse(scan.bundle(state, latest["version"]).exists())
                self.assertEqual(scan.installed_bundle(state), previous)
                self.assertEqual(list(state.glob("install-*")), [])

    def test_sarif_rejects_missing_or_failed_analysis(self):
        report = self.base / "report.sarif"
        for document in ({"version": "2.1.0", "runs": []}, {"version": "2.1.0", "runs": [
                {"invocations": [{"executionSuccessful": False}], "results": []}]}):
            scan.write_json(report, document)
            with self.assertRaises(RuntimeError):
                scan.summarize(report)

    def test_sarif_uses_rule_severity_when_result_omits_it(self):
        report = self.base / "report.sarif"
        scan.write_json(report, {"version": "2.1.0", "runs": [{
            "tool": {"driver": {"rules": [{"id": "sample/rule", "defaultConfiguration": {"level": "error"},
                "properties": {"security-severity": "8.1"}}]}},
            "results": [{"ruleId": "sample/rule", "message": {"text": "finding"}}],
        }]})
        summary = scan.summarize(report)
        self.assertEqual(summary["levels"], {"error": 1})
        self.assertEqual(summary["findings"][0]["security_severity"], "8.1")

    def test_sarif_counts_reviewed_suppressions_separately(self):
        report = self.base / "report.sarif"
        scan.write_json(report, {"version": "2.1.0", "runs": [{"results": [
            {"ruleId": "go/log-injection", "message": {"text": "active"}, "suppressions": []},
            {"ruleId": "go/log-injection", "message": {"text": "reviewed"}, "suppressions": [{"kind": "inSource"}]},
            {"ruleId": "go/log-injection", "message": {"text": "rejected"},
             "suppressions": [{"kind": "inSource", "status": "rejected"}]},
        ]}]})
        summary = scan.summarize(report)
        self.assertEqual((summary["count"], summary["suppressed"]), (2, 1))
        self.assertEqual([f["message"] for f in summary["findings"]], ["active", "rejected"])
        self.assertEqual([f["message"] for f in summary["suppressed_findings"]], ["reviewed"])

    def test_scan_reads_suppression_comments_and_gates_only_active_findings(self):
        state = self.base / "state"
        self.installed(state, self.metadata())
        commands = {}

        def fake_execute(state, work, label, command, version):
            commands[label] = command
            log = work / "logs" / f"{label}.log"
            log.parent.mkdir(exist_ok=True)
            log.write_text(json.dumps({"version": version}))
            if label.startswith("analyze-"):
                scan.write_json(work / "reports" / f"{label.removeprefix('analyze-')}.sarif", {"version": "2.1.0", "runs": [
                    {"results": [{"ruleId": "sample/rule", "suppressions": [{"kind": "inSource"}]}]}]})
            return log

        args = argparse.Namespace(command="scan", suite="security-extended", threads=1, ram=2048, fail_on_findings=True)
        with patch.object(scan, "execute", side_effect=fake_execute):
            self.assertEqual(scan.scan(state, args), 0)
        for language in scan.LANGUAGES:
            query = f"codeql/{language}-queries:AlertSuppression.ql"
            self.assertIn(query, commands[f"resolve-{language}"])
            self.assertIn(query, commands[f"analyze-{language}"])
        summary = json.loads(next(state.glob("projects/*/runs/*/summary.json")).read_text())
        self.assertEqual({language: (r["count"], r["suppressed"]) for language, r in summary.items()},
                         {"go": (0, 1), "javascript": (0, 1)})

    def test_failed_second_language_never_publishes_complete_summary(self):
        state = self.base / "state"
        self.installed(state, self.metadata())
        project = state / "projects" / scan.hashlib.sha256(os.fsencode(self.root)).hexdigest()[:12]
        project.mkdir(parents=True)
        scan.write_json(project / "latest.json", {"run": "old-success"})

        def fake_execute(state, work, label, command, version):
            if label == "analyze-go":
                scan.write_json(work / "reports/go.sarif", {"version": "2.1.0", "runs": [{"results": []}]})
            if label == "analyze-javascript":
                raise RuntimeError("intentional analysis failure")

        args = argparse.Namespace(command="scan", suite="security-extended", threads=1, ram=2048, fail_on_findings=False)
        with patch.object(scan, "preflight"), patch.object(scan, "execute", side_effect=fake_execute):
            with self.assertRaisesRegex(RuntimeError, "intentional"):
                scan.scan(state, args)
        work = next((project / "runs").iterdir())
        self.assertFalse((work / "summary.json").exists())
        self.assertEqual(json.loads((work / "manifest.json").read_text())["status"], "failed")
        self.assertEqual(json.loads((project / "latest.json").read_text()), {"run": "old-success"})

    def test_scan_keeps_one_bundle_version_if_the_installer_updates_selection(self):
        state = self.base / "state"
        initial = self.metadata("1.0.0")
        self.installed(state, initial)
        versions = []

        def fake_execute(state, work, label, command, version):
            versions.append(version)
            if label.startswith("analyze-"):
                scan.write_json(work / "reports" / f"{label.removeprefix('analyze-')}.sarif",
                                {"version": "2.1.0", "runs": [{"results": []}]})

        def upgrade_during_scan(*args):
            self.installed(state, self.metadata())

        args = argparse.Namespace(command="scan", suite="security-extended", threads=1, ram=2048, fail_on_findings=False)
        with patch.object(scan, "preflight", side_effect=upgrade_during_scan), patch.object(scan, "execute", side_effect=fake_execute), patch.object(scan, "download", side_effect=AssertionError("scan attempted network access")):
            self.assertEqual(scan.scan(state, args), 0)
        self.assertEqual(versions, [initial["version"]] * 4)
        manifest = json.loads(next(state.glob("projects/*/runs/*/manifest.json")).read_text())
        self.assertEqual(manifest["codeql"], initial["version"])
        self.assertEqual(manifest["bundle"], initial)

    # Some hosts, such as Ubuntu 24.04 GitHub runners, block the namespaces bwrap needs.
    @unittest.skipUnless(sandbox_available(), "bubblewrap can't create isolated namespaces on this host")
    def test_real_sandbox_blocks_network_credentials_and_source_writes(self):
        state = self.base / "state"
        metadata = self.metadata()
        self.installed(state, metadata)
        work = self.base / "work"
        (work / "source").mkdir(parents=True)
        (work / "source/readonly.txt").write_text("original")
        cache = self.base / "gomodcache"
        cache.mkdir()
        # Model setup-go's toolchain outside /usr, which must be mounted explicitly.
        goroot = self.base / "hostedtoolcache/go"
        (goroot / "bin").mkdir(parents=True)
        (goroot / "bin/go").write_text("#!/bin/sh\nprintf 'hosted Go works\\n'\n")
        (goroot / "bin/go").chmod(0o700)
        probe = """
import errno, os, socket, subprocess
from pathlib import Path
assert 'GITHUB_TOKEN' not in os.environ
assert os.environ['GOROOT'] == '/tools/go'
assert subprocess.check_output(['go'], text=True) == 'hosted Go works\\n'
assert not Path('/run/docker.sock').exists()
assert not list(Path('/home').glob('*/.ssh')) if Path('/home').exists() else True
s = socket.socket()
s.settimeout(2)
assert s.connect_ex(('1.1.1.1', 443)) in (errno.ENETUNREACH, errno.EHOSTUNREACH)
for path in ('/src/readonly.txt', '/work/source/readonly.txt'):
    try:
        Path(path).write_text('changed')
    except OSError as error:
        assert error.errno == errno.EROFS
    else:
        raise AssertionError('source was writable')
Path('/work/result').write_text('ok')
"""
        with patch.object(scan, "run", return_value=argparse.Namespace(stdout=json.dumps({"GOMODCACHE": str(cache), "GOROOT": str(goroot)}))):
            command = scan.sandbox(state, work, ["python3", "-c", probe], metadata["version"])
        result = subprocess.run(command, capture_output=True, text=True, env={**os.environ, "GITHUB_TOKEN": "test-token"})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((work / "result").read_text(), "ok")
        self.assertEqual((work / "source/readonly.txt").read_text(), "original")


if __name__ == "__main__":
    unittest.main()
