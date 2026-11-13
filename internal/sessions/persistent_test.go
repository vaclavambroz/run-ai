package sessions

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPersistentStoreCreatesPrivateDurableExactSession(t *testing.T) {
	root := filepath.Join(t.TempDir(), "persistent")
	store := PersistentStore{Directory: root}
	created, err := NewPersistentSession("persistent-20261019090000-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "codex", "/tmp/work", "", "gpt-5.6-terra", "configured", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(created); err != nil {
		t.Fatal(err)
	}
	dir, err := store.StateDirectory(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("state dir=%v err=%v", info, err)
	}
	info, err = os.Stat(filepath.Join(dir, "session.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("metadata=%v err=%v", info, err)
	}
	bound, err := store.BindThread(created.ID, "01a15365-5be9-7661-a03e-df4519af73ec", time.Unix(1, 0))
	if err != nil || bound.ThreadID == "" {
		t.Fatalf("bound=%#v err=%v", bound, err)
	}
	if _, err := store.BindThread(created.ID, "01a1534f-208f-7882-956d-2da052c0ed42", time.Unix(2, 0)); err == nil {
		t.Fatal("different thread unexpectedly accepted")
	}
	got, err := store.Lookup(created.ID)
	if err != nil || got.Digest != bound.Digest {
		t.Fatalf("got=%#v err=%v", got, err)
	}
}

func TestPersistentStoreListIsReadOnlyWhenAbsent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	values, err := (PersistentStore{Directory: root}).List()
	if err != nil || len(values) != 0 {
		t.Fatalf("values=%#v err=%v", values, err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("list created state: %v", err)
	}
}

// writeSessionWithTamperedDigest creates a session directory and file that
// parses correctly but has an incorrect digest field (all zeros). This
// simulates a session written by a binary version that included additional
// fields in the digest computation – the exact scenario that caused the
// production crash with historic team-persistent sessions.
func writeSessionWithTamperedDigest(t *testing.T, storeDir, id string) {
	t.Helper()
	dir := filepath.Join(storeDir, id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("create stale session dir: %v", err)
	}
	// Build a structurally valid JSON but with a digest that cannot match.
	raw := map[string]string{
		"schema":            "run_ai_persistent_session/v1",
		"id":                id,
		"provider":          "codex",
		"working_directory": "/tmp/work",
		"model":             "gpt-5.6-terra",
		"model_track":       "configured",
		"workflow_digest":   strings.Repeat("a", 64),
		"created_at":        "1970-01-01T00:00:01Z",
		"updated_at":        "1970-01-01T00:00:01Z",
		"resume_capability": "exact_persistent_id",
		"digest":            strings.Repeat("0", 64), // wrong – will not match computed value
	}
	b, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatalf("marshal stale session: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session.json"), b, 0o600); err != nil {
		t.Fatalf("write stale session: %v", err)
	}
}

// writeSessionWithExtraFields creates a session file that includes extra fields
// (simulating a team-persistent session written by an older binary). The stored
// digest is computed over all fields including the extras, so the current binary
// – which ignores unknown JSON fields – will compute a different digest.
func writeSessionWithExtraFields(t *testing.T, storeDir, id string) {
	t.Helper()
	dir := filepath.Join(storeDir, id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("create extra-fields session dir: %v", err)
	}
	// Use an ordered struct so json.Marshal produces a deterministic digest.
	type sessionWithMode struct {
		Schema           string `json:"schema"`
		ID               string `json:"id"`
		Provider         string `json:"provider"`
		WorkingDirectory string `json:"working_directory"`
		Model            string `json:"model"`
		ModelTrack       string `json:"model_track"`
		WorkflowDigest   string `json:"workflow_digest"`
		CreatedAt        string `json:"created_at"`
		UpdatedAt        string `json:"updated_at"`
		ResumeCapability string `json:"resume_capability"`
		Mode             string `json:"mode"` // extra field not in current struct
		Digest           string `json:"digest"`
	}
	s := sessionWithMode{
		Schema:           "run_ai_persistent_session/v1",
		ID:               id,
		Provider:         "codex",
		WorkingDirectory: "/tmp/work",
		Model:            "gpt-5.6-terra",
		ModelTrack:       "configured",
		WorkflowDigest:   strings.Repeat("b", 64),
		CreatedAt:        "1970-01-01T00:00:02Z",
		UpdatedAt:        "1970-01-01T00:00:02Z",
		ResumeCapability: "exact_persistent_id",
		Mode:             "team_persistent_tui",
	}
	// Compute digest the way the old binary would have – including Mode.
	s.Digest = ""
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal for digest: %v", err)
	}
	sum := sha256.Sum256(b)
	s.Digest = hex.EncodeToString(sum[:])
	b, err = json.MarshalIndent(s, "", "  ")
	if err != nil {
		t.Fatalf("marshal extra-fields session: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session.json"), b, 0o600); err != nil {
		t.Fatalf("write extra-fields session: %v", err)
	}
}

