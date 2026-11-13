// Package workflows validates workflow selectors and builds canonical,
// digest-bound plans.
package workflows

import "errors"

// Profile describes host-specific workflow configuration.
// Supply a site-specific profile via BuildWithProfile for production
// deployments; DefaultProfile returns a minimal built-in profile.
type Profile struct {
	Warnings map[string]string
	Spam     SpamPolicy
}

// SpamPolicy configures the spam filter workflow URL parser.
type SpamPolicy struct {
	Host        string
	Paths       []string
	Statuses    []string
	MaxMessages int
	MaxURLBytes int
}

// DefaultProfile returns a minimal built-in profile for standalone use.
func DefaultProfile() Profile {
	return Profile{
		Warnings: map[string]string{
			"full_access_unsafe":     "FULL MODE: AI may modify files and external state without per-action confirmation. Use only in trusted contexts.",
			"confirmation_required":  "CONFIRMATION MODE: each mutation requires a separate confirmation before execution.",
			"hard_readonly_enforced": "HARD READ-ONLY MODE: mutations and permission escalation are not permitted.",
		},
		Spam: SpamPolicy{
			Host:        "",
			Paths:       []string{},
			Statuses:    []string{"active"},
			MaxMessages: 10,
			MaxURLBytes: 2048,
		},
	}
}

// Validate checks that the profile is well-formed for plan building.
func (p Profile) Validate() error {
	if p.Warnings["full_access_unsafe"] == "" ||
		p.Warnings["confirmation_required"] == "" ||
		p.Warnings["hard_readonly_enforced"] == "" {
		return errors.New("profile must provide all three warning messages")
	}
	return nil
}
