package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
)

const (
	projectionSchema    = "run_ai_provider_credential_projection/v1"
	projectionTransport = "unix_provider_api_broker"
	projectionProtocol  = "run_ai_provider_inference_broker/v1"
	sandboxBrokerSocket = "/run/run-ai/provider-api.sock"
)

// ProjectionProfile describes an external broker that owns provider
// credentials. A credential file or directory is intentionally not a valid
// projection: bwrap cannot hide such a file from model-created child
// processes while exposing it to the provider process in the same namespace.
type ProjectionProfile struct {
	Schema                      string `json:"schema"`
	Provider                    string `json:"provider"`
	Transport                   string `json:"transport"`
	Protocol                    string `json:"protocol"`
	SocketName                  string `json:"socket_name"`
	SandboxSocket               string `json:"sandbox_socket"`
	ExpectedUID                 uint32 `json:"expected_uid"`
	ExpectedGID                 uint32 `json:"expected_gid"`
	ExpectedMode                uint32 `json:"expected_mode"`
	ProviderAdapter             string `json:"provider_adapter"`
	ProviderAdapterDigest       string `json:"provider_adapter_digest"`
	CredentialMaterialVisible   bool   `json:"credential_material_visible"`
	DirectNetworkRequired       bool   `json:"direct_network_required"`
	SessionBindingRequired      bool   `json:"session_binding_required"`
	CredentialExportAllowed     bool   `json:"credential_export_allowed"`
	ArbitraryDestinationAllowed bool   `json:"arbitrary_destination_allowed"`
	ValidatorReceiptDigest      string `json:"validator_receipt_digest"`
	ProfileDigest               string `json:"profile_digest"`
}

func NewProjectionProfile(provider string, expectedUID, expectedGID uint32, expectedMode fs.FileMode, validatorReceiptDigest, providerAdapterDigest string) (ProjectionProfile, error) {
	socketName, ok := providerSocketNames[provider]
	if !ok {
		return ProjectionProfile{}, fmt.Errorf("unsupported projection provider %q", provider)
	}
	profile := ProjectionProfile{
		Schema:                 projectionSchema,
		Provider:               provider,
		Transport:              projectionTransport,
		Protocol:               projectionProtocol,
		SocketName:             socketName,
		SandboxSocket:          sandboxBrokerSocket,
		ExpectedUID:            expectedUID,
		ExpectedGID:            expectedGID,
		ExpectedMode:           uint32(expectedMode.Perm()),
		ProviderAdapter:        providerAdapters[provider],
		ProviderAdapterDigest:  providerAdapterDigest,
		SessionBindingRequired: true,
		ValidatorReceiptDigest: validatorReceiptDigest,
	}
	digest, err := digestProjectionProfile(profile)
	if err != nil {
		return ProjectionProfile{}, err
	}
	profile.ProfileDigest = digest
	if err := profile.Validate(); err != nil {
		return ProjectionProfile{}, err
	}
	return profile, nil
}

func (p ProjectionProfile) Validate() error {
	if p.Schema != projectionSchema || p.Transport != projectionTransport || p.Protocol != projectionProtocol || p.SandboxSocket != sandboxBrokerSocket {
		return errors.New("invalid credential projection identity")
	}
	wantSocket, ok := providerSocketNames[p.Provider]
	if !ok || p.SocketName != wantSocket || filepath.Base(p.SocketName) != p.SocketName {
		return errors.New("invalid provider projection socket")
	}
	if p.CredentialMaterialVisible || p.DirectNetworkRequired || !p.SessionBindingRequired || p.CredentialExportAllowed || p.ArbitraryDestinationAllowed {
		return errors.New("projection would expose credentials, allow an unbound session, or require arbitrary network")
	}
	if p.ExpectedMode != 0o600 && p.ExpectedMode != 0o660 {
		return errors.New("projection socket mode must be 0600 or 0660")
	}
	if p.ProviderAdapter != providerAdapters[p.Provider] || !validDigest(p.ProviderAdapterDigest) {
		return errors.New("provider broker adapter is not canonically bound")
	}
	if !validDigest(p.ValidatorReceiptDigest) || !validDigest(p.ProfileDigest) {
		return errors.New("projection proof digest is missing or invalid")
	}
	digest, err := digestProjectionProfile(p)
	if err != nil {
		return err
	}
	if p.ProfileDigest != digest {
		return errors.New("credential projection profile digest mismatch")
	}
	return nil
}

func digestProjectionProfile(profile ProjectionProfile) (string, error) {
	profile.ProfileDigest = ""
	payload, err := json.Marshal(profile)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

var providerSocketNames = map[string]string{
	"codex":       "codex.sock",
	"claude":      "claude.sock",
	"antigravity": "antigravity.sock",
}

var providerAdapters = map[string]string{
	"codex":       "/usr/local/libexec/run-ai/codex-readonly",
	"claude":      "/usr/local/libexec/run-ai/claude-readonly",
	"antigravity": "/usr/local/libexec/run-ai/antigravity-readonly",
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}
