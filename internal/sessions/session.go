package sessions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/vaclavambroz/run-ai/internal/providers"
)

var ErrNotFound = errors.New("session mapping not found")

type Mapping struct {
	Provider providers.Name `json:"provider"`
	Key      string         `json:"key"`
	ID       string         `json:"id"`
}

type Store interface {
	Lookup(context.Context, providers.Name, string) (Mapping, error)
	Put(context.Context, Mapping) error
}

type StartMode string

const (
	StartFresh  StartMode = "new"
	StartResume StartMode = "resume"
)

type Selection struct {
	Provider  providers.Name `json:"provider"`
	Key       string         `json:"key"`
	Mode      StartMode      `json:"mode"`
	SessionID string         `json:"session_id,omitempty"`
	Mapped    bool           `json:"mapped"`
}

type SelectOptions struct {
	NewSession bool
	ResumeID   string
}

// Select is read-only. It never mutates a map or a tmux session. The runtime
// records a newly discovered provider ID only after the provider starts.
func Select(ctx context.Context, store Store, provider providers.Name, key string, opts SelectOptions) (Selection, error) {
	if err := ValidateKey(key); err != nil {
		return Selection{}, err
	}
	if opts.NewSession && opts.ResumeID != "" {
		return Selection{}, errors.New("--new-session conflicts with --resume-id")
	}
	if opts.ResumeID != "" {
		if err := ValidateID(opts.ResumeID); err != nil {
			return Selection{}, fmt.Errorf("invalid --resume-id: %w", err)
		}
		return Selection{Provider: provider, Key: key, Mode: StartResume, SessionID: opts.ResumeID}, nil
	}
	if opts.NewSession {
		return Selection{Provider: provider, Key: key, Mode: StartFresh}, nil
	}

	mapping, err := store.Lookup(ctx, provider, key)
	if err == nil {
		if mapping.Provider != provider || mapping.Key != key {
			return Selection{}, errors.New("session store returned a mismatched mapping")
		}
		if err := ValidateID(mapping.ID); err != nil {
			return Selection{}, fmt.Errorf("stored session ID is invalid: %w", err)
		}
		return Selection{
			Provider: provider, Key: key, Mode: StartResume,
			SessionID: mapping.ID, Mapped: true,
		}, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Selection{}, err
	}
	return Selection{Provider: provider, Key: key, Mode: StartFresh}, nil
}

func ValidateKey(key string) error {
	if key == "" {
		return errors.New("session key is empty")
	}
	if len(key) > 240 {
		return errors.New("session key exceeds 240 bytes")
	}
	for _, r := range key {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_.:-", r) {
			continue
		}
		return fmt.Errorf("session key contains unsupported character %q", r)
	}
	return nil
}

func ValidateID(id string) error {
	if id == "" {
		return errors.New("session ID is empty")
	}
	if len(id) > 256 {
		return errors.New("session ID exceeds 256 bytes")
	}
	for _, r := range id {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_.:-", r) {
			continue
		}
		return fmt.Errorf("session ID contains unsupported character %q", r)
	}
	return nil
}
