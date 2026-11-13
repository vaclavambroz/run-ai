## Security Model

run-ai is a fail-closed terminal AI session launcher. No session launches unless a validated, digest-bound plan is present.

### Fail-closed design

Launch is blocked whenever preconditions are not satisfied:

- Monitoring and spam workflows are plan-only in this build. They produce a projection v1 plan via `--explain` but cannot start a live session.
- The `--confirm` access mode blocks launch if the per-action confirmation adapter is not registered.
- The `--readonly` access mode blocks launch if the hard read-only audit sink is not registered.

Any missing or invalid plan causes an immediate exit before the provider process is started.

### Plan validation and SHA256 digests

Every workflow plan carries a SHA256 digest bound to its full normalized content. The plan is serialized and re-validated on every load. Modifying any field of the plan invalidates its digest and causes the launch to be rejected. Plans are version-bound: a plan built by one binary version is not accepted by a different version.

### Audit receipts

An audit receipt is created for every launch event. Receipts are canonical, secret-free JSON records that bind:

- the plan digest
- the permission policy digest
- the capability profile digest
- the execution outcome

No prompt text, URLs, file paths, or credentials are stored in receipts.

### Permission modes

Three access modes are supported:

| Mode | Description |
|------|-------------|
| `full` | Unrestricted file and network access within the sandbox |
| `confirm` | Per-action confirmation required before each write |
| `readonly` | Hard read-only; write operations are rejected at the audit layer |

Each mode produces a stable policy digest embedded in the plan.

### Sandbox (bubblewrap)

On Linux, the AI process runs inside a bubblewrap filesystem projection. The sandbox restricts the process to its application root and a small set of read-only binaries. The projection is constructed from the plan at launch time and is not shared across sessions.

### No stored credentials

run-ai does not store API keys, tokens, or any other credentials. Provider authentication is passed through the environment at launch time and is not written to disk or included in audit records.
