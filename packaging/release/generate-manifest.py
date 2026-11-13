#!/usr/bin/env python3
"""Hash already-built artifacts and SBOMs into a deterministic manifest."""
from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
import re
import stat
from pathlib import Path, PurePosixPath


VERSION = re.compile(r"^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?$")
COMMIT = re.compile(r"^[0-9a-f]{40,64}$")
KEY_ID = re.compile(r"^[A-Za-z0-9._:-]{8,128}$")
INPUT_KEYS = {"schema", "version", "channel", "source_commit", "source_date_epoch", "key_id", "artifacts"}
ARTIFACT_INPUT_KEYS = {"file", "path", "kind", "os", "arch", "sbom_file", "sbom_path"}
BUILD_KEYS = {"git_tag", "go_toolchain", "host_os", "host_arch"}
TAG = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$")
HOST = re.compile(r"^[a-z0-9]+$")
ARCH = re.compile(r"^[a-z0-9_]+$")


def hash_file(path: Path) -> tuple[int, str]:
    digest = hashlib.sha256()
    size = 0
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | getattr(os, "O_CLOEXEC", 0))
    try:
        before = os.fstat(descriptor)
        if not stat.S_ISREG(before.st_mode) or before.st_size < 1:
            raise ValueError(f"not a non-empty regular release file: {path}")
        while True:
            chunk = os.read(descriptor, 1024 * 1024)
            if not chunk:
                break
            size += len(chunk)
            digest.update(chunk)
        after = os.fstat(descriptor)
        identity = lambda value: (value.st_dev, value.st_ino, value.st_mode, value.st_uid, value.st_gid, value.st_size, value.st_mtime_ns)
        if size != before.st_size or identity(before) != identity(after):
            raise ValueError(f"release file changed while hashing: {path}")
    finally:
        os.close(descriptor)
    return size, digest.hexdigest()


def read_small_regular(path: Path, limit: int = 16 * 1024 * 1024) -> bytes:
    size, expected = hash_file(path)
    if size > limit:
        raise ValueError(f"release metadata exceeds size limit: {path}")
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | getattr(os, "O_CLOEXEC", 0))
    try:
        data = b""
        while len(data) <= limit:
            chunk = os.read(descriptor, min(65536, limit + 1 - len(data)))
            if not chunk:
                break
            data += chunk
    finally:
        os.close(descriptor)
    if len(data) != size or hashlib.sha256(data).hexdigest() != expected:
        raise ValueError(f"release metadata changed while reading: {path}")
    return data


def safe_publish_path(raw: str) -> str:
    path = PurePosixPath(raw)
    if path.is_absolute() or ".." in path.parts or str(path) in {"", "."}:
        raise ValueError(f"unsafe publish path: {raw}")
    return str(path)


def build_metadata(value: object) -> dict[str, str] | None:
    if value is None:
        return None
    if not isinstance(value, dict) or not {"go_toolchain", "host_os", "host_arch"}.issubset(value) or not set(value).issubset(BUILD_KEYS):
        raise ValueError("invalid build metadata keys")
    if any(not isinstance(item, str) for item in value.values()):
        raise ValueError("build metadata values must be strings")
    result = dict(value)
    if not result["go_toolchain"] or len(result["go_toolchain"]) > 256 or not HOST.fullmatch(result["host_os"]) or not ARCH.fullmatch(result["host_arch"]):
        raise ValueError("invalid build metadata")
    if "git_tag" in result and not TAG.fullmatch(result["git_tag"]):
        raise ValueError("invalid build git tag")
    return result


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("input", type=Path)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    spec = json.loads(read_small_regular(args.input, 1024 * 1024))
    version = str(spec.get("version", ""))
    commit = str(spec.get("source_commit", ""))
    epoch = spec.get("source_date_epoch")
    if (set(spec) != INPUT_KEYS and set(spec) != INPUT_KEYS | {"build"}) or spec.get("schema") != "run-ai-release-input/v1":
        raise ValueError("invalid release input schema")
    if not VERSION.fullmatch(version) or not COMMIT.fullmatch(commit):
        raise ValueError("invalid version or source commit")
    if type(epoch) is not int or epoch < 1:
        raise ValueError("invalid source_date_epoch")
    if spec["channel"] not in {"stable", "candidate"} or not KEY_ID.fullmatch(str(spec["key_id"])):
        raise ValueError("invalid release channel or key id")
    build = build_metadata(spec.get("build"))
    artifacts = []
    seen: set[str] = set()
    base = args.input.parent
    for item in spec.get("artifacts", []):
        if not isinstance(item, dict) or set(item) != ARTIFACT_INPUT_KEYS:
            raise ValueError("artifact input keys are not exact")
        if item["kind"] not in {"archive", "deb"} or item["os"] not in {"linux", "darwin"} or item["arch"] not in {"amd64", "arm64"}:
            raise ValueError("unsupported artifact platform")
        if item["kind"] == "deb" and item["os"] != "linux":
            raise ValueError("Debian artifacts must target linux")
        artifact_path = safe_publish_path(str(item["path"]))
        sbom_path = safe_publish_path(str(item["sbom_path"]))
        expected_artifact = (
            f"run-ai_{version}_{item['arch']}.deb" if item["kind"] == "deb"
            else f"run-ai_{version}_{item['os']}_{item['arch']}.tar.gz"
        )
        expected_sbom = f"run-ai_{version}_{item['os']}_{item['arch']}.{item['kind']}.spdx.json"
        if artifact_path != expected_artifact or sbom_path != expected_sbom:
            raise ValueError("artifact or SBOM publish path is not canonical")
        if artifact_path in seen or sbom_path in seen:
            raise ValueError("duplicate published path")
        seen.update((artifact_path, sbom_path))
        artifact_file = base / safe_publish_path(str(item["file"]))
        sbom_file = base / safe_publish_path(str(item["sbom_file"]))
        size, artifact_sha = hash_file(artifact_file)
        sbom_data = read_small_regular(sbom_file)
        sbom_sha = hashlib.sha256(sbom_data).hexdigest()
        sbom = json.loads(sbom_data)
        if sbom.get("spdxVersion") != "SPDX-2.3":
            raise ValueError(f"not an SPDX 2.3 SBOM: {sbom_file}")
        artifacts.append({
            "path": artifact_path,
            "kind": item["kind"],
            "os": item["os"],
            "arch": item["arch"],
            "size": size,
            "sha256": artifact_sha,
            "sbom": {"path": sbom_path, "format": "spdx-json-2.3", "sha256": sbom_sha},
        })
    if not artifacts:
        raise ValueError("release has no artifacts")
    manifest = {
        "schema": "run-ai-release/v1",
        "project": "run-ai",
        "version": version,
        "channel": spec.get("channel", "candidate"),
        "source_commit": commit,
        "source_date_epoch": epoch,
        "generated_at": dt.datetime.fromtimestamp(epoch, tz=dt.timezone.utc).isoformat().replace("+00:00", "Z"),
        "artifacts": sorted(artifacts, key=lambda value: value["path"]),
        "signing": {
            "algorithm": "minisign",
            "key_id": spec["key_id"],
            "signature_path": "manifest.json.minisig",
        },
    }
    if build is not None:
        manifest["build"] = build
    if args.output.is_symlink():
        raise ValueError("manifest output must not be a symlink")
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(
        json.dumps(manifest, sort_keys=True, separators=(",", ":")) + "\n",
        encoding="utf-8",
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
