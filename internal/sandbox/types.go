// Package sandbox implements the hard-readonly execution boundary. It does not
// provide full or confirm pass-through behavior.
package sandbox

import (
	"context"
	"fmt"
	"io/fs"
	"time"
)

type BlockerCode string

const (
	BlockerUnsupportedPlatform BlockerCode = "unsupported_platform"
	BlockerWSLUnsupported      BlockerCode = "wsl_readonly_unproven"
	BlockerMacOSUnavailable    BlockerCode = "macos_adapter_unimplemented"
	BlockerProjectionRequired  BlockerCode = "credential_projection_required"
	BlockerProjectionInvalid   BlockerCode = "credential_projection_invalid"
	BlockerProjectionV2        BlockerCode = "credential_projection_v2_required"
	BlockerWorkspaceInvalid    BlockerCode = "workspace_invalid"
	BlockerBwrapMissing        BlockerCode = "bwrap_missing"
	BlockerBwrapUnsafe         BlockerCode = "bwrap_untrusted"
	BlockerRuntimeDependency   BlockerCode = "readonly_runtime_dependency_missing"
	BlockerProviderUnsupported BlockerCode = "provider_unsupported"
	BlockerSelfTestFailed      BlockerCode = "readonly_self_test_failed"
)

type Blocker struct {
	Code     BlockerCode `json:"code"`
	Platform string      `json:"platform"`
	Detail   string      `json:"detail"`
}

func (b *Blocker) Error() string {
	return fmt.Sprintf("hard-readonly blocker %s on %s: %s", b.Code, b.Platform, b.Detail)
}

type BuildInput struct {
	Provider   string
	Workspace  string
	Projection *ValidatedProjection
}

type Command struct {
	Executable string
	Args       []string
}

type WrapperDescriptor struct {
	Schema                  string   `json:"schema"`
	Provider                string   `json:"provider"`
	Executable              string   `json:"executable"`
	PrefixArgs              []string `json:"prefix_args"`
	ProviderBinary          string   `json:"provider_binary"`
	SandboxWorkingDirectory string   `json:"sandbox_working_directory"`
	ProjectionProfileDigest string   `json:"projection_profile_digest"`
	SelfTestReceiptDigest   string   `json:"self_test_receipt_digest"`
	WrapperDigest           string   `json:"wrapper_digest"`
}

// Wrapper can only be created by Builder.Build after a successful runtime
// self-test. Its executable state is intentionally private.
type Wrapper struct {
	descriptor WrapperDescriptor
	prefixArgs []string
}

func (w *Wrapper) Descriptor() WrapperDescriptor {
	if w == nil {
		return WrapperDescriptor{}
	}
	descriptor := w.descriptor
	descriptor.PrefixArgs = append([]string(nil), w.descriptor.PrefixArgs...)
	return descriptor
}

type ValidatedProjection struct {
	profile    ProjectionProfile
	root       string
	hostSocket string
	device     uint64
	inode      uint64
}

func (v *ValidatedProjection) Profile() ProjectionProfile {
	if v == nil {
		return ProjectionProfile{}
	}
	return v.profile
}

type pathKind uint8

const (
	kindOther pathKind = iota
	kindRegular
	kindDirectory
	kindSocket
	kindSymlink
)

type pathInfo struct {
	Kind   pathKind
	Mode   fs.FileMode
	UID    uint32
	GID    uint32
	Device uint64
	Inode  uint64
}

type runResult struct {
	ExitCode int
	Output   string
}

type commandRunner func(context.Context, string, []string) (runResult, error)
type pathInspector func(string) (pathInfo, error)
type pathLookup func(string) (string, error)
type pathResolver func(string) (string, error)
type fileDigester func(string) (string, error)

type Builder struct {
	goos            string
	wsl             bool
	projectionRoot  string
	inspect         pathInspector
	lookup          pathLookup
	resolve         pathResolver
	digestFile      fileDigester
	run             commandRunner
	selfTestTimeout time.Duration
	// projectionV2Verified is intentionally false in RuntimeBuilder. It is an
	// internal test seam until the fixed broker can verify an exact
	// run/session/workflow/expiry/generation binding. A profile boolean or an
	// opaque receipt digest is not sufficient proof.
	projectionV2Verified bool
}
