# run-ai

A provider-neutral terminal AI session runner. Launches and manages AI provider sessions (Codex, Claude, Gemini) inside tmux, with built-in workflow planning, audit receipts, and optional sandbox isolation.

## Features

- **Provider-neutral**: supports Codex, Claude, and Gemini (via the Antigravity runtime)
- **Workflow planning**: validates and digests every session before launch; plans are tamper-evident
- **Access modes**: full, confirm (per-action), and hard read-only
- **Audit receipts**: canonical, secret-free JSON records of every launch event
- **Session management**: tmux-backed lineage tracking with resume by exact provider ID
- **Persistent sessions**: reconnectable Codex app-server supervisor
- **Remote delegation**: launch on a remote host via SSH using a local inventory
- **Domain development workflow**: resolve a domain from inventory and open the app root
- **Team contracts**: fixed four-role Turing/Fermat/Dewey/Faraday team mode with bound digests
- **Linux sandbox**: optional bubblewrap-based filesystem projection (Linux only)

## Installation

### macOS (Homebrew)

```sh
brew tap vaclavambroz/run-ai
brew install run-ai
run_ai --help
```

### Debian/Ubuntu (APT)

See [docs/distribution.md](docs/distribution.md) for APT repository setup.

### From source

Requires Go 1.22+:

```sh
git clone https://github.com/vaclavambroz/run-ai.git
cd run-ai
make build
./build/run_ai --help
```

## Quick start

```sh
# Open a Codex session on the current host
run_ai --codex

# Open a Claude session in a new isolated tmux lineage
run_ai --claude --new-session

# Explain a planned request without launching
run_ai --claude --confirm --new-session --explain

# Open a development session for a domain
run_ai --gemini --dev example.cz

# Open a monitoring session (plan-only in this build)
run_ai --codex --monitoring --explain

# Resume a Claude session by provider ID
run_ai --claude --resume-id SESSION_ID
```

## Configuration

run-ai reads its configuration from:

- **Linux**: `~/.config/run-ai/config.json`
- **macOS**: `~/Library/Application Support/run-ai/config.json`

See [docs/configuration.md](docs/configuration.md) for details.

### Environment variables

| Variable | Description |
|---|---|
| `RUN_AI_INVENTORY_PATH` | Path to domain inventory JSON file (required for `--remote` and `--dev`) |
| `RUN_AI_REMOTE_ALIASES` | JSON object mapping server hostnames to SSH alias names |
| `RUN_AI_SSH_PATH` | Path to SSH binary (default: `/usr/bin/ssh`) |
| `RUN_AI_LAUNCHER_DIR` | Directory containing provider launcher scripts (default: `/usr/local/lib/run-ai`) |

## Providers

| Flag | Provider | Notes |
|---|---|---|
| `--codex` | OpenAI Codex | Default provider |
| `--claude` | Anthropic Claude | Exact lineage resume by provider ID |
| `--gemini` | Google Gemini | Via Antigravity runtime; tmux-lifetime-only resume |
| `--provider NAME` | Any of the above | Explicit name form |

## Access modes

| Flag | Mode | Description |
|---|---|---|
| (default) | full | Unrestricted; AI may modify files and external state |
| `--confirm` | confirm | Per-action confirmation required before each mutation |
| `--readonly` | readonly | Hard read-only; mutations and escalation not permitted |

## Workflows

| Flag | Description |
|---|---|
| (none) | Generic session on current host |
| `--monitoring` | Icinga repair and robot cleanup (plan-only in this build) |
| `--spam URL [URL...]` | Antispam analysis (plan-only in this build; requires configured spam host) |
| `--dev DOMAIN` | Resolve domain inventory and open the application root |

## Building

```sh
make build          # compile binary to build/run_ai
make test           # run all tests (Go + packaging)
make test-go        # Go tests only
make test-packaging # Python packaging tests only
make lint           # gofmt, go vet, shell and JSON validation
```

## Project structure

```
cmd/run_ai/           main binary and integration tests
internal/
  audit/              canonical launch receipts
  cli/                argument parsing and help text
  config/             XDG/macOS path resolution
  permissions/        access mode policies
  providers/          provider name and model types
  routing/            domain inventory loading and resolution
  sandbox/            bubblewrap sandbox builder (Linux)
  sessions/           tmux session management and lineage tracking
  team/               team contract and prompt activation
  workflows/          workflow planning and digest binding
packaging/
  archive/            platform archive build script
  debian/             Debian package metadata
  homebrew/           Homebrew formula template
  release/            release manifest, SBOM, signing scripts
  tests/              packaging unit tests
  wsl/                WSL detection and profile
schemas/              JSON Schema definitions for all canonical documents
```

## Security

See [SECURITY.md](SECURITY.md) for the vulnerability disclosure policy.

Releases are signed with Minisign. The public key and release manifests are
available at the project homepage.

## License

MIT — see [LICENSE](LICENSE).
