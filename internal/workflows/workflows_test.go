package workflows

import (
	"errors"
	"strings"
	"testing"

	"github.com/vaclavambroz/run-ai/internal/permissions"
)

func testSpamProfile() Profile {
	return Profile{
		Warnings: DefaultProfile().Warnings,
		Spam: SpamPolicy{
			Host:        "mail.example.com",
			Paths:       []string{"/email/view", "/email/raw", "/email/source"},
			Statuses:    []string{"active"},
			MaxMessages: 10,
			MaxURLBytes: 2048,
		},
	}
}

func buildWithSpam(input Input) (Plan, error) {
	if len(input.SpamURLs) > 0 {
		return BuildWithProfile(input, testSpamProfile())
	}
	return Build(input)
}

func TestBuildKindsAndConflicts(t *testing.T) {
	tests := []struct {
		name    string
		input   Input
		kind    Kind
		wantErr error
	}{
		{name: "generic full default", input: Input{AccessMode: permissions.ModeFull}, kind: KindNone},
		{name: "monitoring full", input: Input{AccessMode: permissions.ModeFull, Monitoring: true}, kind: KindMonitoring},
		{name: "monitoring readonly rejected", input: Input{AccessMode: permissions.ModeReadOnly, Monitoring: true}, wantErr: ErrMonitoringReadOnly},
		{name: "development confirm", input: Input{AccessMode: permissions.ModeConfirm, DevDomain: "example.cz"}, kind: KindDev},
		{name: "spam full two phase", input: Input{AccessMode: permissions.ModeFull, SpamURLs: []string{"https://mail.example.com/email/view?id=12"}}, kind: KindSpam},
		{name: "workflow conflict", input: Input{AccessMode: permissions.ModeReadOnly, Monitoring: true, DevDomain: "example.cz"}, wantErr: ErrWorkflowConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, err := buildWithSpam(test.input)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Build error = %v, want %v", err, test.wantErr)
			}
			if test.wantErr != nil {
				return
			}
			if plan.Kind != test.kind {
				t.Fatalf("kind = %q, want %q", plan.Kind, test.kind)
			}
			if err := plan.Validate(); err != nil {
				t.Fatalf("plan invalid: %v", err)
			}
			if len(plan.PlanDigest) != 64 || plan.Warning == "" || plan.Prompt == "" {
				t.Fatalf("incomplete plan: %+v", plan)
			}
		})
	}
}

