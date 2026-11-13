#!/usr/bin/env python3
"""Capture or verify an exact, commit-independent run-ai source state."""
from __future__ import annotations

import argparse
import gzip
import hashlib
import json
import os
import stat
import subprocess
import tarfile
from pathlib import Path, PurePosixPath


SCHEMA = "run-ai-source-state/v1"


def source_paths(repository: Path) -> list[PurePosixPath]:
    result = subprocess.run(
        ["git", "-C", str(repository), "ls-files", "-z", "--cached", "--others", "--exclude-standard"],
        check=True, capture_output=True, timeout=30,
    )
    values: list[PurePosixPath] = []
    for value in result.stdout.split(b"\0"):
        if not value:
            continue
        name = value.decode("utf-8", "strict")
        path = PurePosixPath(name)
        if path.is_absolute() or ".." in path.parts or str(path) in {"", "."}:
            raise ValueError(f"invalid source path from git: {name!r}")
        values.append(path)
    return sorted(values, key=lambda value: str(value).encode("utf-8"))


def records(repository: Path, paths: list[PurePosixPath]) -> list[dict[str, str]]:
    values: list[dict[str, str]] = []
    for relative in paths:
        absolute = repository / str(relative)
        info = absolute.lstat()
        mode = stat.S_IMODE(info.st_mode)
        if stat.S_ISREG(info.st_mode):
            digest = hashlib.sha256(absolute.read_bytes()).hexdigest()
            values.append({"path": str(relative), "type": "file", "mode": f"{mode:04o}", "sha256": digest})
        elif stat.S_ISLNK(info.st_mode):
            target = os.readlink(absolute)
            digest = hashlib.sha256(target.encode("utf-8", "surrogateescape")).hexdigest()
            values.append({"path": str(relative), "type": "symlink", "mode": f"{mode:04o}", "sha256": digest, "target": target})
        else:
            raise ValueError(f"source path is not a regular file or symlink: {relative}")
    return values


def manifest_paths(values: object) -> list[PurePosixPath]:
    if not isinstance(values, list):
        raise ValueError("source-state files must be a list")
    paths: list[PurePosixPath] = []
    for value in values:
        if not isinstance(value, dict) or not isinstance(value.get("path"), str):
            raise ValueError("invalid source-state file entry")
        path = PurePosixPath(value["path"])
        if path.is_absolute() or ".." in path.parts or str(path) in {"", "."}:
            raise ValueError("invalid source-state manifest path")
        paths.append(path)
    if paths != sorted(paths, key=lambda value: str(value).encode("utf-8")) or len(set(paths)) != len(paths):
        raise ValueError("source-state manifest paths are not unique and sorted")
    return paths


def tree_digest(values: list[dict[str, str]]) -> str:
    encoded = json.dumps(values, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()


def write_archive(repository: Path, paths: list[PurePosixPath], archive: Path) -> None:
    archive.parent.mkdir(parents=True, exist_ok=True)
    with archive.open("wb") as raw, gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as zipped:
        with tarfile.open(fileobj=zipped, mode="w", format=tarfile.PAX_FORMAT) as output:
            for relative in paths:
                absolute = repository / str(relative)
                info = absolute.lstat()
                entry = tarfile.TarInfo(str(relative))
                entry.uid = entry.gid = entry.mtime = 0
                entry.uname = entry.gname = ""
                entry.mode = stat.S_IMODE(info.st_mode)
                if stat.S_ISREG(info.st_mode):
                    entry.type = tarfile.REGTYPE
                    entry.size = info.st_size
                    with absolute.open("rb") as source:
                        output.addfile(entry, source)
                elif stat.S_ISLNK(info.st_mode):
                    entry.type = tarfile.SYMTYPE
                    entry.linkname = os.readlink(absolute)
                    output.addfile(entry)
                else:
                    raise ValueError(f"source path is not a regular file or symlink: {relative}")


def sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def capture(repository: Path, manifest: Path, archive: Path) -> None:
    repository = repository.resolve(strict=True)
    initial_paths = source_paths(repository)
    initial_records = records(repository, initial_paths)
    write_archive(repository, initial_paths, archive)
    final_paths = source_paths(repository)
    final_records = records(repository, final_paths)
    if initial_records != final_records:
        raise RuntimeError("source changed while candidate snapshot was being captured")
    revision = subprocess.run(
        ["git", "-C", str(repository), "rev-parse", "HEAD"], check=True,
        capture_output=True, text=True, timeout=15,
    ).stdout.strip()
    state = "clean" if not subprocess.run(
        ["git", "-C", str(repository), "status", "--porcelain", "--untracked-files=all"],
        check=True, capture_output=True, text=True, timeout=15,
    ).stdout else "dirty"
    value = {
        "schema": SCHEMA,
        "base_revision": revision,
        "source_tree": state,
        "tree_sha256": tree_digest(final_records),
        "archive_sha256": sha256(archive),
        "files": final_records,
    }
    manifest.write_text(json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False) + "\n", encoding="utf-8")


def verify(manifest: Path, repository: Path, archive: Path | None) -> None:
    value = json.loads(manifest.read_text(encoding="utf-8"))
    if value.get("schema") != SCHEMA:
        raise ValueError("invalid source-state manifest")
    paths = manifest_paths(value.get("files"))
    actual = records(repository.resolve(strict=True), paths)
    if actual != value["files"] or tree_digest(actual) != value.get("tree_sha256"):
        raise RuntimeError("source state does not match manifest")
    if archive is not None and sha256(archive) != value.get("archive_sha256"):
        raise RuntimeError("source archive does not match manifest")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--repository", type=Path)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--archive", type=Path)
    parser.add_argument("--verify", type=Path)
    parser.add_argument("--source", type=Path)
    args = parser.parse_args()
    if args.verify is not None:
        if args.source is None or any(value is not None for value in (args.repository, args.output)):
            parser.error("--verify requires --source and accepts optional --archive")
        verify(args.verify, args.source, args.archive)
    else:
        if args.repository is None or args.output is None or args.archive is None or args.source is not None:
            parser.error("capture requires --repository, --output and --archive")
        capture(args.repository, args.output, args.archive)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
