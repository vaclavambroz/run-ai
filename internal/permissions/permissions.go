// Package permissions defines the access-mode contract consumed by the CLI and
// provider adapters. It describes intent; provider and OS adapters must enforce
// every field and fail closed when they cannot do so.
package permissions

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

type Mode string

const (
	ModeFull     Mode = "full"
	ModeConfirm  Mode = "confirm"
	ModeReadOnly Mode = "readonly"
)

const policySchema = "run_ai_permission_policy/v1"

var ErrConflictingModes = errors.New("--confirm and --readonly are mutually exclusive")

// Policy is a closed capability declaration. In particular, fixed read
// brokers are distinct from direct network access and credential exposure is
// forbidden in every mode.
type Policy struct {
	Schema                       string `json:"schema"`
	Mode                         Mode   `json:"mode"`
	ProviderPermissionIntent     string `json:"provider_permission_intent"`
	HardReadOnly                 bool   `json:"hard_readonly"`
	AllowFilesystemWrites        bool   `json:"allow_filesystem_writes"`
	AllowExternalMutations       bool   `json:"allow_external_mutations"`
	AllowDirectNetwork           bool   `json:"allow_direct_network"`
	AllowDirectSSH               bool   `json:"allow_direct_ssh"`
	AllowGenericShell            bool   `json:"allow_generic_shell"`
	AllowFixedReadBroker         bool   `json:"allow_fixed_read_broker"`
	AllowApprovalEscalation      bool   `json:"allow_approval_escalation"`
	RequirePerActionConfirmation bool   `json:"require_per_action_confirmation"`
	AllowCredentialExposure      bool   `json:"allow_credential_exposure"`
	WarningCode                  string `json:"warning_code"`
	PolicyDigest                 string `json:"policy_digest"`
}

// Resolve implements the public compatibility decision: neither flag means
// full access, while the two explicit modes are mutually exclusive.
func Resolve(confirm, readonly bool) (Mode, error) {
	if confirm && readonly {
		return "", ErrConflictingModes
	}
	if readonly {
		return ModeReadOnly, nil
	}
	if confirm {
		return ModeConfirm, nil
	}
	return ModeFull, nil
}

func ParseMode(raw string) (Mode, error) {
	mode := Mode(raw)
	if !mode.Valid() {
		return "", fmt.Errorf("unsupported access mode %q", raw)
	}
	return mode, nil
}

func (m Mode) Valid() bool {
	switch m {
	case ModeFull, ModeConfirm, ModeReadOnly:
		return true
	default:
		return false
	}
}

func PolicyFor(mode Mode) (Policy, error) {
	var policy Policy
	switch mode {
	case ModeFull:
		policy = Policy{
			Schema:                   policySchema,
			Mode:                     mode,
			ProviderPermissionIntent: "full_compatibility",
			AllowFilesystemWrites:    true,
			AllowExternalMutations:   true,
			AllowDirectNetwork:       true,
			AllowDirectSSH:           true,
			AllowGenericShell:        true,
			AllowFixedReadBroker:     true,
			AllowApprovalEscalation:  true,
			WarningCode:              "full_access_unsafe",
		}
	case ModeConfirm:
		policy = Policy{
			Schema:                       policySchema,
			Mode:                         mode,
			ProviderPermissionIntent:     "confirm_mutations",
			AllowFilesystemWrites:        true,
			AllowExternalMutations:       true,
			AllowDirectNetwork:           true,
			AllowDirectSSH:               true,
			AllowGenericShell:            true,
			AllowFixedReadBroker:         true,
			AllowApprovalEscalation:      true,
			RequirePerActionConfirmation: true,
			WarningCode:                  "confirmation_required",
		}
	case ModeReadOnly:
		policy = Policy{
			Schema:                   policySchema,
			Mode:                     mode,
			ProviderPermissionIntent: "deny_mutations",
			HardReadOnly:             true,
			AllowFixedReadBroker:     true,
			WarningCode:              "hard_readonly_enforced",
		}
	default:
		return Policy{}, fmt.Errorf("unsupported access mode %q", mode)
	}

	digest, err := digestPolicy(policy)
	if err != nil {
		return Policy{}, err
	}
	policy.PolicyDigest = digest
	return policy, nil
}

func (p Policy) Validate() error {
	if p.Schema != policySchema {
		return fmt.Errorf("invalid permission schema %q", p.Schema)
	}
	canonical, err := PolicyFor(p.Mode)
	if err != nil {
		return err
	}
	if canonical != p {
		return errors.New("permission policy is not canonical")
	}
	return nil
}

func digestPolicy(policy Policy) (string, error) {
	policy.PolicyDigest = ""
	payload, err := json.Marshal(policy)
	if err != nil {
		return "", fmt.Errorf("marshal permission policy: %w", err)
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}
