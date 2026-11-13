from __future__ import annotations

import hashlib
import json
import os
import shutil
import stat
import subprocess
import tarfile
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
PACKAGING = ROOT / "packaging"


def run(*argv: str, cwd: Path | None = None, env: dict[str, str] | None = None) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        argv,
        cwd=cwd or ROOT,
        env=env,
        check=True,
        capture_output=True,
        text=True,
        timeout=30,
    )


class PackagingContractTests(unittest.TestCase):
    def test_candidate_and_release_builds_disable_ambient_vcs_embedding(self) -> None:
        self.assertIn("-trimpath -buildvcs=false", (ROOT / "Makefile").read_text())
        release_builder = (PACKAGING / "release/build-unsigned-release.py").read_text()
        self.assertIn('"-trimpath", "-buildvcs=false"', release_builder)
        candidate_builder = (PACKAGING / "release/prepare-local-candidate.sh").read_text()
        self.assertIn("-trimpath -buildvcs=false", candidate_builder)

    def test_docs_match_predeployment_fail_closed_entrypoint(self) -> None:
        entrypoint = (ROOT / "cmd/run_ai/main.go").read_text()
        self.assertIn(
            "request.Workflow.Kind == workflows.KindMonitoring || request.Workflow.Kind == workflows.KindSpam",
            entrypoint,
        )
        self.assertIn("workflow is plan-only in this build", entrypoint)
        self.assertIn("--readonly launch is blocked", entrypoint)
        self.assertIn("--confirm launch is blocked", entrypoint)

        architecture = (ROOT / "docs/architecture.md").read_text()
        security = (ROOT / "docs/security-model.md").read_text()
        migration = (ROOT / "docs/migration.md").read_text()
        install = (ROOT / "docs/install.md").read_text()
        distribution = (ROOT / "docs/distribution.md").read_text()
        for text in (architecture, security, migration):
            self.assertIn("fail-closed", text)
        for doc_name in ("persistent-codex.md", "persistent-candidate-cutover.md", "upgrade-rollback.md", "release.md"):
            self.assertTrue((ROOT / "docs" / doc_name).is_file(), f"missing docs/{doc_name}")
        _ = install
        _ = distribution

    def test_maintainer_scripts_are_inert_and_dependency_set_is_exact(self) -> None:
        control = (PACKAGING / "debian/control.in").read_text()
        self.assertIn("Package: run-ai", control)
        self.assertIn("License: MIT", (PACKAGING / "debian/copyright").read_text())
        self.assertIn("MIT License", (ROOT / "LICENSE").read_text())
        self.assertIn(
            "Depends: ca-certificates, curl, tmux, openssh-client, rsync, bubblewrap",
            control,
        )
        forbidden = (
            "systemctl", "useradd", "adduser", "groupadd", "/var/log/run-ai",
            "/root/", "tmux kill", "enable --now",
        )
        for name in ("preinst", "postinst", "prerm", "postrm"):
            text = (PACKAGING / f"debian/{name}").read_text()
            for marker in forbidden:
                self.assertNotIn(marker, text, f"{name} contains {marker}")
            run("sh", "-n", str(PACKAGING / f"debian/{name}"))

    def test_wsl_detector_is_pure_and_distinguishes_versions(self) -> None:
        detector = PACKAGING / "wsl/detect-wsl.sh"
        cases = {
            "5.15.90.1-microsoft-standard-WSL2": "wsl2",
            "4.4.0-19041-Microsoft": "wsl1",
            "6.8.0-generic": "not-wsl",
        }
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            for index, (release, expected) in enumerate(cases.items()):
                probe = root / str(index)
                probe.write_text(release + "\n")
                env = os.environ.copy()
                env["RUN_AI_OSRELEASE_FILE"] = str(probe)
                env.pop("WSL_INTEROP", None)
                env.pop("WSL_DISTRO_NAME", None)
                self.assertEqual(run("sh", str(detector), env=env).stdout.strip(), expected)
        profile = json.loads((PACKAGING / "wsl/wsl.json").read_text())
        self.assertEqual(profile["schema"], "run-ai-platform-profile/v1")
        self.assertIs(profile["automatic_mutation"], False)

    @unittest.skipUnless(shutil.which("dpkg-deb"), "dpkg-deb is not installed")
    def test_fresh_deb_contains_exact_cli_and_no_service(self) -> None:
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            binary = root / "run_ai"
            binary.write_text("#!/bin/sh\necho run_ai\n")
            binary.chmod(0o755)
            output = root / "out"
            second_output = root / "out-second"
            env = os.environ.copy()
            env["SOURCE_DATE_EPOCH"] = "1791979200"
            result = run(
                "sh", str(PACKAGING / "debian/build-deb.sh"),
                "1.2.3", "amd64", str(binary), str(output), env=env,
            )
            package = Path(result.stdout.strip())
            self.assertTrue(package.is_file())
            second = Path(run(
                "sh", str(PACKAGING / "debian/build-deb.sh"),
                "1.2.3", "amd64", str(binary), str(second_output), env=env,
            ).stdout.strip())
            self.assertEqual(package.read_bytes(), second.read_bytes())
            listing = run("dpkg-deb", "--contents", str(package)).stdout
            self.assertIn("./usr/bin/run_ai", listing)
            self.assertIn("./etc/run-ai/config.json", listing)
            self.assertIn("./usr/share/run-ai/profiles.d/wsl.json", listing)
            self.assertIn("./usr/share/run-ai/schemas/run-ai-platform-profile-v1.schema.json", listing)
            self.assertIn("./usr/share/doc/run-ai/persistent-codex.md", listing)
            self.assertIn("./usr/share/doc/run-ai/persistent-candidate-cutover.md", listing)
            self.assertNotIn("systemd", listing)
            fields = run("dpkg-deb", "--field", str(package)).stdout
            self.assertIn("Package: run-ai", fields)
            self.assertIn("Architecture: amd64", fields)
            for dependency in ("ca-certificates", "curl", "tmux", "openssh-client", "rsync", "bubblewrap"):
                self.assertIn(dependency, fields)

    def test_archive_is_reproducible_and_contains_only_run_ai(self) -> None:
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            binary = root / "source-run_ai"
            binary.write_bytes(b"#!/bin/sh\necho run_ai\n")
            binary.chmod(0o755)
            env = os.environ.copy()
            env["SOURCE_DATE_EPOCH"] = "1791979200"
            script = PACKAGING / "archive/build-archive.sh"
            first = Path(run("sh", str(script), "1.2.3", "darwin", "arm64", str(binary), str(root / "first"), env=env).stdout.strip())
            second = Path(run("sh", str(script), "1.2.3", "darwin", "arm64", str(binary), str(root / "second"), env=env).stdout.strip())
            self.assertEqual(first.read_bytes(), second.read_bytes())
            self.assertEqual(run("tar", "-tzf", str(first)).stdout.splitlines(), ["LICENSE", "run_ai"])

    def test_local_candidate_is_digest_bound_and_refuses_live_path(self) -> None:
        script = PACKAGING / "release/prepare-local-candidate.sh"
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            binary = root / "run_ai"
            run("go", "build", "-trimpath", "-buildvcs=false", "-o", str(binary), "./cmd/run_ai")
            target = root / "candidate"
            result = run("sh", str(script), str(binary), str(target))
            self.assertEqual(Path(result.stdout.strip()), target)
            metadata = json.loads((target / "candidate.json").read_text())
            self.assertEqual(metadata["schema"], "run-ai-local-candidate/v1")
            self.assertFalse(metadata["release_eligible"])
            digest = hashlib.sha256((target / "run_ai").read_bytes()).hexdigest()
            self.assertEqual(metadata["candidate_sha256"], digest)
            self.assertEqual((target / "SHA256SUMS").read_text(), f"{digest}  run_ai\n")
            source_manifest = json.loads((target / "source-state.json").read_text())
            archive = target / "source-state.tar.gz"
            self.assertEqual(source_manifest["schema"], "run-ai-source-state/v1")
            self.assertEqual(source_manifest["archive_sha256"], hashlib.sha256(archive.read_bytes()).hexdigest())
            self.assertEqual(metadata["source_state_sha256"], hashlib.sha256((target / "source-state.json").read_bytes()).hexdigest())
            self.assertEqual(metadata["source_archive_sha256"], source_manifest["archive_sha256"])
            self.assertEqual(metadata["source_rebuild_sha256"], digest)
            extracted = root / "extracted-source"
            extracted.mkdir()
            with tarfile.open(archive) as input_archive:
                input_archive.extractall(extracted, filter="data")
            run("python3", str(PACKAGING / "release/source-state-manifest.py"), "--verify", str(target / "source-state.json"), "--archive", str(archive), "--source", str(extracted))
            repeated = subprocess.run(
                ["sh", str(script), str(binary), str(target)], cwd=ROOT,
                capture_output=True, text=True, timeout=30,
            )
            self.assertNotEqual(repeated.returncode, 0)
            live = subprocess.run(
                ["sh", str(script), str(binary), "/usr/bin/run-ai-test-candidate"], cwd=ROOT,
                capture_output=True, text=True, timeout=30,
            )
            self.assertEqual(live.returncode, 77)


