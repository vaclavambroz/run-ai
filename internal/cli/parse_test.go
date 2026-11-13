package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/vaclavambroz/run-ai/internal/permissions"
	"github.com/vaclavambroz/run-ai/internal/providers"
	"github.com/vaclavambroz/run-ai/internal/workflows"
)

func TestParseCompatibilityDefaults(t *testing.T) {
	t.Parallel()
	request, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if request.Provider != providers.Codex || request.ProviderAlias != "configured_default" || request.ProviderExplicit {
		t.Fatalf("provider default = %#v", request)
	}
	if request.AccessMode != permissions.ModeFull {
		t.Fatalf("access = %q", request.AccessMode)
	}
	if request.Workflow.Kind != workflows.KindNone || request.Model.Track != ModelConfigured {
		t.Fatalf("defaults = %#v", request)
	}
	if err := request.AccessPolicy.Validate(); err != nil {
		t.Fatalf("invalid access policy: %v", err)
	}
}

func TestParseExactSessionsListCommand(t *testing.T) {
	t.Parallel()
	request, err := Parse([]string{"sessions", "list"})
	if err != nil || !request.ListSessions {
		t.Fatalf("sessions list = %#v, %v", request, err)
	}
	for _, args := range [][]string{{"sessions"}, {"sessions", "list", "extra"}, {"sessions", "unknown"}} {
		assertParseError(t, args)
	}
}

func TestParseProviderFormsAndGeminiNormalization(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args []string
		want providers.Name
	}{
		{[]string{"--codex"}, providers.Codex},
		{[]string{"--claude"}, providers.Claude},
		{[]string{"--gemini"}, providers.Antigravity},
		{[]string{"--provider", "antigravity"}, providers.Antigravity},
		{[]string{"--provider=gemini"}, providers.Antigravity},
	}
	for _, test := range tests {
		request, err := Parse(test.args)
		if err != nil {
			t.Fatalf("Parse(%v): %v", test.args, err)
		}
		if request.Provider != test.want || !request.ProviderExplicit {
			t.Fatalf("Parse(%v) provider = %q explicit=%t", test.args, request.Provider, request.ProviderExplicit)
		}
	}
	gemini, err := Parse([]string{"--gemini", "--explain"})
	if err != nil || gemini.ProviderAlias != "gemini" || gemini.Provider != providers.Antigravity {
		t.Fatalf("gemini identity = %#v, %v", gemini, err)
	}
}

func TestParseAccessModes(t *testing.T) {
	t.Parallel()
	confirm, err := Parse([]string{"--confirm"})
	if err != nil || confirm.AccessMode != permissions.ModeConfirm {
		t.Fatalf("confirm = %#v, %v", confirm, err)
	}
	readonly, err := Parse([]string{"--readonly"})
	if err != nil || readonly.AccessMode != permissions.ModeReadOnly || !readonly.AccessPolicy.HardReadOnly {
		t.Fatalf("readonly = %#v, %v", readonly, err)
	}
	assertParseError(t, []string{"--confirm", "--readonly"})
}

func TestParseWorkflowFormsAndConflicts(t *testing.T) {
	t.Parallel()
	monitoring, err := Parse([]string{"--monitoring"})
	if err != nil || monitoring.Workflow.Kind != workflows.KindMonitoring {
		t.Fatalf("monitoring = %#v, %v", monitoring, err)
	}
	spam, err := Parse([]string{"--spam-host", "spam.example.com", "--spam", "https://spam.example.com/email/view?id=1", "https://spam.example.com/email/source?id=2", "--spam=https://spam.example.com/email/view?id=3"})
	if err != nil || spam.Workflow.Kind != workflows.KindSpam || len(spam.Workflow.SpamReferences) != 3 || !spam.Session.NewSession {
		t.Fatalf("spam = %#v, %v", spam, err)
	}
	dev, err := Parse([]string{"run", "--dev=example.cz"})
	if err != nil || dev.Workflow.Kind != workflows.KindDev || dev.Workflow.Target != "example.cz" {
		t.Fatalf("dev = %#v, %v", dev, err)
	}
	legacy, err := Parse([]string{"example.cz"})
	if err != nil || legacy.Workflow.Kind != dev.Workflow.Kind || legacy.Workflow.Target != dev.Workflow.Target {
		t.Fatalf("legacy domain = %#v, %v", legacy, err)
	}
	for _, args := range [][]string{
		{"--monitoring", "--dev", "example.cz"},
		{"--host", "hosting", "example.cz"},
		{"--spam", "https://spam.example.com/email/view?id=1", "--monitoring"},
		{"one.cz", "two.cz"},
	} {
		assertParseError(t, args)
	}
}

