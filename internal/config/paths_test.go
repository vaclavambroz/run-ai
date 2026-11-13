package config

import "testing"

func TestLinuxPaths(t *testing.T) {
	paths, err := ResolvePaths("linux", "/root", map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if paths.UserConfig != "/root/.config/run-ai/config.json" || paths.AuditLog != "/root/.local/state/run-ai/audit.jsonl" {
		t.Fatalf("unexpected paths: %#v", paths)
	}
}

func TestMacPaths(t *testing.T) {
	paths, err := ResolvePaths("darwin", "/Users/test", map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if paths.UserConfig != "/Users/test/Library/Application Support/run-ai/config.json" {
		t.Fatalf("unexpected paths: %#v", paths)
	}
}

func TestRejectsRelativeXDG(t *testing.T) {
	if _, err := ResolvePaths("linux", "/root", map[string]string{"XDG_STATE_HOME": "relative"}); err == nil {
		t.Fatal("relative XDG path must be rejected")
	}
}
