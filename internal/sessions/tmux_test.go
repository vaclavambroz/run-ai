package sessions

import (
	"testing"

	"github.com/vaclavambroz/run-ai/internal/providers"
)

func TestLegacyTmuxNamesRemainCompatible(t *testing.T) {
	t.Parallel()
	tests := []struct {
		provider providers.Name
		key      string
		fresh    bool
		nonce    string
		want     string
	}{
		{providers.Codex, "hosting_api_main", false, "", "codex_codex_hosting_api_main"},
		{providers.Claude, "hosting_api_main", false, "", "codex_claude_hosting_api_main"},
		{providers.Antigravity, "hosting_api_main", false, "", "codex_antigravity_hosting_api_main"},
		{providers.Codex, "hosting_api_main", true, "20261013162932-3335201", "codex_codex_hosting_api_main_new_20261013162932-3335201"},
	}
	for _, test := range tests {
		got, err := LegacyTmuxName("", test.provider, test.key, test.fresh, test.nonce)
		if err != nil {
			t.Fatal(err)
		}
		if got != test.want {
			t.Fatalf("LegacyTmuxName() = %q, want %q", got, test.want)
		}
	}
}

func TestLegacyTmuxNameRequiresNonceForFreshSession(t *testing.T) {
	t.Parallel()
	if _, err := LegacyTmuxName("", providers.Codex, "main_plan", true, ""); err == nil {
		t.Fatal("fresh session without nonce unexpectedly accepted")
	}
}
