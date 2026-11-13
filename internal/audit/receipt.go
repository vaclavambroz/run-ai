// Package audit creates canonical, secret-free receipts. It deliberately has
// no generic metadata, message, prompt, URL, command, path, environment, or
// credential fields.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/vaclavambroz/run-ai/internal/permissions"
	"github.com/vaclavambroz/run-ai/internal/workflows"
)

const receiptSchema = "run_ai_audit_receipt/v1"

type Event string
type Outcome string
type FaultCode string

const (
	EventLaunchStarted  Event = "launch_started"
	EventLaunchFinished Event = "launch_finished"

	OutcomeStarted   Outcome = "started"
	OutcomeSucceeded Outcome = "succeeded"
	OutcomeFailed    Outcome = "failed"
	OutcomeDenied    Outcome = "denied"

	FaultNone         FaultCode = "none"
	FaultPolicy       FaultCode = "policy_denied"
	FaultPrerequisite FaultCode = "prerequisite_missing"
	FaultAuth         FaultCode = "authentication_failed"
	FaultCollision    FaultCode = "session_collision"
	FaultRuntime      FaultCode = "provider_runtime_failed"
)

var (
	safeIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	hexDigest      = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

type Input struct {
	Event                   Event
	Outcome                 Outcome
	FaultCode               FaultCode
	OccurredAt              time.Time
	RunID                   string
	Provider                string
	Model                   string
	SessionID               string
	CapabilityProfileDigest string
	Plan                    workflows.Plan
	WarningPresented        bool
	ApprovalArtifactDigest  string
	PreviousReceiptDigest   string
	ExitCode                *int
}

type Receipt struct {
	Schema                  string                  `json:"schema"`
	Event                   Event                   `json:"event"`
	Outcome                 Outcome                 `json:"outcome"`
	FaultCode               FaultCode               `json:"fault_code"`
	OccurredAt              string                  `json:"occurred_at"`
	RunID                   string                  `json:"run_id"`
	Provider                string                  `json:"provider"`
	Model                   string                  `json:"model"`
	SessionID               string                  `json:"session_id"`
	AccessMode              permissions.Mode        `json:"access_mode"`
	WorkflowKind            workflows.Kind          `json:"workflow_kind"`
	WorkflowPlanDigest      string                  `json:"workflow_plan_digest"`
	PermissionPolicyDigest  string                  `json:"permission_policy_digest"`
	CapabilityProfileDigest string                  `json:"capability_profile_digest"`
	ExecutionMode           workflows.ExecutionMode `json:"execution_mode"`
	MutationAllowed         bool                    `json:"mutation_allowed"`
	TargetRef               string                  `json:"target_ref,omitempty"`
	SpamMessageIDs          []uint64                `json:"spam_message_ids,omitempty"`
	WarningCode             string                  `json:"warning_code"`
	WarningPresented        bool                    `json:"warning_presented"`
	ApprovalArtifactDigest  string                  `json:"approval_artifact_digest,omitempty"`
	PreviousReceiptDigest   string                  `json:"previous_receipt_digest,omitempty"`
	ExitCode                *int                    `json:"exit_code,omitempty"`
	ReceiptDigest           string                  `json:"receipt_digest"`
}

func New(input Input) (Receipt, error) {
	if err := input.Plan.Validate(); err != nil {
		return Receipt{}, fmt.Errorf("invalid workflow plan: %w", err)
	}
	receipt := Receipt{
		Schema:                  receiptSchema,
		Event:                   input.Event,
		Outcome:                 input.Outcome,
		FaultCode:               input.FaultCode,
		OccurredAt:              input.OccurredAt.UTC().Format(time.RFC3339Nano),
		RunID:                   input.RunID,
		Provider:                input.Provider,
		Model:                   input.Model,
		SessionID:               input.SessionID,
		AccessMode:              input.Plan.AccessMode,
		WorkflowKind:            input.Plan.Kind,
		WorkflowPlanDigest:      input.Plan.PlanDigest,
		PermissionPolicyDigest:  input.Plan.PermissionPolicyDigest,
		CapabilityProfileDigest: input.CapabilityProfileDigest,
		ExecutionMode:           input.Plan.ExecutionMode,
		MutationAllowed:         input.Plan.MutationAllowed,
		TargetRef:               input.Plan.Target,
		WarningCode:             input.Plan.WarningCode,
		WarningPresented:        input.WarningPresented,
		ApprovalArtifactDigest:  input.ApprovalArtifactDigest,
		PreviousReceiptDigest:   input.PreviousReceiptDigest,
	}
	if input.ExitCode != nil {
		exitCode := *input.ExitCode
		receipt.ExitCode = &exitCode
	}
	for _, reference := range input.Plan.SpamReferences {
		receipt.SpamMessageIDs = append(receipt.SpamMessageIDs, reference.MessageID)
	}
	if err := receipt.validateFields(); err != nil {
		return Receipt{}, err
	}
	digest, err := digestReceipt(receipt)
	if err != nil {
		return Receipt{}, err
	}
	receipt.ReceiptDigest = digest
	return receipt, nil
}

func (r Receipt) Validate() error {
	if err := r.validateFields(); err != nil {
		return err
	}
	digest, err := digestReceipt(r)
	if err != nil {
		return err
	}
	if r.ReceiptDigest != digest {
		return errors.New("audit receipt digest mismatch")
	}
	return nil
}

// ValidateWithPlan performs all Validate checks and additionally cross-validates
// that the receipt's TargetRef matches the plan's Target, binding the receipt to
// the specific plan that produced it. This catches domain-substitution forgeries
// that self-rehashing alone cannot detect.
func (r Receipt) ValidateWithPlan(plan workflows.Plan) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if r.WorkflowPlanDigest != plan.PlanDigest {
		return errors.New("receipt plan digest does not match supplied plan")
	}
	if r.TargetRef != plan.Target {
		return errors.New("receipt target is inconsistent with plan")
	}
	return nil
}

