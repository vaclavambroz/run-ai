package audit

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vaclavambroz/run-ai/internal/permissions"
	"github.com/vaclavambroz/run-ai/internal/workflows"
)

const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func testSpamProfile() workflows.Profile {
	return workflows.Profile{
		Warnings: workflows.DefaultProfile().Warnings,
		Spam: workflows.SpamPolicy{
			Host:        "mail.example.com",
			Paths:       []string{"/email/view", "/email/raw", "/email/source"},
			Statuses:    []string{"active"},
			MaxMessages: 10,
			MaxURLBytes: 2048,
		},
	}
}

func testPlan(t *testing.T, mode permissions.Mode, spam bool) workflows.Plan {
	t.Helper()
	input := workflows.Input{AccessMode: mode, DevDomain: "example.cz"}
	if spam {
		input.DevDomain = ""
		input.SpamURLs = []string{"https://mail.example.com/email/view?id=1008054&status=active"}
		plan, err := workflows.BuildWithProfile(input, testSpamProfile())
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}
	plan, err := workflows.Build(input)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestReceiptContainsBindingsButNoContentOrURL(t *testing.T) {
	receipt, err := New(Input{
		Event: EventLaunchStarted, Outcome: OutcomeStarted, FaultCode: FaultNone,
		OccurredAt: time.Date(2026, 10, 14, 8, 0, 0, 0, time.UTC),
		RunID:      "run-123", Provider: "codex", Model: "gpt-5.4", SessionID: "session-123",
		CapabilityProfileDigest: digest, Plan: testPlan(t, permissions.ModeReadOnly, true), WarningPresented: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := receipt.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "https://") || strings.Contains(string(payload), "canonical_url") || strings.Contains(string(payload), "prompt") {
		t.Fatalf("receipt leaks URL or prompt: %s", payload)
	}
	if !strings.Contains(string(payload), `"spam_message_ids":[1008054]`) {
		t.Fatalf("receipt lacks safe message identity: %s", payload)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"body", "subject", "recipient", "command", "path", "environment", "credential", "secret"} {
		if _, exists := decoded[forbidden]; exists {
			t.Fatalf("receipt has forbidden field %q", forbidden)
		}
	}
}

func TestFinishedReceiptBindsPreviousAndApproval(t *testing.T) {
	exit := 0
	receipt, err := New(Input{
		Event: EventLaunchFinished, Outcome: OutcomeSucceeded, FaultCode: FaultNone,
		OccurredAt: time.Date(2026, 10, 14, 8, 1, 0, 0, time.UTC),
		RunID:      "run-123", Provider: "claude", Model: "claude-opus-4-6", SessionID: "session-123",
		CapabilityProfileDigest: digest, Plan: testPlan(t, permissions.ModeConfirm, false), WarningPresented: true,
		ApprovalArtifactDigest: digest, PreviousReceiptDigest: digest, ExitCode: &exit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := receipt.Validate(); err != nil {
		t.Fatal(err)
	}
	receipt.TargetRef = "attacker.invalid"
	if err := receipt.Validate(); err == nil {
		t.Fatal("tampered receipt accepted")
	}
}

func TestReceiptRejectsUnsafeOrIncompleteInputs(t *testing.T) {
	base := Input{
		Event: EventLaunchStarted, Outcome: OutcomeStarted, FaultCode: FaultNone,
		OccurredAt: time.Date(2026, 10, 14, 8, 0, 0, 0, time.UTC),
		RunID:      "run-123", Provider: "gemini", Model: "gemini-2.5-pro", SessionID: "session-123",
		CapabilityProfileDigest: digest, Plan: testPlan(t, permissions.ModeFull, false), WarningPresented: true,
	}
	tests := []struct {
		name   string
		mutate func(*Input)
	}{
		{"warning omitted", func(input *Input) { input.WarningPresented = false }},
		{"provider injection", func(input *Input) { input.Provider = "codex\nsecret" }},
		{"session injection", func(input *Input) { input.SessionID = "session path" }},
		{"bad capability digest", func(input *Input) { input.CapabilityProfileDigest = "secret" }},
		{"invalid outcome fault", func(input *Input) { input.FaultCode = FaultRuntime }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := base
			test.mutate(&input)
			if _, err := New(input); err == nil {
				t.Fatal("unsafe receipt accepted")
			}
		})
	}
}

func TestRehashedCrossFieldForgeryRejected(t *testing.T) {
	plan := testPlan(t, permissions.ModeReadOnly, true)
	receipt, err := New(Input{
		Event: EventLaunchStarted, Outcome: OutcomeStarted, FaultCode: FaultNone,
		OccurredAt: time.Date(2026, 10, 14, 8, 0, 0, 0, time.UTC),
		RunID:      "run-123", Provider: "codex", Model: "gpt-5.4", SessionID: "session-123",
		CapabilityProfileDigest: digest, Plan: plan, WarningPresented: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Substitute a different domain into the receipt and self-rehash. Validate()
	// alone cannot detect this (the hash is self-consistent), but ValidateWithPlan
	// cross-checks TargetRef against the plan's Target and rejects the forgery.
	receipt.TargetRef = "attacker.invalid"
	receipt.ReceiptDigest, err = digestReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := receipt.Validate(); err != nil {
		t.Fatal("unexpected: self-rehashed receipt failed structural validation:", err)
	}
	if err := receipt.ValidateWithPlan(plan); err == nil {
		t.Fatal("self-rehashed cross-field domain substitution accepted by plan-backed validation")
	}
}

func TestConfirmedSuccessfulMutationRequiresApproval(t *testing.T) {
	exit := 0
	_, err := New(Input{
		Event: EventLaunchFinished, Outcome: OutcomeSucceeded, FaultCode: FaultNone,
		OccurredAt: time.Date(2026, 10, 14, 8, 1, 0, 0, time.UTC),
		RunID:      "run-123", Provider: "codex", Model: "gpt-5.4", SessionID: "session-123",
		CapabilityProfileDigest: digest, Plan: testPlan(t, permissions.ModeConfirm, false), WarningPresented: true,
		PreviousReceiptDigest: digest, ExitCode: &exit,
	})
	if err == nil {
		t.Fatal("successful confirmed mutation accepted without approval")
	}
}

func TestReceiptCopiesExitCode(t *testing.T) {
	exit := 0
	receipt, err := New(Input{
		Event: EventLaunchFinished, Outcome: OutcomeSucceeded, FaultCode: FaultNone,
		OccurredAt: time.Date(2026, 10, 14, 8, 1, 0, 0, time.UTC),
		RunID:      "run-123", Provider: "antigravity", Model: "gemini-3.1-pro", SessionID: "session-123",
		CapabilityProfileDigest: digest, Plan: testPlan(t, permissions.ModeFull, false), WarningPresented: true,
		PreviousReceiptDigest: digest, ExitCode: &exit,
	})
	if err != nil {
		t.Fatal(err)
	}
	exit = 7
	if receipt.ExitCode == nil || *receipt.ExitCode != 0 {
		t.Fatal("receipt retained caller-owned exit-code pointer")
	}
	if err := receipt.Validate(); err != nil {
		t.Fatal(err)
	}
}
