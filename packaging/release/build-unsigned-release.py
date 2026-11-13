#!/usr/bin/env python3
"""Build the complete unsigned release matrix into a new staging directory."""
from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import stat
import subprocess
import tempfile
from pathlib import Path


REPOSITORY = Path(__file__).resolve().parents[2]
PACKAGING = REPOSITORY / "packaging"
VERSION = re.compile(r"^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?$")
COMMIT = re.compile(r"^[0-9a-f]{40,64}$")


def run(argv: list[str], *, env: dict[str, str] | None = None) -> None:
    subprocess.run(argv, cwd=REPOSITORY, env=env, check=True, timeout=180)


def prepare_output(path: Path) -> None:
    if path.is_symlink():
        raise ValueError("release output must not be a symlink")
    if path.exists():
        info = path.lstat()
        if not stat.S_ISDIR(info.st_mode) or any(path.iterdir()):
            raise ValueError("release output must be absent or an empty real directory")
    else:
        path.mkdir(parents=True, mode=0o755)


TAG = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$")


def command_output(argv: list[str]) -> str:
    return subprocess.run(argv, cwd=REPOSITORY, check=True, capture_output=True, text=True, timeout=15).stdout.strip()


def verify_source(commit: str, development_allow_dirty: bool, channel: str, git_tag: str | None) -> None:
    if development_allow_dirty:
        if channel == "stable":
            raise ValueError("stable releases cannot use the dirty-source test override")
        return
    actual = command_output(["git", "rev-parse", "HEAD"])
    if actual != commit:
        raise ValueError("release commit does not match checked-out HEAD")
    status = command_output(["git", "status", "--porcelain", "--untracked-files=all"])
    if status:
        raise ValueError("release source tree is not clean")
    if git_tag is not None:
        if not TAG.fullmatch(git_tag) or ".." in git_tag or "@{" in git_tag:
            raise ValueError("invalid git tag")
        if command_output(["git", "cat-file", "-t", f"refs/tags/{git_tag}"]) != "tag":
            raise ValueError("release git tag must be annotated")
        if command_output(["git", "rev-parse", f"refs/tags/{git_tag}^{{}}"] ) != commit:
            raise ValueError("release git tag does not resolve to source commit")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--version", required=True)
    parser.add_argument("--commit", required=True)
    parser.add_argument("--source-date-epoch", required=True, type=int)
    parser.add_argument("--channel", choices=("candidate", "stable"), default="candidate")
    parser.add_argument("--key-id", default="run-ai-release-2026")
    parser.add_argument("--git-tag")
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--development-allow-dirty", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args()
    if not VERSION.fullmatch(args.version) or not COMMIT.fullmatch(args.commit):
        raise ValueError("invalid release version or source commit")
    if args.source_date_epoch < 1:
        raise ValueError("invalid source date epoch")
    for command in ("go", "tar", "gzip", "dpkg-deb"):
        if shutil.which(command) is None:
            raise RuntimeError(f"required build command is missing: {command}")
    verify_source(args.commit, args.development_allow_dirty, args.channel, args.git_tag)
    prepare_output(args.output)
    environment = os.environ.copy()
    environment.update({"CGO_ENABLED": "0", "SOURCE_DATE_EPOCH": str(args.source_date_epoch)})
    build = {
        "go_toolchain": command_output(["go", "version"]),
        "host_os": command_output(["go", "env", "GOHOSTOS"]),
        "host_arch": command_output(["go", "env", "GOHOSTARCH"]),
    }
    if args.git_tag is not None:
        build["git_tag"] = args.git_tag
    entries: list[dict[str, object]] = []
    with tempfile.TemporaryDirectory(prefix="run-ai-release-") as raw_work:
        work = Path(raw_work)
        binaries: dict[tuple[str, str], Path] = {}
        for operating_system in ("linux", "darwin"):
            for architecture in ("amd64", "arm64"):
                binary = work / f"run_ai_{operating_system}_{architecture}"
                build_env = environment | {"GOOS": operating_system, "GOARCH": architecture}
                run(["go", "build", "-trimpath", "-buildvcs=false", "-o", str(binary), "./cmd/run_ai"], env=build_env)
                binaries[(operating_system, architecture)] = binary
                run([
                    "sh", str(PACKAGING / "archive/build-archive.sh"), args.version,
                    operating_system, architecture, str(binary), str(args.output),
                ], env=environment)
                artifact = f"run-ai_{args.version}_{operating_system}_{architecture}.tar.gz"
                sbom = f"run-ai_{args.version}_{operating_system}_{architecture}.archive.spdx.json"
                run([
                    "python3", str(PACKAGING / "release/generate-sbom.py"), str(args.output / artifact),
                    str(args.output / sbom), "--version", args.version, "--commit",
                    args.commit, "--source-date-epoch", str(args.source_date_epoch), "--go-binary", str(binary),
                ], env=environment)
                entries.append({
                    "file": artifact, "path": artifact, "kind": "archive",
                    "os": operating_system, "arch": architecture,
                    "sbom_file": sbom, "sbom_path": sbom,
                })
        for architecture in ("amd64", "arm64"):
            binary = binaries[("linux", architecture)]
            run([
                "sh", str(PACKAGING / "debian/build-deb.sh"), args.version,
                architecture, str(binary), str(args.output),
            ], env=environment)
            artifact = f"run-ai_{args.version}_{architecture}.deb"
            sbom = f"run-ai_{args.version}_linux_{architecture}.deb.spdx.json"
            run([
                "python3", str(PACKAGING / "release/generate-sbom.py"), str(args.output / artifact),
                str(args.output / sbom), "--version", args.version, "--commit",
                args.commit, "--source-date-epoch", str(args.source_date_epoch), "--go-binary", str(binary),
            ], env=environment)
            entries.append({
                "file": artifact, "path": artifact, "kind": "deb",
                "os": "linux", "arch": architecture,
                "sbom_file": sbom, "sbom_path": sbom,
            })
        spec = args.output / ".release-input.json"
        spec.write_text(json.dumps({
            "schema": "run-ai-release-input/v1",
            "version": args.version,
            "channel": args.channel,
            "source_commit": args.commit,
            "source_date_epoch": args.source_date_epoch,
            "key_id": args.key_id,
            "build": build,
            "artifacts": entries,
        }, sort_keys=True), encoding="utf-8")
        manifest = args.output / "manifest.json"
        try:
            run(["python3", str(PACKAGING / "release/generate-manifest.py"), str(spec), str(manifest)])
        finally:
            spec.unlink(missing_ok=True)
        run([
            "python3", str(PACKAGING / "release/validate-release.py"), str(manifest),
            "--artifact-root", str(args.output), "--require-platforms",
        ])
        run([
            "python3", str(PACKAGING / "homebrew/render-formula.py"), str(manifest),
            str(args.output / "run-ai.rb"),
        ])
    print(args.output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