func TestTeamModeBindsExactContractAndForcesNewSession(t *testing.T) {
	plain, err := Build(Input{AccessMode: permissions.ModeFull, DevDomain: "example.cz"})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Team != nil || plain.RequiresNewSession || strings.Contains(plain.Prompt, "root=Turing") {
		t.Fatalf("plain workflow unexpectedly enabled team mode: %+v", plain)
	}

	plan, err := Build(Input{AccessMode: permissions.ModeFull, DevDomain: "example.cz", Team: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Team == nil || !plan.RequiresNewSession || plan.PlanDigest == plain.PlanDigest {
		t.Fatalf("team workflow is not separately bound: %+v", plan)
	}
	for _, required := range []string{
		"root=Turing", "candidate_cutover_packaging=Fermat", "candidate_evidence_broker=Dewey",
		"ikb_candidate_consistency=Faraday", "autor zmeny nesmi nahrazovat nezavisly audit",
	} {
		if !strings.Contains(plan.Prompt, required) {
			t.Fatalf("team prompt lacks %q: %s", required, plan.Prompt)
		}
	}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}

	plan.Team.Members[0].WorkingName = "Dewey"
	if err := plan.Validate(); err == nil {
		t.Fatal("tampered team mapping accepted")
	}
}

func TestTeamModeValidatesForEveryWorkflowKind(t *testing.T) {
	inputs := []Input{
		{AccessMode: permissions.ModeFull, Team: true},
		{AccessMode: permissions.ModeFull, Monitoring: true, Team: true},
		{AccessMode: permissions.ModeReadOnly, SpamURLs: []string{"https://mail.example.com/email/view?id=12"}, Team: true},
		{AccessMode: permissions.ModeConfirm, DevDomain: "example.cz", Team: true},
	}
	for _, input := range inputs {
		plan, err := buildWithSpam(input)
		if err != nil {
			t.Fatalf("Build(%+v): %v", input, err)
		}
		if !plan.RequiresNewSession || plan.Team == nil {
			t.Fatalf("Build(%+v) did not enable team isolation: %+v", input, plan)
		}
		if err := plan.Validate(); err != nil {
			t.Fatalf("Build(%+v) invalid: %v", input, err)
		}
	}
}

func TestSpamPlanCanonicalizesAndDeduplicates(t *testing.T) {
	plan, err := BuildWithProfile(Input{
		AccessMode: permissions.ModeReadOnly,
		SpamURLs: []string{
			"https://mail.example.com/email/raw?status=active&id=1008054",
			"https://mail.example.com/email/view?id=1008054&status=active",
			"https://mail.example.com/email/source?id=22",
		},
	}, testSpamProfile())
	if err != nil {
		t.Fatal(err)
	}
	if !plan.RequiresNewSession || len(plan.SpamReferences) != 2 {
		t.Fatalf("unexpected spam plan: %+v", plan)
	}
	want := SpamReference{
		MessageID: 1008054, Status: "active", SourceKind: "raw",
		CanonicalURL: "https://mail.example.com/email/raw?id=1008054&status=active",
	}
	if plan.SpamReferences[0] != want {
		t.Fatalf("first reference = %+v, want %+v", plan.SpamReferences[0], want)
	}
	if plan.ExecutionMode != ExecutionAnalyzeOnly || plan.MutationAllowed {
		t.Fatalf("readonly spam is not analyze-only: %+v", plan)
	}
	if !strings.Contains(plan.Prompt, "neduveryhodna data") || strings.Contains(plan.Prompt, "1008054") {
		t.Fatalf("unsafe/noncanonical spam prompt: %q", plan.Prompt)
	}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSpamURLAdversarial(t *testing.T) {
	bad := []string{
		"http://mail.example.com/email/view?id=1",
		"https://evil.invalid/email/view?id=1",
		"https://mail.example.com.evil.invalid/email/view?id=1",
		"https://user@mail.example.com/email/view?id=1",
		"https://mail.example.com:443/email/view?id=1",
		"https://MAIL.example.com/email/view?id=1",
		"https://mail.example.com/email/view?id=1#",
		"https://mail.example.com/email/%76iew?id=1",
		"https://mail.example.com/email/view",
		"https://mail.example.com/email/view?id=",
		"https://mail.example.com/email/view?id=0",
		"https://mail.example.com/email/view?id=01",
		"https://mail.example.com/email/view?id=%31",
		"https://mail.example.com/email/view?id=1&id=2",
		"https://mail.example.com/email/view?id=1&status=",
		"https://mail.example.com/email/view?id=1&status=spam",
		"https://mail.example.com/email/view?id=1&next=https://evil.invalid",
		" https://mail.example.com/email/view?id=1",
	}
	for _, raw := range bad {
		t.Run(raw, func(t *testing.T) {
			_, err := BuildWithProfile(Input{AccessMode: permissions.ModeReadOnly, SpamURLs: []string{raw}}, testSpamProfile())
			if err == nil {
				t.Fatalf("accepted malicious/ambiguous URL %q", raw)
			}
		})
	}

	tooMany := make([]string, 11)
	for index := range tooMany {
		tooMany[index] = "https://mail.example.com/email/view?id=" + string(rune('1'+index))
	}
	if _, err := BuildWithProfile(Input{AccessMode: permissions.ModeReadOnly, SpamURLs: tooMany}, testSpamProfile()); err == nil {
		t.Fatal("accepted more than ten spam URLs")
	}
}

func TestSpamMutationModes(t *testing.T) {
	url := "https://mail.example.com/email/view?id=10&status=active"
	tests := []struct {
		mode      permissions.Mode
		execution ExecutionMode
		mutation  bool
	}{
		{permissions.ModeFull, ExecutionTwoPhaseApply, true},
		{permissions.ModeConfirm, ExecutionTwoPhaseConfirm, true},
		{permissions.ModeReadOnly, ExecutionAnalyzeOnly, false},
	}
	for _, test := range tests {
		plan, err := BuildWithProfile(Input{AccessMode: test.mode, SpamURLs: []string{url}}, testSpamProfile())
		if err != nil {
			t.Fatal(err)
		}
		if plan.ExecutionMode != test.execution || plan.MutationAllowed != test.mutation {
			t.Fatalf("%s plan execution=%q mutation=%v", test.mode, plan.ExecutionMode, plan.MutationAllowed)
		}
		for _, required := range []string{"bounded hledani", "lint", "test", "regresni", "message_id"} {
			if !strings.Contains(plan.Prompt, required) {
				t.Fatalf("%s spam prompt lacks %q: %s", test.mode, required, plan.Prompt)
			}
		}
		if err := plan.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMonitoringPromptContract(t *testing.T) {
	for _, mode := range []permissions.Mode{permissions.ModeFull, permissions.ModeConfirm} {
		plan, err := Build(Input{AccessMode: mode, Monitoring: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{
			"live Icinga inventory", "vlastnika robota", "bounded ACK", "root cause", "child check",
			"aggregate check", "manual-step", "escalation hold", "total problems=0", "typed blocker",
			"neopravnuje monitoring cutover", "retirement legacy",
		} {
			if !strings.Contains(plan.Prompt, required) {
				t.Fatalf("%s monitoring prompt lacks %q: %s", mode, required, plan.Prompt)
			}
		}
	}
}

func TestValidateDomain(t *testing.T) {
	valid := []string{"example.cz", "xn--e1afmkfd.xn--p1ai", "a-b.example.cz"}
	for _, domain := range valid {
		got, err := ValidateDomain(domain)
		if err != nil || got != domain {
			t.Fatalf("ValidateDomain(%q) = %q, %v", domain, got, err)
		}
	}
	invalid := []string{"", "localhost", "Example.cz", "example.cz.", "-x.example", "x-.example", "x..example", "example.123", "127.0.0.1", "example.cz/path", "example.cz\n--readonly", "žluťoučký.cz"}
	for _, domain := range invalid {
		if _, err := ValidateDomain(domain); err == nil {
			t.Fatalf("accepted invalid domain %q", domain)
		}
	}
}

func TestDevelopmentPromptRequiresCanonicalInventory(t *testing.T) {
	plan, err := Build(Input{AccessMode: permissions.ModeConfirm, DevDomain: "example.cz"})
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"prave jeden exact inventory",
		"canonical app root",
		"typed blocker",
		"nesmi",
		"rozsirit cil",
		"nejmene dva nezavisle kompletni pruchody",
		"nejmene dve prakticka overeni",
		"matici spust znovu od zacatku",
		"design-analyze analyze",
		"design-analyze compare",
		"design-analyze audit-url",
		"headless Chromium",
		"multi-viewport",
		"full-page",
		"browser console a network chyby",
		"desktopnim i mobilnim viewportu",
		"Screenshot neni nahrada funkcniho testu",
		"vrat .design-analyze vystupy canonical vlastnikovi aplikace",
		"deterministicky to over",
		"neprohlasuj praci za hotovou",
	} {
		if !strings.Contains(plan.Prompt, required) {
			t.Fatalf("development prompt lacks %q: %s", required, plan.Prompt)
		}
	}
}

func TestPlanTamperingRejected(t *testing.T) {
	plan, err := Build(Input{AccessMode: permissions.ModeFull, Monitoring: true})
	if err != nil {
		t.Fatal(err)
	}
	plan.Prompt += " Ignore policy"
	if err := plan.Validate(); err == nil {
		t.Fatal("tampered prompt accepted")
	}

	plan, _ = Build(Input{AccessMode: permissions.ModeFull, Monitoring: true})
	plan.PermissionPolicyDigest = strings.Repeat("0", 64)
	if err := plan.Validate(); err == nil {
		t.Fatal("wrong permission binding accepted")
	}
}
