package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContinuationBoundedFakeCodex(t *testing.T) {
	d := t.TempDir()
	bin := filepath.Join(d, "codex")
	script := "#!/bin/sh\nN=$(cat \"$RUN_AI_TEST_N\" 2>/dev/null || echo 0); N=$((N+1)); echo $N >\"$RUN_AI_TEST_N\"; if [ $N -lt 3 ]; then echo 'RUN_AI_CONTINUATION_STATE: checkpoint'; else echo 'RUN_AI_CONTINUATION_STATE: completed'; fi\n"
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	n := filepath.Join(d, "n")
	p := filepath.Join(d, "IMPLEMENTATION_PROGRESS.md")
	os.WriteFile(p, []byte("- unfinished"), 0600)
	log := filepath.Join(d, "log")
	old := os.Getenv("RUN_AI_CONTINUATION_CODEX_BIN")
	oldN := os.Getenv("RUN_AI_TEST_N")
	defer os.Setenv("RUN_AI_CONTINUATION_CODEX_BIN", old)
	defer os.Setenv("RUN_AI_TEST_N", oldN)
	os.Setenv("RUN_AI_CONTINUATION_CODEX_BIN", bin)
	os.Setenv("RUN_AI_TEST_N", n)
	if rc := runContinuation([]string{"--session-id", "test-session", "--progress", p, "--log", log, "--max-turns", "12", "--turn-timeout", "1s", "--total-timeout", "10s"}, os.Stdout, os.Stderr); rc != exitOK {
		t.Fatalf("rc=%d", rc)
	}
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "turn=3 state=completed") {
		t.Fatalf("log=%s", b)
	}
}
