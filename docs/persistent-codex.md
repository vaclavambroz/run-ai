## Persistent Codex Sessions

### Overview

A persistent Codex session uses a reconnectable app-server supervisor that keeps the Codex process alive between terminal sessions. The supervisor manages a local Unix socket; connecting to it resumes an existing Codex conversation without restarting the process.

Persistent sessions are restricted to Codex/full mode. Team, remote, monitoring, and spam workflows are not supported in persistent mode.

### Starting a persistent session

Pass `--persistent --new-session` to create a new persistent session:

```
run_ai --persistent --new-session codex
```

This starts the supervisor and attaches to a new Codex session. The session ID is printed and recorded in the state directory.

### Reconnecting

To reconnect to an existing persistent session, pass `--persistent` with the session ID:

```
run_ai --persistent <session-id>
```

If the supervisor is still running, the command attaches to the existing Codex process. The conversation state is preserved.

To list available persistent sessions:

```
run_ai sessions list
```

### How it works

The supervisor process listens on a Unix socket in the state directory. When you disconnect from the terminal (close the window, end SSH, etc.), the supervisor and the Codex process continue running. Reconnecting creates a new tmux attachment to the existing session.

The supervisor records a sealed workflow manifest in the state directory. This manifest is re-validated on each reconnect to ensure the session has not been tampered with.

### Restrictions

- Persistent mode requires `--full` access (the default). `--confirm` and `--readonly` are not supported.
- Only one persistent session per session ID can run at a time. Starting a second session with the same ID fails.
- To replace a running session with a new binary candidate, see [persistent-candidate-cutover.md](persistent-candidate-cutover.md).
