package cli

import (
	"github.com/vaclavambroz/run-ai/internal/permissions"
	"github.com/vaclavambroz/run-ai/internal/providers"
	"github.com/vaclavambroz/run-ai/internal/workflows"
)

type ModelTrack = providers.ModelTrack

const (
	ModelConfigured = providers.ModelConfigured
	ModelDefault    = providers.ModelDefault
	ModelLatest     = providers.ModelLatest
	ModelExplicit   = providers.ModelExplicit
)

type SessionOptions struct {
	NewSession bool   `json:"new_session"`
	ResumeID   string `json:"resume_id,omitempty"`
	Persistent bool   `json:"persistent,omitempty"`
}

type ModelOptions struct {
	Track ModelTrack `json:"track"`
	Model string     `json:"model,omitempty"`
}

type Request struct {
	Provider           providers.Name     `json:"provider"`
	ProviderAlias      string             `json:"provider_alias"`
	ProviderExplicit   bool               `json:"provider_explicit"`
	AccessMode         permissions.Mode   `json:"access_mode"`
	AccessPolicy       permissions.Policy `json:"access_policy"`
	Workflow           workflows.Plan     `json:"workflow"`
	Session            SessionOptions     `json:"session"`
	Model              ModelOptions       `json:"model"`
	Help               bool               `json:"-"`
	Explain            bool               `json:"-"`
	ListSessions       bool               `json:"-"`
	TerminateSessionID string             `json:"-"`
}
