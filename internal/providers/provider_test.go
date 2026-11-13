package providers

import (
	"slices"
	"testing"

	"github.com/vaclavambroz/run-ai/internal/permissions"
)

func TestParseNormalizesProviderAliases(t *testing.T) {
	t.Parallel()
	tests := map[string]Name{
		"codex":       Codex,
		"CODEX":       Codex,
		"claude":      Claude,
		"gemini":      Antigravity,
		"antigravity": Antigravity,
	}
	for input, want := range tests {
		input, want := input, want
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(input)
			if err != nil {
				t.Fatalf("Parse(%q): %v", input, err)
			}
			if got != want {
				t.Fatalf("Parse(%q) = %q, want %q", input, got, want)
			}
		})
	}
}

func TestBuildInvocationModesModelsAndResume(t *testing.T) {
	t.Parallel()
	tests := []struct {
		provider Name
		mode     permissions.Mode
		track    ModelTrack
		model    string
		resume   string
		contains []string
	}{
		{Codex, permissions.ModeFull, ModelDefault, "", "codex-id", []string{"--dangerously-bypass-approvals-and-sandbox", "gpt-5.6-terra", "resume", "codex-id"}},
		{Codex, permissions.ModeReadOnly, ModelLatest, "", "", []string{"--sandbox", "read-only", "--ask-for-approval", "never", "gpt-5.6-sol"}},
		{Claude, permissions.ModeConfirm, ModelExplicit, "opus", "claude-id", []string{"--permission-mode", "default", "opus", "--resume", "claude-id"}},
		{Claude, permissions.ModeReadOnly, ModelConfigured, "", "", []string{"--permission-mode", "plan", "--tools", "Read,Glob,Grep", "--strict-mcp-config"}},
		{Antigravity, permissions.ModeFull, ModelDefault, "", "conversation.db", []string{"--dangerously-skip-permissions", "gemini-2.5-pro", "--conversation", "conversation.db"}},
		{Antigravity, permissions.ModeConfirm, ModelLatest, "", "", []string{"--mode", "auto", "--sandbox", "gemini-3.1-pro-preview"}},
	}
	for _, test := range tests {
		invocation, err := BuildInvocation(test.provider, LaunchOptions{
			WorkingDirectory: "/work", AccessMode: test.mode,
			ModelTrack: test.track, Model: test.model, ResumeID: test.resume,
			Prompt: "prompt", ContextDirs: []string{"/var/www/app"},
		})
		if err != nil {
			t.Fatalf("%s: %v", test.provider, err)
		}
		for _, value := range test.contains {
			if !slices.Contains(invocation.Args, value) {
				t.Fatalf("%s args %q do not contain %q", test.provider, invocation.Args, value)
			}
		}
	}
}

// The standard Codex path must invoke the native interactive CLI directly.
// The app-server is a separately requested --persistent transport; inserting
// either `exec` or `app-server` here would silently replace the full Codex TUI.
func TestCodexStandardInvocationUsesNativeInteractiveTUI(t *testing.T) {
	t.Parallel()
	invocation, err := BuildInvocation(Codex, LaunchOptions{
		Binary: "/usr/local/bin/codex", WorkingDirectory: "/work", AccessMode: permissions.ModeConfirm,
		ModelTrack: ModelConfigured, Prompt: "continue the existing task",
	})
	if err != nil {
		t.Fatal(err)
	}
	if invocation.Binary != "/usr/local/bin/codex" {
		t.Fatalf("Codex binary = %q", invocation.Binary)
	}
	if slices.Contains(invocation.Args, "exec") || slices.Contains(invocation.Args, "app-server") || slices.Contains(invocation.Args, "--remote") {
		t.Fatalf("standard Codex invocation is not native interactive CLI: %q", invocation.Args)
	}
	if !slices.Contains(invocation.Args, "--no-alt-screen") || invocation.Args[len(invocation.Args)-1] != "continue the existing task" {
		t.Fatalf("interactive Codex TUI argv = %q", invocation.Args)
	}
}

func TestBuildInvocationIsArgvSafeAndRejectsIncompleteInput(t *testing.T) {
	t.Parallel()
	invocation, err := BuildInvocation(Codex, LaunchOptions{
		WorkingDirectory: "/tmp/path with spaces", AccessMode: permissions.ModeConfirm,
		ModelTrack: ModelExplicit, Model: "model;touch /tmp/no", Prompt: "prompt; still one argument",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(invocation.Args, "model;touch /tmp/no") || invocation.Args[len(invocation.Args)-1] != "prompt; still one argument" {
		t.Fatalf("arguments were altered: %q", invocation.Args)
	}
	if _, err := BuildInvocation(Codex, LaunchOptions{WorkingDirectory: "relative", AccessMode: permissions.ModeFull, Prompt: "x"}); err == nil {
		t.Fatal("relative cwd unexpectedly accepted")
	}
	if _, err := BuildInvocation(Codex, LaunchOptions{WorkingDirectory: "/", AccessMode: permissions.ModeFull}); err == nil {
		t.Fatal("empty prompt unexpectedly accepted")
	}
}

func TestParseRejectsUnknownProvider(t *testing.T) {
	t.Parallel()
	if _, err := Parse("other"); err == nil {
		t.Fatal("Parse(other) unexpectedly succeeded")
	}
}

func TestSpecsHaveStableRuntimeIdentities(t *testing.T) {
	t.Parallel()
	want := map[Name]string{Codex: "codex", Claude: "claude", Antigravity: "agy"}
	for name, binary := range want {
		spec, ok := Lookup(name)
		if !ok {
			t.Fatalf("missing spec for %q", name)
		}
		if spec.DefaultBinary != binary {
			t.Fatalf("binary for %q = %q, want %q", name, spec.DefaultBinary, binary)
		}
	}
}

func TestConfiguredTrackAlwaysPinsLegacyBaselineModel(t *testing.T) {
	t.Parallel()
	for _, provider := range []Name{Codex, Claude, Antigravity} {
		spec := MustLookup(provider)
		invocation, err := BuildInvocation(provider, LaunchOptions{
			WorkingDirectory: "/", AccessMode: permissions.ModeFull, ModelTrack: ModelConfigured, Prompt: "prompt",
		})
		if err != nil {
			t.Fatalf("%s: %v", provider, err)
		}
		if !slices.Contains(invocation.Args, spec.DefaultModel) {
			t.Fatalf("%s configured args %q omit baseline %q", provider, invocation.Args, spec.DefaultModel)
		}
	}
}
