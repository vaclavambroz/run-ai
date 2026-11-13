## Distribution Channels

### GitHub releases

The primary distribution channel is the [GitHub releases page](https://github.com/vaclavambroz/run-ai/releases).

Each release publishes:
- `.tar.gz` archives for Linux amd64, Linux arm64, macOS amd64, macOS arm64
- `.deb` packages for Debian/Ubuntu (amd64, arm64)
- `manifest.json` listing all artifacts with SHA256 checksums
- `manifest.json.minisig` — a minisign signature over the manifest

#### Verifying SHA256 checksums

Download `manifest.json` alongside the artifact, then verify:

```
sha256sum run-ai_<version>_linux_amd64.tar.gz
```

Compare the output to the `sha256` field for the matching artifact entry in `manifest.json`.

To verify the manifest signature, use [minisign](https://jedisct1.github.io/minisign/) with the release public key published in the repository.

### go install

```
go install github.com/vaclavambroz/run-ai/cmd/run_ai@latest
```

Installs directly from source. No signature verification is performed by `go install`; use release artifacts if you require verified binaries.

### APT repository

An APT repository for Debian/Ubuntu is planned. When available, it will be announced on the GitHub releases page.

### Homebrew (future)

A Homebrew formula is planned for macOS distribution. It will be published to a tap on GitHub when available.