class ReleaseToolingTests(unittest.TestCase):
    def _spdx(self, path: Path, artifact: Path) -> None:
        name = artifact.name
        path.write_text(json.dumps({
            "spdxVersion": "SPDX-2.3",
            "dataLicense": "CC0-1.0",
            "SPDXID": "SPDXRef-DOCUMENT",
            "name": name,
            "documentNamespace": f"https://example.invalid/{name}",
            "creationInfo": {"created": "2026-10-14T00:00:00Z", "creators": ["Tool: test"]},
            "packages": [{
                "name": "run-ai", "SPDXID": "SPDXRef-Package-run-ai",
                "versionInfo": "1.2.3", "downloadLocation": "NOASSERTION",
                "filesAnalyzed": False,
                "licenseConcluded": "MIT",
                "licenseDeclared": "MIT",
                "copyrightText": "Copyright (c) 2026 Václav Ambrož",
                "checksums": [{
                    "algorithm": "SHA256",
                    "checksumValue": hashlib.sha256(artifact.read_bytes()).hexdigest(),
                }],
            }],
            "relationships": [],
        }, sort_keys=True) + "\n")

    def test_complete_unsigned_release_and_formula_are_deterministic(self) -> None:
        matrix = [
            ("archive", "linux", "amd64", "tar.gz"),
            ("archive", "linux", "arm64", "tar.gz"),
            ("archive", "darwin", "amd64", "tar.gz"),
            ("archive", "darwin", "arm64", "tar.gz"),
            ("deb", "linux", "amd64", "deb"),
            ("deb", "linux", "arm64", "deb"),
        ]
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            entries = []
            expected_darwin: dict[str, str] = {}
            for kind, operating_system, arch, suffix in matrix:
                artifact_name = (
                    f"run-ai_1.2.3_{arch}.deb" if kind == "deb"
                    else f"run-ai_1.2.3_{operating_system}_{arch}.{suffix}"
                )
                artifact = root / artifact_name
                artifact.write_bytes(f"{kind}:{operating_system}:{arch}\n".encode())
                sbom_name = f"run-ai_1.2.3_{operating_system}_{arch}.{kind}.spdx.json"
                sbom = root / sbom_name
                self._spdx(sbom, artifact)
                entries.append({
                    "file": artifact_name,
                    "path": artifact_name,
                    "kind": kind,
                    "os": operating_system,
                    "arch": arch,
                    "sbom_file": sbom_name,
                    "sbom_path": sbom_name,
                })
                if kind == "archive" and operating_system == "darwin":
                    expected_darwin[arch] = hashlib.sha256(artifact.read_bytes()).hexdigest()
            spec = root / "input.json"
            spec.write_text(json.dumps({
                "schema": "run-ai-release-input/v1",
                "version": "1.2.3",
                "channel": "candidate",
                "source_commit": "a" * 40,
                "source_date_epoch": 1791979200,
                "key_id": "run-ai-release-2026",
                "build": {
                    "git_tag": "v1.2.3",
                    "go_toolchain": "go version go1.24.4 linux/amd64",
                    "host_os": "linux",
                    "host_arch": "amd64",
                },
                "artifacts": entries,
            }))
            manifest = root / "manifest.json"
            generator = PACKAGING / "release/generate-manifest.py"
            run("python3", str(generator), str(spec), str(manifest))
            first = manifest.read_bytes()
            run("python3", str(generator), str(spec), str(manifest))
            self.assertEqual(first, manifest.read_bytes())
            self.assertEqual(json.loads(first)["build"], {
                "git_tag": "v1.2.3",
                "go_toolchain": "go version go1.24.4 linux/amd64",
                "host_os": "linux",
                "host_arch": "amd64",
            })
            run(
                "python3", str(PACKAGING / "release/validate-release.py"),
                str(manifest), "--artifact-root", str(root), "--require-platforms",
            )
            invalid_spec = json.loads(spec.read_text())
            invalid_spec["build"]["host_os"] = 1
            spec.write_text(json.dumps(invalid_spec))
            invalid = subprocess.run(
                ["python3", str(generator), str(spec), str(manifest)],
                capture_output=True, text=True,
            )
            self.assertNotEqual(invalid.returncode, 0)
            formula = root / "run-ai.rb"
            run(
                "python3", str(PACKAGING / "homebrew/render-formula.py"),
                str(manifest), str(formula),
            )
            rendered = formula.read_text()
            self.assertNotIn("@VERSION@", rendered)
            self.assertIn(expected_darwin["amd64"], rendered)
            self.assertIn(expected_darwin["arm64"], rendered)
            self.assertIn('bin.install "run_ai"', rendered)
            self.assertIn('license "MIT"', rendered)
            self.assertIn('doc.install "LICENSE"', rendered)
            self.assertIn('depends_on "tmux"', rendered)
            self.assertIn('depends_on "rsync"', rendered)
            self.assertIn("depends_on :macos", rendered)

    def test_sbom_generation_needs_no_signing_key(self) -> None:
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            binary = root / "run_ai"
            binary.write_bytes(b"not-a-go-binary-but-release-shaped\n")
            binary.chmod(0o755)
            output = root / "sbom.spdx.json"
            run(
                "python3", str(PACKAGING / "release/generate-sbom.py"),
                str(binary), str(output), "--version", "1.2.3",
                "--commit", "b" * 40, "--source-date-epoch", "1791979200",
            )
            document = json.loads(output.read_text())
            self.assertEqual(document["spdxVersion"], "SPDX-2.3")
            self.assertEqual(document["packages"][0]["name"], "run-ai")
            self.assertEqual(
                document["packages"][0]["checksums"][0]["checksumValue"],
                hashlib.sha256(binary.read_bytes()).hexdigest(),
            )

    def test_release_validator_rejects_digest_tamper(self) -> None:
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            artifact = root / "run-ai_1.2.3_darwin_amd64.tar.gz"
            artifact.write_bytes(b"artifact")
            sbom = root / "run-ai_1.2.3_darwin_amd64.archive.spdx.json"
            self._spdx(sbom, artifact)
            manifest = {
                "schema": "run-ai-release/v1", "project": "run-ai", "version": "1.2.3",
                "channel": "candidate", "source_commit": "c" * 40,
                "source_date_epoch": 1791979200, "generated_at": "2026-10-14T12:00:00Z",
                "artifacts": [{
                    "path": artifact.name, "kind": "archive", "os": "darwin", "arch": "amd64",
                    "size": len(artifact.read_bytes()), "sha256": "0" * 64,
                    "sbom": {"path": sbom.name, "format": "spdx-json-2.3", "sha256": hashlib.sha256(sbom.read_bytes()).hexdigest()},
                }],
                "signing": {"algorithm": "minisign", "key_id": "run-ai-release-2026", "signature_path": "manifest.json.minisig"},
            }
            manifest_path = root / "manifest.json"
            manifest_path.write_text(json.dumps(manifest))
            result = subprocess.run(
                ["python3", str(PACKAGING / "release/validate-release.py"), str(manifest_path), "--artifact-root", str(root)],
                capture_output=True,
                text=True,
            )
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("do not match", result.stderr)

    def test_manifest_generator_rejects_symlink_artifact(self) -> None:
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            real = root / "real.tar.gz"
            real.write_bytes(b"artifact")
            linked = root / "run-ai_1.2.3_linux_amd64.tar.gz"
            linked.symlink_to(real.name)
            sbom = root / "run-ai_1.2.3_linux_amd64.archive.spdx.json"
            self._spdx(sbom, linked)
            spec = root / "input.json"
            spec.write_text(json.dumps({
                "schema": "run-ai-release-input/v1", "version": "1.2.3",
                "channel": "candidate", "source_commit": "d" * 40,
                "source_date_epoch": 1791979200, "key_id": "run-ai-release-2026",
                "artifacts": [{
                    "file": linked.name, "path": linked.name, "kind": "archive",
                    "os": "linux", "arch": "amd64", "sbom_file": sbom.name,
                    "sbom_path": sbom.name,
                }],
            }))
            result = subprocess.run(
                ["python3", str(PACKAGING / "release/generate-manifest.py"), str(spec), str(root / "manifest.json")],
                capture_output=True,
                text=True,
            )
            self.assertNotEqual(result.returncode, 0)


if __name__ == "__main__":
    unittest.main()
