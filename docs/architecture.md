## Architecture

run-ai is a fail-closed terminal AI session launcher. It validates and digests every workflow request before launching any provider session. Plans are tamper-evident: each plan carries a SHA256 digest bound to its full content, and the plan is re-validated before every launch.

### Components

**`cmd/run_ai`** — the main binary. Parses CLI arguments, resolves the launch target, selects or creates a tmux session, and starts the AI provider. The main program does not write state without an explicit workflow plan.

**`internal/workflows`** — workflow planning. Validates the access mode, workflow selector, and input URLs. Builds a digest-bound plan. The plan is serialized and re-validated on every load.

**`internal/permissions`** — access mode policies. Three modes: full, confirm (per-action), and hard read-only. Each mode produces a stable policy digest that is embedded in the workflow plan.

**`internal/audit`** — audit receipts. Creates canonical, secret-free JSON records of every launch event. Receipts bind the plan digest, permission policy digest, capability profile digest, and execution outcome. No prompt text, URLs, file paths, or credentials are stored.

**`internal/sessions`** — tmux session management. Tracks provider session IDs and lineage. Classic sessions use a key/ID mapping; persistent sessions use a sealed workflow manifest.

**`internal/routing`** — domain inventory loading and resolution. Resolves a domain name to its canonical server and document root.

**`internal/sandbox`** — Linux sandbox builder. Constructs a bubblewrap filesystem projection that restricts the AI process to its application root and a small set of read-only binaries.

**`internal/team`** — team contracts. The fixed four-role Turing/Fermat/Dewey/Faraday team contract is activated as a digest-bound manifest embedded in the plan.

### Fail-closed design

Monitoring and spam workflows are plan-only in this build: they produce a plan via `--explain` but cannot launch a live session. The `--confirm` and `--readonly` access modes block launch if the per-action confirmation adapter or the read-only audit sink are not registered.

Remote delegation forwards to a legacy launcher on the target server over SSH. The delegation path is restricted to full mode during migration.

### Persistent sessions

The reconnectable Codex app-server supervisor manages a local Unix socket. The main program connects to the supervisor, which keeps the Codex process alive between terminal sessions. Persistent sessions are restricted to Codex/full mode; team, remote, monitoring, and spam workflows are intentionally rejected.
