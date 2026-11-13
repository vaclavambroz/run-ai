#!/usr/bin/env python3
"""Render a Homebrew formula from a validated run-ai release manifest."""
from __future__ import annotations

import argparse
import json
import os
import re
import stat
from pathlib import Path


SHA256 = re.compile(r"^[0-9a-f]{64}$")
VERSION = re.compile(r"^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?$")


def read_manifest(path: Path) -> dict:
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | getattr(os, "O_CLOEXEC", 0))
    try:
        info = os.fstat(descriptor)
        if not stat.S_ISREG(info.st_mode) or info.st_size < 1 or info.st_size > 1024 * 1024:
            raise ValueError("manifest must be a bounded regular file")
        data = b""
        while len(data) <= 1024 * 1024:
            chunk = os.read(descriptor, min(65536, 1024 * 1024 + 1 - len(data)))
            if not chunk:
                break
            data += chunk
    finally:
        os.close(descriptor)
    if len(data) != info.st_size:
        raise ValueError("manifest changed while reading")
    return json.loads(data)


def artifact_sha(manifest: dict, arch: str) -> str:
    matches = [
        item for item in manifest.get("artifacts", [])
        if item.get("kind") == "archive"
        and item.get("os") == "darwin"
        and item.get("arch") == arch
    ]
    if len(matches) != 1 or not SHA256.fullmatch(str(matches[0].get("sha256", ""))):
        raise ValueError(f"manifest must contain one darwin/{arch} archive")
    expected = f"run-ai_{manifest['version']}_darwin_{arch}.tar.gz"
    if matches[0].get("path") != expected:
        raise ValueError(f"unexpected darwin/{arch} artifact path")
    return matches[0]["sha256"]


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("manifest", type=Path)
    parser.add_argument("output", type=Path)
    parser.add_argument(
        "--template",
        type=Path,
        default=Path(__file__).with_name("Formula") / "run-ai.rb.in",
    )
    args = parser.parse_args()
    manifest = read_manifest(args.manifest)
    version = str(manifest.get("version", ""))
    if manifest.get("schema") != "run-ai-release/v1" or not VERSION.fullmatch(version):
        raise ValueError("invalid run-ai release manifest")
    replacements = {
        "@VERSION@": version,
        "@DARWIN_AMD64_SHA256@": artifact_sha(manifest, "amd64"),
        "@DARWIN_ARM64_SHA256@": artifact_sha(manifest, "arm64"),
    }
    rendered = args.template.read_text(encoding="utf-8")
    for marker, value in replacements.items():
        rendered = rendered.replace(marker, value)
    if re.search(r"@[A-Z0-9_]+@", rendered):
        raise ValueError("unresolved formula marker")
    if args.output.is_symlink():
        raise ValueError("formula output must not be a symlink")
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(rendered, encoding="utf-8")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
