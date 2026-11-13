#!/usr/bin/env python3
"""Offline structural and digest validation; signing keys are not required."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import stat
import datetime as dt
from pathlib import Path, PurePosixPath


SHA = re.compile(r"^[0-9a-f]{64}$")
VERSION = re.compile(r"^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?$")
ALLOWED_ARTIFACT_KEYS = {"path", "kind", "os", "arch", "size", "sha256", "sbom"}
BUILD_KEYS = {"git_tag", "go_toolchain", "host_os", "host_arch"}
TAG = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$")
HOST = re.compile(r"^[a-z0-9]+$")
ARCH = re.compile(r"^[a-z0-9_]+$")


def safe_path(raw: object) -> str:
    if not isinstance(raw, str):
        raise ValueError("published path is not a string")
    value = PurePosixPath(raw)
    if value.is_absolute() or ".." in value.parts or str(value) in {"", "."}:
        raise ValueError(f"unsafe published path: {raw}")
    return str(value)


def digest(path: Path) -> tuple[int, str]:
    value = hashlib.sha256()
    size = 0
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | getattr(os, "O_CLOEXEC", 0))
    try:
        before = os.fstat(descriptor)
        if not stat.S_ISREG(before.st_mode) or before.st_size < 1:
            raise ValueError(f"not a non-empty regular published file: {path}")
        while True:
            chunk = os.read(descriptor, 1024 * 1024)
            if not chunk:
                break
            size += len(chunk)
            value.update(chunk)
        after = os.fstat(descriptor)
        identity = lambda item: (item.st_dev, item.st_ino, item.st_mode, item.st_uid, item.st_gid, item.st_size, item.st_mtime_ns)
        if size != before.st_size or identity(before) != identity(after):
            raise ValueError(f"published file changed while hashing: {path}")
    finally:
        os.close(descriptor)
    return size, value.hexdigest()


def published_file(root: Path, relative: str) -> Path:
    current = root
    root_info = current.lstat()
    if current.is_symlink() or not stat.S_ISDIR(root_info.st_mode):
        raise ValueError("artifact root is not a real directory")
    parts = PurePosixPath(relative).parts
    for part in parts[:-1]:
        current = current / part
        info = current.lstat()
        if current.is_symlink() or not stat.S_ISDIR(info.st_mode):
            raise ValueError(f"published parent is not a real directory: {current}")
    return current / parts[-1]


def read_small_verified(path: Path, maximum: int = 16 * 1024 * 1024) -> bytes:
    size, expected = digest(path)
    if size > maximum:
        raise ValueError(f"published metadata exceeds size limit: {path}")
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | getattr(os, "O_CLOEXEC", 0))
    try:
        chunks: list[bytes] = []
        total = 0
        while True:
            chunk = os.read(descriptor, min(65536, maximum + 1 - total))
            if not chunk:
                break
            chunks.append(chunk)
            total += len(chunk)
            if total > maximum:
                raise ValueError(f"published metadata exceeds size limit: {path}")
    finally:
        os.close(descriptor)
    data = b"".join(chunks)
    if len(data) != size or hashlib.sha256(data).hexdigest() != expected:
        raise ValueError(f"published metadata changed while reading: {path}")
    return data


def validate_build(value: object) -> None:
    if not isinstance(value, dict) or not {"go_toolchain", "host_os", "host_arch"}.issubset(value) or not set(value).issubset(BUILD_KEYS):
        raise ValueError("invalid build metadata keys")
    if not isinstance(value["go_toolchain"], str) or not value["go_toolchain"] or len(value["go_toolchain"]) > 256:
        raise ValueError("invalid Go toolchain metadata")
    if not isinstance(value["host_os"], str) or not HOST.fullmatch(value["host_os"]) or not isinstance(value["host_arch"], str) or not ARCH.fullmatch(value["host_arch"]):
        raise ValueError("invalid build host metadata")
    if "git_tag" in value and (not isinstance(value["git_tag"], str) or not TAG.fullmatch(value["git_tag"])):
        raise ValueError("invalid build git tag")


def validate(manifest: dict, artifact_root: Path | None, require_platforms: bool) -> None:
    expected_top = {
        "schema", "project", "version", "channel", "source_commit",
        "source_date_epoch", "generated_at", "artifacts", "signing",
    }
    if set(manifest) != expected_top and set(manifest) != expected_top | {"build"}:
        raise ValueError("manifest top-level keys are not exact")
    if manifest["schema"] != "run-ai-release/v1" or manifest["project"] != "run-ai":
        raise ValueError("wrong release manifest identity")
    if not VERSION.fullmatch(str(manifest["version"])):
        raise ValueError("invalid version")
    if manifest["channel"] not in {"stable", "candidate"}:
        raise ValueError("invalid channel")
    if not re.fullmatch(r"[0-9a-f]{40,64}", str(manifest["source_commit"])):
        raise ValueError("invalid source commit")
    epoch = manifest["source_date_epoch"]
    if type(epoch) is not int or epoch < 1:
        raise ValueError("invalid source date epoch")
    expected_time = dt.datetime.fromtimestamp(epoch, tz=dt.timezone.utc).isoformat().replace("+00:00", "Z")
    if manifest["generated_at"] != expected_time:
        raise ValueError("generated_at is not bound to source_date_epoch")
    signing = manifest["signing"]
    if set(signing) != {"algorithm", "key_id", "signature_path"} or signing["algorithm"] != "minisign" or signing["signature_path"] != "manifest.json.minisig":
        raise ValueError("invalid signing contract")
    if not re.fullmatch(r"[A-Za-z0-9._:-]{8,128}", str(signing["key_id"])):
        raise ValueError("invalid signing key id")
    if "build" in manifest:
        validate_build(manifest["build"])
    artifacts = manifest["artifacts"]
    if not isinstance(artifacts, list) or not artifacts:
        raise ValueError("manifest contains no artifacts")
    if artifacts != sorted(artifacts, key=lambda item: item.get("path", "")):
        raise ValueError("artifacts are not sorted")
    seen: set[str] = set()
    platforms: set[tuple[str, str, str]] = set()
    for item in artifacts:
        if set(item) != ALLOWED_ARTIFACT_KEYS:
            raise ValueError("artifact keys are not exact")
        path = safe_path(item["path"])
        sbom = item["sbom"]
        sbom_path = safe_path(sbom.get("path"))
        if path in seen or sbom_path in seen:
            raise ValueError("duplicate published path")
        seen.update((path, sbom_path))
        if item["kind"] not in {"archive", "deb"} or item["os"] not in {"linux", "darwin"} or item["arch"] not in {"amd64", "arm64"}:
            raise ValueError("unsupported artifact platform")
        if item["kind"] == "deb" and item["os"] != "linux":
            raise ValueError("Debian artifacts must target linux")
        expected_path = (
            f"run-ai_{manifest['version']}_{item['arch']}.deb"
            if item["kind"] == "deb"
            else f"run-ai_{manifest['version']}_{item['os']}_{item['arch']}.tar.gz"
        )
        expected_sbom = f"run-ai_{manifest['version']}_{item['os']}_{item['arch']}.{item['kind']}.spdx.json"
        if path != expected_path or sbom_path != expected_sbom:
            raise ValueError("artifact or SBOM path is not canonical")
        if type(item["size"]) is not int or item["size"] < 1 or not SHA.fullmatch(str(item["sha256"])):
            raise ValueError("invalid artifact size or digest")
        if set(sbom) != {"path", "format", "sha256"} or sbom["format"] != "spdx-json-2.3" or not SHA.fullmatch(str(sbom["sha256"])):
            raise ValueError("invalid SBOM metadata")
        platforms.add((item["kind"], item["os"], item["arch"]))
        if artifact_root is not None:
            artifact_file = published_file(artifact_root, path)
            sbom_file = published_file(artifact_root, sbom_path)
            actual_size, actual_sha = digest(artifact_file)
            _, actual_sbom_sha = digest(sbom_file)
            if (actual_size, actual_sha) != (item["size"], item["sha256"]) or actual_sbom_sha != sbom["sha256"]:
                raise ValueError(f"published bytes do not match manifest: {path}")
            sbom_document = json.loads(read_small_verified(sbom_file))
            if sbom_document.get("spdxVersion") != "SPDX-2.3":
                raise ValueError(f"invalid SPDX document: {sbom_path}")
            root_packages = [
                package for package in sbom_document.get("packages", [])
                if isinstance(package, dict) and package.get("name") == "run-ai"
            ]
            if len(root_packages) != 1:
                raise ValueError(f"SPDX document has no exact run-ai root package: {sbom_path}")
            root_package = root_packages[0]
            if root_package.get("versionInfo") != manifest["version"] or root_package.get("licenseDeclared") != "MIT":
                raise ValueError(f"SPDX root package identity is not exact: {sbom_path}")
            checksums = {
                (checksum.get("algorithm"), checksum.get("checksumValue"))
                for checksum in root_package.get("checksums", [])
                if isinstance(checksum, dict)
            }
            if ("SHA256", item["sha256"]) not in checksums:
                raise ValueError(f"SPDX root package is not bound to artifact: {path}")
    if require_platforms:
        required = {
            ("archive", "linux", "amd64"), ("archive", "linux", "arm64"),
            ("archive", "darwin", "amd64"), ("archive", "darwin", "arm64"),
            ("deb", "linux", "amd64"), ("deb", "linux", "arm64"),
        }
        if not required.issubset(platforms):
            raise ValueError("release matrix is incomplete")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("manifest", type=Path)
    parser.add_argument("--artifact-root", type=Path)
    parser.add_argument("--require-platforms", action="store_true")
    args = parser.parse_args()
    validate(
        json.loads(read_small_verified(args.manifest, 1024 * 1024)),
        args.artifact_root,
        args.require_platforms,
    )
    print("release manifest: OK")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
