package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestEnsureAppSupervisorReapsUnreadyChild(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "unready-supervisor")
	// This behaves like a supervisor stuck before creating its Unix socket.
	// It is its own session leader under ensureAppSupervisor, so the test also
	// exercises process-group cleanup rather than merely killing a shell.
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntrap 'exit 0' TERM INT\nwhile :; do :; done\n"), 0700); err != nil {
		t.Fatal(err)
	}
	oldFactory := newAppSupervisorCommand
	var child *exec.Cmd
	newAppSupervisorCommand = func(string, string) *exec.Cmd {
		child = exec.Command(script)
		return child
	}
	t.Cleanup(func() { newAppSupervisorCommand = oldFactory })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := ensureAppSupervisor(ctx, filepath.Join(dir, "supervisor.sock"), "", appSupervisorLaunch{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ensure error = %v, want context deadline", err)
	}
	if child == nil || child.ProcessState == nil || !child.ProcessState.Exited() {
		t.Fatalf("unready supervisor was not reaped: %#v", child)
	}
}
