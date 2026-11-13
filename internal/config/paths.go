package config

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
)

type Paths struct {
	SystemConfig string `json:"system_config"`
	SystemData   string `json:"system_data"`
	UserConfig   string `json:"user_config"`
	UserState    string `json:"user_state"`
	UserCache    string `json:"user_cache"`
	AuditLog     string `json:"audit_log"`
}

// ResolvePaths contains no filesystem side effects. Values supplied through
// XDG variables must be absolute; accepting relative privileged paths would
// make launcher behavior depend on the caller's working directory.
func ResolvePaths(goos, home string, env map[string]string) (Paths, error) {
	if goos == "" {
		goos = runtime.GOOS
	}
	home = filepath.Clean(strings.TrimSpace(home))
	if !filepath.IsAbs(home) {
		return Paths{}, errors.New("home directory must be absolute")
	}
	configHome, err := absoluteOrDefault(env["XDG_CONFIG_HOME"], filepath.Join(home, ".config"))
	if err != nil {
		return Paths{}, err
	}
	stateHome, err := absoluteOrDefault(env["XDG_STATE_HOME"], filepath.Join(home, ".local", "state"))
	if err != nil {
		return Paths{}, err
	}
	cacheHome, err := absoluteOrDefault(env["XDG_CACHE_HOME"], filepath.Join(home, ".cache"))
	if err != nil {
		return Paths{}, err
	}
	paths := Paths{
		SystemConfig: "/etc/run-ai/config.json",
		SystemData:   "/usr/share/run-ai",
		UserConfig:   filepath.Join(configHome, "run-ai", "config.json"),
		UserState:    filepath.Join(stateHome, "run-ai"),
		UserCache:    filepath.Join(cacheHome, "run-ai"),
	}
	if goos == "darwin" {
		paths.SystemConfig = "/Library/Application Support/run-ai/config.json"
		paths.SystemData = "/Library/Application Support/run-ai"
		if env["XDG_CONFIG_HOME"] == "" {
			paths.UserConfig = filepath.Join(home, "Library", "Application Support", "run-ai", "config.json")
		}
		if env["XDG_STATE_HOME"] == "" {
			paths.UserState = filepath.Join(home, "Library", "Application Support", "run-ai", "state")
		}
		if env["XDG_CACHE_HOME"] == "" {
			paths.UserCache = filepath.Join(home, "Library", "Caches", "run-ai")
		}
	}
	paths.AuditLog = filepath.Join(paths.UserState, "audit.jsonl")
	return paths, nil
}

func absoluteOrDefault(value, fallback string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return filepath.Clean(fallback), nil
	}
	value = filepath.Clean(value)
	if !filepath.IsAbs(value) {
		return "", errors.New("XDG path must be absolute")
	}
	return value, nil
}
