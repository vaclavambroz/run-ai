// Package team defines the fixed run_ai collaboration contract. The working
// names are role labels only; they do not grant permissions or authority.
package team

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
)

const Schema = "run_ai_team/v1"

type Member struct {
	RobotID        string `json:"robot_id"`
	WorkingName    string `json:"working_name"`
	Responsibility string `json:"responsibility"`
}

type Contract struct {
	Schema      string   `json:"schema"`
	Coordinator Member   `json:"coordinator"`
	Members     []Member `json:"members"`
}

// Default returns a fresh copy of the only supported team mapping.
func Default() Contract {
	return Contract{
		Schema: Schema,
		Coordinator: Member{
			RobotID:        "root",
			WorkingName:    "Turing",
			Responsibility: "root coordinator, integration, and final control",
		},
		Members: []Member{
			{
				RobotID:        "candidate_cutover_packaging",
				WorkingName:    "Fermat",
				Responsibility: "packaging, cutover, and release invariants",
			},
			{
				RobotID:        "candidate_evidence_broker",
				WorkingName:    "Dewey",
				Responsibility: "evidence, security runtime, and practical verification",
			},
			{
				RobotID:        "ikb_candidate_consistency",
				WorkingName:    "Faraday",
				Responsibility: "independent consistency audit and documentation",
			},
		},
	}
}

func (c Contract) Validate() error {
	if !reflect.DeepEqual(c, Default()) {
		return errors.New("team contract does not match the fixed robot/name mapping")
	}
	return nil
}

// ActivatePrompt appends the exact collaboration contract to a workflow
// prompt. It never changes the workflow's permission or mutation policy.
func ActivatePrompt(base string, c Contract) (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	if strings.TrimSpace(base) == "" {
		return "", errors.New("team mode requires a non-empty base workflow prompt")
	}
	return base + fmt.Sprintf(" TEAM MODE run_ai. Pouzij pevnou vazbu technickych robotu na pracovni jmena: %s=%s (%s); %s=%s (%s); %s=%s (%s); %s=%s (%s). Koordinator je existujici technicky robot root a pouziva pracovni jmeno Turing. Pri delegaci techto roli pouzij presne technicke identifikatory candidate_cutover_packaging, candidate_evidence_broker a ikb_candidate_consistency jako task_name; nevytvarej pro ne aliasy ani nezamen jmena. Turing koordinuje, rozdeluje jen konkretni ohranicene nezavisle ukoly, integruje vysledky a provadi finalni closeout. Fermat drzi implementacni packaging, cutover a release invarianty. Dewey dodava praktickou evidenci, security runtime a adversarialni overeni. Faraday provadi nezavisly consistency audit a dokumentaci; autor zmeny nesmi nahrazovat nezavisly audit. Pouzij spolupracujici agenty jen kdyz je runtime skutecne poskytuje. Kdyz nektera role neni dostupna, nevymyslej jeji vystup: pokracuj jako Turing v rozsahu opravneni a nedostupnou roli uved jako blocker tam, kde je jeji nezavisly vystup nutny. Tato jmena jsou pouze interni pracovni role, nejsou identitou historickych osob a nepridavaji zadne opravneni. Rozsah a prava vzdy urcuje zadani uzivatele, systemove mantinely, typovane kontrakty a skutecna opravneni.",
		c.Coordinator.RobotID, c.Coordinator.WorkingName, c.Coordinator.Responsibility,
		c.Members[0].RobotID, c.Members[0].WorkingName, c.Members[0].Responsibility,
		c.Members[1].RobotID, c.Members[1].WorkingName, c.Members[1].Responsibility,
		c.Members[2].RobotID, c.Members[2].WorkingName, c.Members[2].Responsibility,
	), nil
}
