package sessions

import (
	"errors"
	"strings"
	"unicode"

	"github.com/vaclavambroz/run-ai/internal/providers"
)

const LegacyTmuxPrefix = "codex"

// LegacyTmuxName preserves the naming contract established by the original provider launcher.
// It only computes an identity; it never invokes tmux or inspects live state.
func LegacyTmuxName(prefix string, provider providers.Name, key string, newSession bool, nonce string) (string, error) {
	if prefix == "" {
		prefix = LegacyTmuxPrefix
	}
	if _, ok := providers.Lookup(provider); !ok {
		return "", errors.New("unknown provider")
	}
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	if newSession && nonce == "" {
		return "", errors.New("new session requires a nonce")
	}
	raw := prefix + "_" + string(provider) + "_" + key
	if newSession {
		raw += "_new_" + nonce
	}
	return sanitizeTmuxName(raw), nil
}

func sanitizeTmuxName(value string) string {
	var result strings.Builder
	result.Grow(len(value))
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_.:-", r) {
			result.WriteRune(r)
		} else {
			result.WriteByte('_')
		}
	}
	return result.String()
}
