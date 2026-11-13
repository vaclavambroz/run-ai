package legacy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/vaclavambroz/run-ai/internal/providers"
	"github.com/vaclavambroz/run-ai/internal/sessions"
)

func TestCurrentMapPathsPreserveLegacyNames(t *testing.T) {
	t.Parallel()
	paths := CurrentMapPaths("/opt/codex")
	want := map[providers.Name]string{
		providers.Codex:       "/opt/codex/access/SESSIONS.md",
		providers.Claude:      "/opt/codex/access/CLAUDE_SESSIONS.md",
		providers.Antigravity: "/opt/codex/access/ANTIGRAVITY_SESSIONS.md",
	}
	for provider, path := range want {
		if paths[provider] != path {
			t.Fatalf("path for %q = %q, want %q", provider, paths[provider], path)
		}
	}
}

func TestMapStoreReadsAndUpdatesWithoutDestroyingComments(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "SESSIONS.md")
	original := "# Session IDs\n\nmain_plan=old-id\nmain_app_server=keep-id\n"
	if err := os.WriteFile(path, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	store := NewMapStore(MapPaths{providers.Codex: path})
	ctx := context.Background()
	mapping, err := store.Lookup(ctx, providers.Codex, "main_app_server")
	if err != nil || mapping.ID != "keep-id" {
		t.Fatalf("Lookup() = %#v, %v", mapping, err)
	}
	if err := store.Put(ctx, sessions.Mapping{Provider: providers.Codex, Key: "main_plan", ID: "new-id"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "# Session IDs\n\nmain_plan=new-id\nmain_app_server=keep-id\n"
	if string(data) != want {
		t.Fatalf("updated map:\n%s\nwant:\n%s", data, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o, want 640", info.Mode().Perm())
	}
}

func TestMapStoreAddsMissingMappingIdempotently(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "CLAUDE_SESSIONS.md")
	store := NewMapStore(MapPaths{providers.Claude: path})
	want := sessions.Mapping{Provider: providers.Claude, Key: "main_plan", ID: "uuid-1"}
	for range 2 {
		if err := store.Put(context.Background(), want); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "main_plan=uuid-1\n" {
		t.Fatalf("unexpected idempotent output %q", data)
	}
}

func TestMapStoreRejectsDuplicateAndSymlinkMaps(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	duplicate := filepath.Join(directory, "duplicate.md")
	if err := os.WriteFile(duplicate, []byte("main_plan=one\nmain_plan=two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewMapStore(MapPaths{providers.Codex: duplicate})
	if _, err := store.Lookup(context.Background(), providers.Codex, "main_plan"); err == nil {
		t.Fatal("duplicate map unexpectedly accepted")
	}

	target := filepath.Join(directory, "target.md")
	link := filepath.Join(directory, "link.md")
	if err := os.WriteFile(target, []byte("main_plan=one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	store = NewMapStore(MapPaths{providers.Codex: link})
	if _, err := store.Lookup(context.Background(), providers.Codex, "main_plan"); err == nil {
		t.Fatal("symlink map unexpectedly accepted")
	}
}

func TestMapStoreMissingMappingUsesTypedNotFound(t *testing.T) {
	t.Parallel()
	store := NewMapStore(MapPaths{providers.Codex: filepath.Join(t.TempDir(), "missing.md")})
	_, err := store.Lookup(context.Background(), providers.Codex, "main_plan")
	if !errors.Is(err, sessions.ErrNotFound) {
		t.Fatalf("Lookup error = %v, want ErrNotFound", err)
	}
}
