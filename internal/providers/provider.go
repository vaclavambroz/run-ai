package providers

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/vaclavambroz/run-ai/internal/permissions"
)

// Name is the canonical provider identity used by session maps and tmux.
// Gemini is intentionally normalized to Antigravity for compatibility with
// the existing run_gemini -> run_antigravity launcher chain.
type Name string

const (
	Codex       Name = "codex"
	Claude      Name = "claude"
	Antigravity Name = "antigravity"
)

type Spec struct {
	Name          Name
	DisplayName   string
	DefaultBinary string
	DefaultModel  string
	LatestModel   string
	Aliases       []string
}

var specs = map[Name]Spec{
	Codex: {
		Name: Codex, DisplayName: "Codex", DefaultBinary: "codex",
		DefaultModel: "gpt-5.6-terra", LatestModel: "gpt-5.6-sol",
		Aliases: []string{"codex"},
	},
	Claude: {
		Name: Claude, DisplayName: "Claude", DefaultBinary: "claude",
		DefaultModel: "sonnet", LatestModel: "fable",
		Aliases: []string{"claude"},
	},
	Antigravity: {
		Name: Antigravity, DisplayName: "Gemini/Antigravity", DefaultBinary: "agy",
		DefaultModel: "gemini-2.5-pro", LatestModel: "gemini-3.1-pro-preview",
		Aliases: []string{"antigravity", "gemini"},
	},
}

func Parse(value string) (Name, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "codex":
		return Codex, nil
	case "claude":
		return Claude, nil
	case "antigravity", "gemini":
		return Antigravity, nil
	default:
		return "", fmt.Errorf("unsupported provider %q (want codex, claude, or gemini)", value)
	}
}

func Lookup(name Name) (Spec, bool) {
	spec, ok := specs[name]
	return spec, ok
}

func MustLookup(name Name) Spec {
	spec, ok := Lookup(name)
	if !ok {
		panic(fmt.Sprintf("unknown canonical provider %q", name))
	}
	return spec
}

func All() []Spec {
	return []Spec{specs[Codex], specs[Claude], specs[Antigravity]}
}

type ModelTrack string

const (
	ModelConfigured ModelTrack = "configured"
	ModelDefault    ModelTrack = "default"
	ModelLatest     ModelTrack = "latest"
	ModelExplicit   ModelTrack = "explicit"
)

type LaunchOptions struct {
	Binary           string
	WorkingDirectory string
	AccessMode       permissions.Mode
	ModelTrack       ModelTrack
	Model            string
	ResumeID         string
	NewSessionID     string
	SessionName      string
	Prompt           string
	ContextDirs      []string
}

type Invocation struct {
	Provider         Name     `json:"provider"`
	Binary           string   `json:"binary"`
	Args             []string `json:"args"`
	WorkingDirectory string   `json:"working_directory"`
}

