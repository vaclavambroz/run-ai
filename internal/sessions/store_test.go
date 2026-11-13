package sessions

import (
	"context"
	"errors"
	"testing"

	"github.com/vaclavambroz/run-ai/internal/providers"
)

func TestSelectPreservesMappedSession(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	want := Mapping{Provider: providers.Codex, Key: "hosting_api_main", ID: "019dd437-1a0e-7e40-b09c-651bffee48e4"}
	if err := store.Put(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	got, err := Select(context.Background(), store, providers.Codex, want.Key, SelectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != StartResume || !got.Mapped || got.SessionID != want.ID {
		t.Fatalf("unexpected selection: %#v", got)
	}
}

func TestSelectNewDoesNotOverwriteMapping(t *testing.T) {
	t.Parallel()
	store := NewMemoryStore()
	want := Mapping{Provider: providers.Claude, Key: "hosting_api_main", ID: "42a4275e-4b5e-4321-84c4-901b540d123a"}
	if err := store.Put(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	selected, err := Select(context.Background(), store, providers.Claude, want.Key, SelectOptions{NewSession: true})
	if err != nil {
		t.Fatal(err)
	}
	if selected.Mode != StartFresh || selected.SessionID != "" {
		t.Fatalf("unexpected fresh selection: %#v", selected)
	}
	stored, err := store.Lookup(context.Background(), providers.Claude, want.Key)
	if err != nil || stored != want {
		t.Fatalf("new selection mutated mapping: %#v, %v", stored, err)
	}
}

func TestSelectRejectsConflictingSessionFlags(t *testing.T) {
	t.Parallel()
	_, err := Select(context.Background(), NewMemoryStore(), providers.Codex, "main_plan", SelectOptions{NewSession: true, ResumeID: "abc"})
	if err == nil {
		t.Fatal("conflicting session flags unexpectedly accepted")
	}
}

func TestSelectPropagatesStoreErrors(t *testing.T) {
	t.Parallel()
	want := errors.New("broken store")
	_, err := Select(context.Background(), errorStore{err: want}, providers.Codex, "main_plan", SelectOptions{})
	if !errors.Is(err, want) {
		t.Fatalf("Select error = %v, want %v", err, want)
	}
}

type errorStore struct{ err error }

func (s errorStore) Lookup(context.Context, providers.Name, string) (Mapping, error) {
	return Mapping{}, s.err
}
func (s errorStore) Put(context.Context, Mapping) error { return s.err }