func TestParseWorkflowPolicyRunsBeforeExplain(t *testing.T) {
	t.Parallel()
	assertParseError(t, []string{"--monitoring", "--readonly", "--explain"})

	spam, err := Parse([]string{"--readonly", "--spam-host", "spam.example.com", "--spam", "https://spam.example.com/email/view?id=1", "--explain"})
	if err != nil {
		t.Fatal(err)
	}
	if spam.Workflow.ExecutionMode != workflows.ExecutionAnalyzeOnly || spam.Workflow.MutationAllowed {
		t.Fatalf("readonly spam = %#v", spam.Workflow)
	}
}

func TestParseTeamActivatesFixedContractInNewSession(t *testing.T) {
	t.Parallel()
	request, err := Parse([]string{"--team", "--dev", "example.cz", "--explain"})
	if err != nil {
		t.Fatal(err)
	}
	if request.Workflow.Team == nil || !request.Workflow.RequiresNewSession || !request.Session.NewSession {
		t.Fatalf("team request = %#v", request)
	}
	if request.Workflow.Team.Coordinator.RobotID != "root" || request.Workflow.Team.Coordinator.WorkingName != "Turing" {
		t.Fatalf("team contract = %#v", request.Workflow.Team)
	}
	if err := request.Workflow.Validate(); err != nil {
		t.Fatal(err)
	}
	resumed, err := Parse([]string{"--team", "--dev", "example.cz", "--resume-id", "abc.db"})
	if err != nil || resumed.Workflow.Team == nil || !resumed.Workflow.RequiresNewSession || resumed.Session.NewSession || resumed.Workflow.PlanDigest != request.Workflow.PlanDigest {
		t.Fatalf("resumed team request = %#v, %v", resumed, err)
	}
	if err := resumed.Workflow.Validate(); err != nil {
		t.Fatal(err)
	}
	if resumed.Workflow.PlanDigest != request.Workflow.PlanDigest {
		t.Fatalf("team resume changed digest: fresh=%s resumed=%s", request.Workflow.PlanDigest, resumed.Workflow.PlanDigest)
	}
}

func TestParseSessionAndModelConflicts(t *testing.T) {
	t.Parallel()
	request, err := Parse([]string{"--resume-id", "abc.db", "--model=model/1"})
	if err != nil {
		t.Fatal(err)
	}
	if request.Session.ResumeID != "abc.db" || request.Model.Track != ModelExplicit || request.Model.Model != "model/1" {
		t.Fatalf("request = %#v", request)
	}
	for _, args := range [][]string{
		{"--new-session", "--resume-id", "abc"},
		{"--default", "--latest"},
		{"--model", "x", "--latest"},
		{"--codex", "--provider", "codex"},
		{"--claude", "--gemini"},
	} {
		assertParseError(t, args)
	}
}

func TestResumeIDDoesNotSkipFollowingFlag(t *testing.T) {
	t.Parallel()
	request, err := Parse([]string{"--resume-id", "abc.db", "--readonly"})
	if err != nil {
		t.Fatal(err)
	}
	if request.Session.ResumeID != "abc.db" || request.AccessMode != permissions.ModeReadOnly {
		t.Fatalf("request = %#v", request)
	}
}

func TestParsePersistentCodexOnlyAndRejectsTeam(t *testing.T) {
	request, err := Parse([]string{"--codex", "--persistent", "--new-session", "--model", "gpt-5.6-terra"})
	if err != nil || !request.Session.Persistent || !request.Session.NewSession || request.Model.Model != "gpt-5.6-terra" {
		t.Fatalf("request=%#v err=%v", request, err)
	}
	for _, args := range [][]string{{"--claude", "--persistent"}, {"--gemini", "--persistent"}, {"--persistent", "--team"}} {
		if _, err := Parse(args); err == nil {
			t.Fatalf("args %v unexpectedly parsed", args)
		}
	}
}

func TestParseRejectsMissingUnknownAndControlValues(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"--provider"},
		{"--provider", "unknown"},
		{"--model"},
		{"--resume-id", "bad/id"},
		{"--dev", "bad\nvalue"},
		{"--unknown"},
	} {
		assertParseError(t, args)
	}
}

func TestWriteExplanationIsTypedJSONWithoutControlFlags(t *testing.T) {
	t.Parallel()
	request, err := Parse([]string{"--gemini", "--readonly", "--dev", "example.cz", "--new-session", "--latest", "--explain"})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := WriteExplanation(&output, request); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["provider"] != "antigravity" || decoded["provider_alias"] != "gemini" || decoded["access_mode"] != "readonly" {
		t.Fatalf("explanation = %s", output.Bytes())
	}
	if _, exists := decoded["explain"]; exists {
		t.Fatalf("control flag leaked into explanation: %s", output.Bytes())
	}
}

func assertParseError(t *testing.T, args []string) {
	t.Helper()
	if _, err := Parse(args); err == nil || !IsParseError(err) {
		t.Fatalf("Parse(%v) error = %v, want ParseError", args, err)
	}
}
