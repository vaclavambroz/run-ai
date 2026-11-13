## Migration Guide

This document covers what to verify when upgrading run-ai between versions.

### Plans are version-bound

Each plan is bound to the binary version that created it. A plan produced by one version is rejected by a different version. This is a fail-closed safeguard: stale plans cannot silently activate under a new binary.

After upgrading, any in-flight `--explain` output must be regenerated with the new binary before it can be used to start a session.

### Audit receipts carry plan digests

Audit receipts reference the SHA256 digest of the plan that authorized each session. The receipt format is stable across versions, but the plan digest will differ between versions even for identical inputs. Keep old receipts for historical audit trails; they remain valid records of what ran under the previous version.

### Verifying binary integrity

Before deploying a new binary, verify its SHA256 checksum against the value published in the release manifest:

```
sha256sum run_ai
```

Compare the output to the `sha256` field in `manifest.json` for the corresponding artifact. Signed releases also include a minisign signature over the manifest.

### Monitoring and spam workflows remain plan-only

Monitoring and spam workflows are restricted to plan-only mode in this build. After upgrading, these workflows continue to produce plans via `--explain` but cannot launch live sessions. This restriction is intentional and does not change between versions.

### State directory compatibility

The state directory layout is stable. Upgrading the binary does not require migrating session metadata or audit logs. If you need to roll back, restore the previous binary; existing state remains readable.

### Rollback

See [upgrade-rollback.md](upgrade-rollback.md) for step-by-step rollback instructions.
