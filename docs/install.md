## Installation

### Primary method: go install

```
go install github.com/vaclavambroz/run-ai/cmd/run_ai@latest
```

This installs the `run_ai` binary into `$GOPATH/bin` (or `$HOME/go/bin` by default).

### Release artifacts

Pre-built binaries for Linux (amd64, arm64) and macOS (amd64, arm64) are available on the [GitHub releases page](https://github.com/vaclavambroz/run-ai/releases).

Each release includes:
- `.tar.gz` archives for Linux and macOS
- `.deb` packages for Debian/Ubuntu
- `manifest.json` with SHA256 checksums for all artifacts
- A minisign signature over the manifest

After downloading a binary, verify the SHA256 checksum against the value in `manifest.json` before running it.

### APT repository

An APT repository for Debian/Ubuntu is planned. Details will be published on the GitHub releases page when available.

### Dependencies

Runtime dependencies: `tmux`, `rsync`, `openssh-client`, `ca-certificates`, `curl`.

On Linux, `bubblewrap` is required for sandbox mode.

### macOS

Install via the release `.tar.gz` archive or `go install`. Homebrew support is planned; see [distribution.md](distribution.md).
