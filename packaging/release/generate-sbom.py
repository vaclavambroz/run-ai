#!/usr/bin/env python3
"""Generate a deterministic SPDX 2.3 JSON SBOM for one release artifact."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import stat
import subprocess
from pathlib import Path


REPOSITORY_LICENSE = Path(__file__).resolve().parents[2] / "LICENSE"


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | getattr(os, "O_CLOEXEC", 0))
    try:
        before = os.fstat(descriptor)
        if not stat.S_ISREG(before.st_mode) or before.st_size < 1:
            raise ValueError("binary is not a non-empty regular file")
        while True:
            chunk = os.read(descriptor, 1024 * 1024)
            if not chunk:
                break
            digest.update(chunk)
        after = os.fstat(descriptor)
        identity = lambda value: (value.st_dev, value.st_ino, value.st_mode, value.st_uid, value.st_gid, value.st_size, value.st_mtime_ns)
        if identity(before) != identity(after):
            raise ValueError("binary changed while generating SBOM")
    finally:
        os.close(descriptor)
    return digest.hexdigest()


def spdx_id(value: str) -> str:
    return "SPDXRef-" + re.sub(r"[^A-Za-z0-9.-]", "-", value).strip("-.")


def go_dependencies(binary: Path | None) -> list[tuple[str, str, str | None]]:
    if binary is None:
        return []
    try:
        result = subprocess.run(
            ["go", "version", "-m", str(binary)],
            check=True,
            capture_output=True,
            text=True,
            timeout=15,
        )
    except (FileNotFoundError, subprocess.CalledProcessError, subprocess.TimeoutExpired):
        return []
    dependencies: list[tuple[str, str, str | None]] = []
    for line in result.stdout.splitlines():
        fields = line.strip().split("\t")
        if len(fields) >= 3 and fields[0] == "dep":
            checksum = fields[3] if len(fields) >= 4 and fields[3].startswith("h1:") else None
            dependencies.append((fields[1], fields[2], checksum))
    return sorted(set(dependencies))


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("artifact", type=Path)
    parser.add_argument("output", type=Path)
    parser.add_argument("--go-binary", type=Path)
    parser.add_argument("--version", required=True)
    parser.add_argument("--commit", required=True)
    parser.add_argument("--source-date-epoch", required=True, type=int)
    args = parser.parse_args()
    artifact_sha = sha256(args.artifact)
    if args.go_binary is not None:
        sha256(args.go_binary)  # Enforce the same regular/non-symlink contract.
    root_id = "SPDXRef-Package-run-ai"
    packages = [{
        "name": "run-ai",
        "SPDXID": root_id,
        "versionInfo": args.version,
        "downloadLocation": "NOASSERTION",
        "filesAnalyzed": False,
        "licenseConcluded": "MIT",
        "licenseDeclared": "MIT",
        "copyrightText": "Copyright (c) 2026 Václav Ambrož",
        "checksums": [{"algorithm": "SHA256", "checksumValue": artifact_sha}],
        "externalRefs": [{
            "referenceCategory": "PACKAGE-MANAGER",
            "referenceType": "purl",
            "referenceLocator": f"pkg:generic/run-ai@{args.version}?commit={args.commit}",
        }],
    }]
    relationships = [{
        "spdxElementId": "SPDXRef-DOCUMENT",
        "relationshipType": "DESCRIBES",
        "relatedSpdxElement": root_id,
    }]
    for index, (module, version, go_sum) in enumerate(go_dependencies(args.go_binary), 1):
        dependency_id = spdx_id(f"GoModule-{index}-{module}")
        package = {
            "name": module,
            "SPDXID": dependency_id,
            "versionInfo": version,
            "downloadLocation": "NOASSERTION",
            "filesAnalyzed": False,
            "licenseConcluded": "NOASSERTION",
            "licenseDeclared": "NOASSERTION",
            "copyrightText": "NOASSERTION",
            "externalRefs": [{
                "referenceCategory": "PACKAGE-MANAGER",
                "referenceType": "purl",
                "referenceLocator": f"pkg:golang/{module}@{version}",
            }],
        }
        if go_sum:
            package["comment"] = f"Go module checksum: {go_sum}"
        packages.append(package)
        relationships.append({
            "spdxElementId": root_id,
            "relationshipType": "DEPENDS_ON",
            "relatedSpdxElement": dependency_id,
        })
    document = {
        "spdxVersion": "SPDX-2.3",
        "dataLicense": "CC0-1.0",
        "SPDXID": "SPDXRef-DOCUMENT",
        "name": f"run-ai-{args.version}-{args.artifact.name}",
        "documentNamespace": f"https://github.com/vaclavambroz/run-ai/sbom/{args.version}/{artifact_sha}",
        "creationInfo": {
            "created": __import__("datetime").datetime.fromtimestamp(
                args.source_date_epoch, tz=__import__("datetime").timezone.utc
            ).isoformat().replace("+00:00", "Z"),
            "creators": ["Tool: run-ai packaging/generate-sbom.py"],
        },
        "packages": packages,
        "relationships": relationships,
    }
    if args.output.is_symlink():
        raise ValueError("SBOM output must not be a symlink")
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(
        json.dumps(document, sort_keys=True, separators=(",", ":")) + "\n",
        encoding="utf-8",
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
