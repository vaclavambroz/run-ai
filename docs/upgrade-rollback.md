## Upgrade and Rollback

### Before upgrading

1. Back up the state directory:
   ```
   cp -a ~/.local/share/run-ai/state ~/.local/share/run-ai/state.bak
   ```
2. Record the SHA256 digest of the current binary:
   ```
   sha256sum $(which run_ai)
   ```
   Keep this value; you will need it if you need to roll back.

### Upgrading

1. Download the new binary or release archive from the [GitHub releases page](https://github.com/vaclavambroz/run-ai/releases).
2. Verify the SHA256 digest of the new binary against the value in `manifest.json`:
   ```
   sha256sum run_ai
   ```
3. Replace the binary:
   ```
   install -m 0755 run_ai ~/.local/bin/run_ai
   ```
4. Verify that sessions are listed correctly:
   ```
   run_ai sessions list
   ```
5. Regenerate any pending `--explain` plans, as plans are version-bound and will be rejected by the new binary.

### Rolling back

1. Restore the previous binary using the digest you recorded before upgrading. If you kept the previous binary:
   ```
   install -m 0755 run_ai.prev ~/.local/bin/run_ai
   ```
2. Verify the restored binary digest matches what you recorded.
3. If session state was affected, restore from the backup:
   ```
   rm -rf ~/.local/share/run-ai/state
   cp -a ~/.local/share/run-ai/state.bak ~/.local/share/run-ai/state
   ```

### Notes

- Audit receipts are not affected by upgrade or rollback. Historical receipts remain valid records of what ran under each binary version.
- The state directory layout is stable; rolling back the binary does not require migrating state.
