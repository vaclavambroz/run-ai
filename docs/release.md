## Release Process

### Overview

Releases are built from a tagged commit using a reproducible build pipeline. The same source commit and build inputs always produce byte-for-byte identical binaries.

### Deterministic builds

Builds set `SOURCE_DATE_EPOCH` to the commit timestamp and pass `-trimpath -buildvcs=false` to the Go compiler. This ensures that file modification times and embedded paths do not vary between builds on different machines.

### Artifacts

Each release produces:
- `.tar.gz` archives for Linux amd64, Linux arm64, macOS amd64, macOS arm64
- `.deb` packages for Debian/Ubuntu (amd64, arm64)
- An SPDX 2.3 JSON SBOM for each artifact
- `manifest.json` listing all artifacts with SHA256 checksums and SBOM references
- `manifest.json.minisig` — a minisign signature over the manifest

### SBOM generation

An SBOM is generated for each artifact using `packaging/release/generate-sbom.py`. The SBOM lists the root package and all Go module dependencies extracted from the binary using `go version -m`. The document namespace is bound to the artifact SHA256 digest.

### Signing

The release manifest is signed with [minisign](https://jedisct1.github.io/minisign/). The public key is published in the repository. Signing is performed offline; the build pipeline produces an unsigned manifest, which is then signed separately.

### Publishing

Release artifacts are published to the GitHub releases page. The manifest and signature are included in the release assets alongside the binaries.

### Verification

To verify a release:
1. Download `manifest.json` and `manifest.json.minisig`.
2. Verify the signature: `minisign -Vm manifest.json -p run-ai-release.pub`
3. Verify artifact checksums against the `sha256` fields in `manifest.json`.