func (r Receipt) CanonicalJSON() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(r)
}

func (r Receipt) validateFields() error {
	if r.Schema != receiptSchema {
		return errors.New("invalid audit receipt schema")
	}
	if r.Event != EventLaunchStarted && r.Event != EventLaunchFinished {
		return errors.New("invalid audit event")
	}
	if !slices.Contains([]Outcome{OutcomeStarted, OutcomeSucceeded, OutcomeFailed, OutcomeDenied}, r.Outcome) {
		return errors.New("invalid audit outcome")
	}
	if !slices.Contains([]FaultCode{FaultNone, FaultPolicy, FaultPrerequisite, FaultAuth, FaultCollision, FaultRuntime}, r.FaultCode) {
		return errors.New("invalid audit fault code")
	}
	if (r.Outcome == OutcomeStarted || r.Outcome == OutcomeSucceeded) != (r.FaultCode == FaultNone) {
		return errors.New("outcome/fault mismatch")
	}
	if r.Event == EventLaunchStarted && r.Outcome != OutcomeStarted {
		return errors.New("launch_started requires started outcome")
	}
	if r.Event == EventLaunchFinished && r.Outcome == OutcomeStarted {
		return errors.New("launch_finished cannot have started outcome")
	}
	if r.Event == EventLaunchStarted && (r.PreviousReceiptDigest != "" || r.ExitCode != nil) {
		return errors.New("launch_started cannot have previous receipt or exit code")
	}
	if r.Event == EventLaunchFinished && !validDigest(r.PreviousReceiptDigest) {
		return errors.New("launch_finished requires previous receipt digest")
	}
	if r.OccurredAt == "" {
		return errors.New("occurred_at is required")
	}
	parsedTime, err := time.Parse(time.RFC3339Nano, r.OccurredAt)
	if err != nil || parsedTime.IsZero() || parsedTime.UTC().Format(time.RFC3339Nano) != r.OccurredAt {
		return errors.New("occurred_at must be canonical UTC RFC3339")
	}
	if !safeIdentifier.MatchString(r.RunID) || !safeIdentifier.MatchString(r.SessionID) || !safeIdentifier.MatchString(r.Model) {
		return errors.New("run, session, and model identifiers must be bounded canonical identifiers")
	}
	if !slices.Contains([]string{"codex", "claude", "gemini", "antigravity"}, r.Provider) {
		return errors.New("invalid provider")
	}
	if r.Event == EventLaunchFinished {
		switch r.Outcome {
		case OutcomeSucceeded:
			if r.ExitCode == nil || *r.ExitCode != 0 {
				return errors.New("successful launch requires exit code zero")
			}
		case OutcomeFailed:
			if r.ExitCode == nil || *r.ExitCode < 1 || *r.ExitCode > 255 {
				return errors.New("failed launch requires bounded nonzero exit code")
			}
		case OutcomeDenied:
			if r.ExitCode != nil {
				return errors.New("denied launch cannot carry provider exit code")
			}
		}
	}
	policy, err := permissions.PolicyFor(r.AccessMode)
	if err != nil || !validDigest(r.WorkflowPlanDigest) || r.PermissionPolicyDigest != policy.PolicyDigest || !validDigest(r.CapabilityProfileDigest) {
		return errors.New("invalid policy or plan binding")
	}
	if r.WarningCode != policy.WarningCode || !r.WarningPresented {
		return errors.New("security warning was not presented")
	}
	if r.ApprovalArtifactDigest != "" && !validDigest(r.ApprovalArtifactDigest) {
		return errors.New("invalid approval artifact digest")
	}
	if r.AccessMode == permissions.ModeConfirm && r.Event == EventLaunchFinished && r.Outcome == OutcomeSucceeded &&
		r.MutationAllowed && !validDigest(r.ApprovalArtifactDigest) {
		return errors.New("successful confirmed mutation requires approval artifact digest")
	}
	if r.AccessMode == permissions.ModeReadOnly && (r.MutationAllowed || r.ApprovalArtifactDigest != "") {
		return errors.New("readonly receipt contains mutation or approval")
	}
	switch r.WorkflowKind {
	case workflows.KindNone:
		if r.TargetRef != "" || len(r.SpamMessageIDs) != 0 || r.ExecutionMode != receiptExecution(r.AccessMode, true) || r.MutationAllowed != policy.AllowExternalMutations {
			return errors.New("invalid generic workflow receipt")
		}
	case workflows.KindMonitoring:
		if r.AccessMode == permissions.ModeReadOnly || r.TargetRef != "monitoring" || len(r.SpamMessageIDs) != 0 ||
			r.ExecutionMode != receiptExecution(r.AccessMode, false) || !r.MutationAllowed {
			return errors.New("invalid monitoring workflow receipt")
		}
	case workflows.KindSpam:
		if len(r.SpamMessageIDs) == 0 || len(r.SpamMessageIDs) > 10 {
			return errors.New("invalid spam message identity set")
		}
		if r.TargetRef == "" || r.MutationAllowed != policy.AllowExternalMutations {
			return errors.New("invalid spam workflow receipt")
		}
		expected := workflows.ExecutionTwoPhaseApply
		if r.AccessMode == permissions.ModeReadOnly {
			expected = workflows.ExecutionAnalyzeOnly
		} else if r.AccessMode == permissions.ModeConfirm {
			expected = workflows.ExecutionTwoPhaseConfirm
		}
		if r.ExecutionMode != expected {
			return errors.New("invalid spam execution receipt")
		}
	case workflows.KindDev:
		if _, err := workflows.ValidateDomain(r.TargetRef); err != nil || len(r.SpamMessageIDs) != 0 ||
			r.ExecutionMode != receiptExecution(r.AccessMode, false) || r.MutationAllowed != policy.AllowExternalMutations {
			return errors.New("invalid development workflow receipt")
		}
	default:
		return errors.New("invalid workflow kind")
	}
	for index, messageID := range r.SpamMessageIDs {
		if messageID == 0 || (index > 0 && slices.Contains(r.SpamMessageIDs[:index], messageID)) {
			return errors.New("invalid or duplicate spam message identity")
		}
	}
	if r.ReceiptDigest != "" && !validDigest(r.ReceiptDigest) {
		return errors.New("invalid receipt digest")
	}
	return nil
}

func validDigest(value string) bool { return hexDigest.MatchString(value) }

func receiptExecution(mode permissions.Mode, interactive bool) workflows.ExecutionMode {
	if mode == permissions.ModeReadOnly {
		return workflows.ExecutionAnalyzeOnly
	}
	if mode == permissions.ModeConfirm {
		return workflows.ExecutionConfirmed
	}
	if interactive {
		return workflows.ExecutionInteractive
	}
	return workflows.ExecutionDirect
}

func digestReceipt(receipt Receipt) (string, error) {
	receipt.ReceiptDigest = ""
	payload, err := json.Marshal(receipt)
	if err != nil {
		return "", fmt.Errorf("marshal audit receipt: %w", err)
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}
