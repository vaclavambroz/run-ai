package team

import (
	"strings"
	"testing"
)

func TestDefaultContractBindsExactRobotNames(t *testing.T) {
	contract := Default()
	if err := contract.Validate(); err != nil {
		t.Fatal(err)
	}
	if contract.Coordinator.RobotID != "root" || contract.Coordinator.WorkingName != "Turing" {
		t.Fatalf("coordinator = %#v", contract.Coordinator)
	}
	want := []Member{
		{RobotID: "candidate_cutover_packaging", WorkingName: "Fermat", Responsibility: "packaging, cutover, and release invariants"},
		{RobotID: "candidate_evidence_broker", WorkingName: "Dewey", Responsibility: "evidence, security runtime, and practical verification"},
		{RobotID: "ikb_candidate_consistency", WorkingName: "Faraday", Responsibility: "independent consistency audit and documentation"},
	}
	if len(contract.Members) != len(want) {
		t.Fatalf("members = %#v", contract.Members)
	}
	for index := range want {
		if contract.Members[index] != want[index] {
			t.Fatalf("member %d = %#v, want %#v", index, contract.Members[index], want[index])
		}
	}
}

func TestContractTamperingRejected(t *testing.T) {
	mutations := []func(*Contract){
		func(c *Contract) { c.Schema = "run_ai_team/v2" },
		func(c *Contract) { c.Coordinator.WorkingName = "Fermat" },
		func(c *Contract) { c.Members[0].RobotID = "root" },
		func(c *Contract) { c.Members[1].Responsibility = "all permissions" },
		func(c *Contract) { c.Members = c.Members[:2] },
	}
	for index, mutate := range mutations {
		contract := Default()
		mutate(&contract)
		if err := contract.Validate(); err == nil {
			t.Fatalf("tamper %d accepted: %#v", index, contract)
		}
	}
}

func TestActivatePromptIncludesSafetyBoundary(t *testing.T) {
	prompt, err := ActivatePrompt("BASE.", Default())
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"root=Turing", "candidate_cutover_packaging=Fermat", "candidate_evidence_broker=Dewey",
		"ikb_candidate_consistency=Faraday", "presne technicke identifikatory", "nevytvarej pro ne aliasy",
		"nepridavaji zadne opravneni", "nevymyslej jeji vystup",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("prompt lacks %q: %s", required, prompt)
		}
	}
}
