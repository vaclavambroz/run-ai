# Roadmap

This document describes the current implementation state of run-ai features.
It distinguishes features that are fully implemented, partially implemented
(plan or infrastructure exists, execution blocked), and planned.

## Status key

| Symbol | Meaning |
|---|---|
| ✅ | Implemented — works end-to-end |
| ⚠️ | Partial — planning, parsing, or infrastructure complete; execution blocked in this build |
| 🔲 | Planned — not yet implemented |

---

## Access modes

| Feature | Status | Notes |
|---|---|---|
| Full mode (default) | ✅ | Sessions launch; warning presented before execution |
| `--confirm` | ⚠️ | Plan building, permission policy, and two-phase execution contract implemented. Launch is blocked (`--confirm launch is blocked until a hard per-action confirmation adapter is linked`). `--explain` works. |
| `--readonly` | ⚠️ | Plan building, permission policy, and analyze-only contract implemented. Launch is blocked (`--readonly launch is blocked until the platform hard-readonly adapter is linked`). `--explain` works. |

## Workflows

| Feature | Status | Notes |
|---|---|---|
| Generic session | ✅ | Launches provider session with access-mode prompt |
| `--dev DOMAIN` | ✅ | Resolves domain from inventory, opens app root; requires `RUN_AI_INVENTORY_PATH` |
| `--remote HOST` | ✅ | Delegates to remote host via SSH; requires `RUN_AI_INVENTORY_PATH` |
| `--monitoring` | ⚠️ | Plan building complete. Launch is blocked (`monitoring workflow is plan-only in this build`). `--explain` works. |
| `--spam URL [URL...]` | ⚠️ | Plan building, URL validation, fail-closed host binding (`--spam-host` required), and two-phase execution contract implemented. Launch is blocked (`spam workflow is plan-only in this build`). `--explain` works. |

## Session management

| Feature | Status | Notes |
|---|---|---|
| tmux session launch | ✅ | |
| Session resume by key (`--resume`) | ✅ | |
| Session resume by provider ID (`--resume-id`) | ✅ | Claude; tmux-lifetime-only for Gemini |
| Session lineage tracking | ✅ | |
| `--new-session` | ✅ | |
| Persistent Codex app-server (`--persistent`) | ✅ | Requires app-server supervisor running |
| Continuation runner (`run_ai continuation`) | ✅ | Requires explicit `--session-id`, `--progress`, `--log` |

## Providers

| Feature | Status | Notes |
|---|---|---|
| OpenAI Codex (`--codex`) | ✅ | Default provider |
| Anthropic Claude (`--claude`) | ✅ | |
| Google Gemini via Antigravity (`--gemini`) | ✅ | |

## Audit receipts

| Feature | Status | Notes |
|---|---|---|
| Receipt schema and tamper-evident digest | ✅ | `internal/audit` package; structural validation and self-rehash |
| Plan-bound receipt validation (`ValidateWithPlan`) | ✅ | Detects cross-field domain-substitution forgeries |
| Receipt creation on launch | 🔲 | `audit.New()` exists but is not called from `main.go` in this build |
| Receipt log on disk | 🔲 | `audit.WriteAuditLog()` / `ReadAuditLog()` exist; path reserved in config but not wired |
| `run_ai audit` subcommand | 🔲 | No CLI surface for reading or verifying receipts |

## Sandbox

| Feature | Status | Notes |
|---|---|---|
| Linux bubblewrap projection (v2) | ✅ | Requires kernel support and projection socket |
| Self-test on sandbox build | ✅ | Adversarial self-test verifies isolation before wrapping |
| macOS sandbox | ⚠️ | Placeholder blocker; not implemented |

## Team mode

| Feature | Status | Notes |
|---|---|---|
| `--team` Turing/Fermat/Dewey/Faraday contract | ✅ | Digest-bound four-role prompt contract |

## Remote and dev workflows

| Feature | Status | Notes |
|---|---|---|
| Inventory resolution | ✅ | JSON inventory file; requires `RUN_AI_INVENTORY_PATH` |
| SSH remote delegation | ✅ | Requires inventory entry and SSH connectivity |
| Domain dev scope | ✅ | Requires inventory entry |
| Integration test against real inventory | 🔲 | No automated integration test covers live remote/dev paths |

## Distribution

| Feature | Status | Notes |
|---|---|---|
| Debian APT package | ✅ | Packaging scripts and postinst in `packaging/debian/` |
| macOS Homebrew formula | ✅ | Formula in `packaging/homebrew/`; tap at `vaclavambroz/homebrew-run-ai` |
| Minisign-signed release manifests | ✅ | `packaging/release/` tooling |
| `go install github.com/vaclavambroz/run-ai/cmd/run_ai@latest` | ✅ | Module path is canonical |

---

## Notes for contributors

Features marked ⚠️ have their planning and contract layers complete and
tested. The missing piece in each case is a runtime adapter: a sandboxed
execution environment that enforces the declared contract without exposing
network access or mutation capability to the AI before the access-mode
gate is satisfied.

The `--confirm` and `--readonly` guards in `main.go` are intentional
fail-closed stubs, not bugs to remove. Do not bypass them without
implementing the corresponding execution adapter.
