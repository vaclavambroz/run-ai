package audit

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/vaclavambroz/run-ai/internal/permissions"
	"github.com/vaclavambroz/run-ai/internal/workflows"
)

func TestAppendAndReadAll(t *testing.T) {
	receipt := testReceipt(t)
	path := filepath.Join(t.TempDir(), "state", "audit.jsonl")
	if err := Append(path, receipt); err != nil {
		t.Fatal(err)
	}
	items, err := ReadAll(path, 1<<20)
	if err != nil || len(items) != 1 || items[0].ReceiptDigest != receipt.ReceiptDigest {
		t.Fatalf("read = %#v, %v", items, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, err=%v", info.Mode(), err)
	}
}

func testReceipt(t *testing.T) Receipt {
	t.Helper()
	plan, err := workflows.Build(workflows.Input{AccessMode: permissions.ModeReadOnly, DevDomain: "example.cz"})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := New(Input{
		Event: EventLaunchStarted, Outcome: OutcomeStarted, FaultCode: FaultNone,
		OccurredAt: time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC),
		RunID:      "run-12345678", Provider: "codex", Model: "configured",
		SessionID: "session-12345678", CapabilityProfileDigest: string(make([]byte, 0)),
		Plan: plan, WarningPresented: true,
	})
	// Capability digests are deliberately not invented by the log writer.
	if err == nil {
		t.Fatal("fixture with empty capability digest must fail")
	}
	receipt, err = New(Input{
		Event: EventLaunchStarted, Outcome: OutcomeStarted, FaultCode: FaultNone,
		OccurredAt: time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC),
		RunID:      "run-12345678", Provider: "codex", Model: "configured",
		SessionID: "session-12345678", CapabilityProfileDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Plan: plan, WarningPresented: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func TestAppendRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(state, "audit.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := Append(filepath.Join(state, "audit.jsonl"), testReceipt(t)); err == nil {
		t.Fatal("symlink append unexpectedly succeeded")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "untouched" {
		t.Fatal("symlink target changed")
	}
}

func TestAppendAndReadRejectSymlinkedAncestor(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	realState := filepath.Join(root, "real-state")
	if err := os.Mkdir(realState, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedState := filepath.Join(root, "linked-state")
	if err := os.Symlink(realState, linkedState); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(linkedState, "nested", "audit.jsonl")
	if err := Append(path, testReceipt(t)); err == nil {
		t.Fatal("append through symlinked ancestor unexpectedly succeeded")
	}
	if _, err := ReadAll(path, 1<<20); err == nil {
		t.Fatal("read through symlinked ancestor unexpectedly succeeded")
	}
	if _, err := os.Lstat(filepath.Join(realState, "nested")); !os.IsNotExist(err) {
		t.Fatalf("symlink target was mutated: %v", err)
	}
}