// BuildInvocation returns argv without executing it or using a shell.
func BuildInvocation(provider Name, options LaunchOptions) (Invocation, error) {
	spec, ok := Lookup(provider)
	if !ok {
		return Invocation{}, fmt.Errorf("unknown provider %q", provider)
	}
	if !filepath.IsAbs(options.WorkingDirectory) {
		return Invocation{}, errors.New("working directory must be absolute")
	}
	if options.Prompt == "" {
		return Invocation{}, errors.New("provider prompt is empty")
	}
	if _, err := permissions.PolicyFor(options.AccessMode); err != nil {
		return Invocation{}, err
	}
	binary := options.Binary
	if binary == "" {
		binary = spec.DefaultBinary
	}
	model, err := resolveModel(spec, options.ModelTrack, options.Model)
	if err != nil {
		return Invocation{}, err
	}
	invocation := Invocation{Provider: provider, Binary: binary, WorkingDirectory: options.WorkingDirectory}
	switch provider {
	case Codex:
		invocation.Args = append(invocation.Args, codexAccessArgs(options.AccessMode)...)
		invocation.Args = append(invocation.Args, "--no-alt-screen", "-C", options.WorkingDirectory)
		if model != "" {
			invocation.Args = append(invocation.Args, "--model", model)
		}
		invocation.Args = append(invocation.Args, "--disable", "shell_snapshot")
		if options.ResumeID != "" {
			invocation.Args = append(invocation.Args, "resume", options.ResumeID, options.Prompt)
		} else {
			invocation.Args = append(invocation.Args, options.Prompt)
		}
	case Claude:
		invocation.Args = append(invocation.Args, claudeAccessArgs(options.AccessMode)...)
		if model != "" {
			invocation.Args = append(invocation.Args, "--model", model)
		}
		for _, directory := range uniqueDirs(append(options.ContextDirs, options.WorkingDirectory)) {
			invocation.Args = append(invocation.Args, "--add-dir", directory)
		}
		if options.ResumeID != "" {
			invocation.Args = append(invocation.Args, "--resume", options.ResumeID)
		} else if options.NewSessionID != "" {
			invocation.Args = append(invocation.Args, "--session-id", options.NewSessionID)
			if options.SessionName != "" {
				invocation.Args = append(invocation.Args, "--name", options.SessionName)
			}
		}
		invocation.Args = append(invocation.Args, options.Prompt)
	case Antigravity:
		invocation.Args = append(invocation.Args, antigravityAccessArgs(options.AccessMode)...)
		if model != "" {
			invocation.Args = append(invocation.Args, "--model", model)
		}
		for _, directory := range uniqueDirs(append(options.ContextDirs, options.WorkingDirectory)) {
			invocation.Args = append(invocation.Args, "--add-dir", directory)
		}
		if options.ResumeID != "" {
			invocation.Args = append(invocation.Args, "--conversation", options.ResumeID)
		}
		invocation.Args = append(invocation.Args, "--prompt-interactive", options.Prompt)
	}
	return invocation, nil
}

func resolveModel(spec Spec, track ModelTrack, explicit string) (string, error) {
	switch track {
	case "", ModelConfigured:
		if explicit != "" {
			return "", errors.New("configured model track cannot carry an explicit model")
		}
		// Until the typed config/profile loader is wired into the executable,
		// use the exact legacy baseline rather than a mutable vendor default.
		return spec.DefaultModel, nil
	case ModelDefault:
		return spec.DefaultModel, nil
	case ModelLatest:
		return spec.LatestModel, nil
	case ModelExplicit:
		if strings.TrimSpace(explicit) == "" {
			return "", errors.New("explicit model is empty")
		}
		return explicit, nil
	default:
		return "", fmt.Errorf("unsupported model track %q", track)
	}
}

// ResolveModel exposes the same stable model selection used by the provider
// argv builders to transports such as the Codex app-server.
func ResolveModel(provider Name, track ModelTrack, explicit string) (string, error) {
	spec, ok := Lookup(provider)
	if !ok {
		return "", fmt.Errorf("unknown provider %q", provider)
	}
	return resolveModel(spec, track, explicit)
}

func codexAccessArgs(mode permissions.Mode) []string {
	switch mode {
	case permissions.ModeFull:
		return []string{"--dangerously-bypass-approvals-and-sandbox"}
	case permissions.ModeConfirm:
		return []string{"--sandbox", "workspace-write", "--ask-for-approval", "on-request"}
	case permissions.ModeReadOnly:
		return []string{"--sandbox", "read-only", "--ask-for-approval", "never"}
	default:
		return nil
	}
}

func claudeAccessArgs(mode permissions.Mode) []string {
	switch mode {
	case permissions.ModeFull:
		return []string{"--permission-mode", "dontAsk", "--allowedTools", "Bash(*) Read Edit Write Glob Grep"}
	case permissions.ModeConfirm:
		return []string{"--permission-mode", "default"}
	case permissions.ModeReadOnly:
		return []string{"--permission-mode", "plan", "--tools", "Read,Glob,Grep", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--no-chrome"}
	default:
		return nil
	}
}

func antigravityAccessArgs(mode permissions.Mode) []string {
	switch mode {
	case permissions.ModeFull:
		return []string{"--dangerously-skip-permissions"}
	case permissions.ModeConfirm:
		return []string{"--mode", "auto", "--sandbox"}
	case permissions.ModeReadOnly:
		return []string{"--mode", "plan", "--sandbox"}
	default:
		return nil
	}
}

func uniqueDirs(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || !filepath.IsAbs(value) {
			continue
		}
		value = filepath.Clean(value)
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