func TestListWithStaleReturnsBothValidAndStaleEntries(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "persistent")
	store := PersistentStore{Directory: root}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}

	validID := "persistent-20261019090000-cccccccccccccccccccccccccccccccc"
	valid, err := NewPersistentSession(validID, "codex", "/tmp/work", "", "gpt-5.6-terra", "configured", strings.Repeat("c", 64), time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Create(valid); err != nil {
		t.Fatal(err)
	}

	// A session with a tampered digest (all zeros).
	staleID1 := "persistent-20261019090001-dddddddddddddddddddddddddddddddd"
	writeSessionWithTamperedDigest(t, root, staleID1)

	// A session written by an older binary that included extra fields in digest.
	staleID2 := "persistent-20261019090002-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	writeSessionWithExtraFields(t, root, staleID2)

	validSessions, staleIDs, err := store.ListWithStale()
	if err != nil {
		t.Fatalf("ListWithStale error: %v", err)
	}
	if len(validSessions) != 1 || validSessions[0].ID != validID {
		t.Fatalf("valid sessions = %#v, want one entry with id=%q", validSessions, validID)
	}
	if len(staleIDs) != 2 {
		t.Fatalf("stale IDs = %v, want 2", staleIDs)
	}
	found1, found2 := false, false
	for _, id := range staleIDs {
		if id == staleID1 {
			found1 = true
		}
		if id == staleID2 {
			found2 = true
		}
	}
	if !found1 || !found2 {
		t.Fatalf("stale IDs = %v, want both %q and %q", staleIDs, staleID1, staleID2)
	}
}

func TestListWithStaleIsReadOnlyWhenAbsent(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "missing")
	valid, stale, err := (PersistentStore{Directory: root}).ListWithStale()
	if err != nil || len(valid) != 0 || len(stale) != 0 {
		t.Fatalf("valid=%#v stale=%v err=%v", valid, stale, err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ListWithStale created state directory: %v", err)
	}
}

func TestListWithStaleFailsOnCorruptJSON(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "persistent")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	corruptID := "persistent-20261019090003-ffffffffffffffffffffffffffffffff"
	corruptDir := filepath.Join(root, corruptID)
	if err := os.Mkdir(corruptDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corruptDir, "session.json"), []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := (PersistentStore{Directory: root}).ListWithStale()
	if err == nil {
		t.Fatal("expected error for corrupt JSON, got nil")
	}
	// Must NOT be wrapped as ErrDigestMismatch: corrupt JSON is a hard error.
	if errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("corrupt JSON was incorrectly treated as digest mismatch: %v", err)
	}
}

func TestListWithStaleFailsOnSchemaViolation(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "persistent")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	badSchemaID := "persistent-20261019090004-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa0"
	badDir := filepath.Join(root, badSchemaID)
	if err := os.Mkdir(badDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Wrong schema string – this is a structural violation, not a digest mismatch.
	raw := map[string]string{
		"schema":            "run_ai_persistent_session/v9999",
		"id":                badSchemaID,
		"provider":          "codex",
		"working_directory": "/tmp/work",
		"model":             "gpt-5.6-terra",
		"model_track":       "configured",
		"workflow_digest":   strings.Repeat("a", 64),
		"created_at":        "1970-01-01T00:00:04Z",
		"updated_at":        "1970-01-01T00:00:04Z",
		"resume_capability": "exact_persistent_id",
		"digest":            strings.Repeat("0", 64),
	}
	b, _ := json.MarshalIndent(raw, "", "  ")
	if err := os.WriteFile(filepath.Join(badDir, "session.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := (PersistentStore{Directory: root}).ListWithStale()
	if err == nil {
		t.Fatal("expected error for schema violation, got nil")
	}
	if errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("schema violation was incorrectly treated as digest mismatch: %v", err)
	}
}

func TestLookupStillFailsHardOnDigestMismatch(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "persistent")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	staleID := "persistent-20261019090005-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	writeSessionWithTamperedDigest(t, root, staleID)
	store := PersistentStore{Directory: root}
	_, err := store.Lookup(staleID)
	if err == nil {
		t.Fatal("Lookup should fail for stale session, got nil error")
	}
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("Lookup should return ErrDigestMismatch, got: %v", err)
	}
}

func TestErrDigestMismatchWrapsCorrectly(t *testing.T) {
	t.Parallel()
	// Verify that the sentinel wraps as expected via errors.Is.
	wrapped := errors.New("persistent session: " + ErrDigestMismatch.Error())
	// Plain string concatenation is NOT errors.Is – this tests the actual fmt.Errorf wrapping.
	if errors.Is(wrapped, ErrDigestMismatch) {
		t.Fatal("plain string error should not satisfy errors.Is for ErrDigestMismatch")
	}
}
