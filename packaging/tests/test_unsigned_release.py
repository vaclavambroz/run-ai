from __future__ import annotations

import json
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]


@unittest.skipUnless(
    all(shutil.which(item) for item in ("go", "tar", "gzip", "dpkg-deb")),
    "complete release build tools are not installed",
)
class UnsignedReleaseTests(unittest.TestCase):
    def test_actual_six_artifact_matrix_builds_without_signing_key(self) -> None:
        with tempfile.TemporaryDirectory() as raw:
            output = Path(raw) / "release"
            subprocess.run([
                "python3", str(ROOT / "packaging/release/build-unsigned-release.py"),
                "--version", "0.1.0-test", "--commit", "e" * 40,
                "--source-date-epoch", "1791979200", "--output", str(output),
                "--development-allow-dirty",
            ], cwd=ROOT, check=True, capture_output=True, text=True, timeout=180)
            manifest = json.loads((output / "manifest.json").read_text())
            self.assertEqual(set(manifest["build"]), {"go_toolchain", "host_os", "host_arch"})
            self.assertTrue(manifest["build"]["go_toolchain"].startswith("go version go"))
            self.assertTrue(manifest["build"]["host_os"])
            self.assertTrue(manifest["build"]["host_arch"])
            self.assertEqual(len(manifest["artifacts"]), 6)
            self.assertEqual(
                {(item["kind"], item["os"], item["arch"]) for item in manifest["artifacts"]},
                {
                    ("archive", "linux", "amd64"), ("archive", "linux", "arm64"),
                    ("archive", "darwin", "amd64"), ("archive", "darwin", "arm64"),
                    ("deb", "linux", "amd64"), ("deb", "linux", "arm64"),
                },
            )
            self.assertTrue((output / "run-ai.rb").is_file())
            self.assertFalse((output / "manifest.json.minisig").exists())
            if shutil.which("ruby"):
                subprocess.run(["ruby", "-c", str(output / "run-ai.rb")], check=True, capture_output=True, text=True)
