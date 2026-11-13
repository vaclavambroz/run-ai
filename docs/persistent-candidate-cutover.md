## Persistent Session Candidate Cutover

### Overview

This document describes how to cut over from one Codex binary candidate to another in a running persistent session, without losing the conversation. The existing Codex process and conversation state are preserved; only the supervisor and the binary used for future reconnects are replaced.

### Prerequisites

- A running persistent session (see [persistent-codex.md](persistent-codex.md)).
- A candidate binary prepared with `packaging/release/prepare-local-candidate.sh`.
- The candidate's SHA256 digest from `candidate.json`.

### Steps

**1. Prepare the candidate**

Build or download the candidate binary and prepare a candidate directory:

```
sh packaging/release/prepare-local-candidate.sh ./run_ai_new /path/to/candidate
```

This creates `candidate/run_ai`, `candidate/candidate.json`, and `candidate/SHA256SUMS`.

**2. Verify the candidate digest**

```
sha256sum candidate/run_ai
cat candidate/SHA256SUMS
```

Confirm the digest in `SHA256SUMS` matches `candidate_sha256` in `candidate.json` and the `sha256sum` output.

**3. Signal the supervisor**

Send the supervisor a cutover request by writing the candidate directory path to the supervisor's control socket or by using the cutover subcommand (if available in this build):

```
run_ai persistent cutover <session-id> /path/to/candidate
```

The supervisor validates the candidate digest before accepting it.

**4. Monitor reconnection**

After signaling the cutover, the supervisor replaces its binary reference and restarts the tmux integration layer without terminating the Codex process. Monitor the state directory and session log for confirmation:

```
run_ai sessions list
```

The session should remain listed with the same session ID. Reconnect to verify the conversation is intact:

```
run_ai --persistent <session-id>
```

### Notes

- The Codex process itself is not restarted during cutover. Only the supervisor is updated.
- If the candidate fails validation (bad digest, wrong version), the supervisor rejects the cutover and continues with the existing binary.
- After a successful cutover, the previous binary is no longer referenced by the supervisor. Keep it available on disk if you need to roll back manually.
