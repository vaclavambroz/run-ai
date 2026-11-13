package sessions

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type shortWriter struct {
	buffer bytes.Buffer
	limit  int
}

func (writer *shortWriter) Write(payload []byte) (int, error) {
	if len(payload) > writer.limit {
		payload = payload[:writer.limit]
	}
	return writer.buffer.Write(payload)
}

func testLineage(t *testing.T, providerID string) Lineage {
	t.Helper()
	lineage, err := NewLineageIntent(
		"codex_claude_main", "claude", "main", providerID,
		strings.Repeat("a", 64), false, time.Unix(1, 0),
	)
	if err != nil {
		t.Fatal(err)
	}
	return lineage
}

func TestFileLineageTwoPhaseIsPrivateDurableAndIdempotent(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "lineage")
	store := FileLineageStore{Directory: directory}
	intent := testLineage(t, "123e4567-e89b-42d3-a456-426614174000")
	prepared, err := store.Prepare(intent)
	if err != nil || prepared.State != "intent" {
		t.Fatalf("prepare = %#v, %v", prepared, err)
	}
	path := filepath.Join(directory, intent.TmuxName+".json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("lineage metadata = %#v, %v", info, err)
	}
	active, err := store.Activate(intent.TmuxName, intent.ManifestDigest)
	if err != nil || active.State != "active" || active.PromotionState != "unpromoted" {
		t.Fatalf("activate = %#v, %v", active, err)
	}
	replay, err := store.Prepare(intent)
	if err != nil || replay.LineageDigest != active.LineageDigest {
		t.Fatalf("idempotent replay = %#v, %v", replay, err)
	}
	if _, err := store.Activate(intent.TmuxName, intent.ManifestDigest); err != nil {
		t.Fatalf("idempotent activate: %v", err)
	}
}

func TestFileLineageListOnAbsentStateDoesNotCreateAnything(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	directory := filepath.Join(root, "not-created", "sessions")
	values, err := (FileLineageStore{Directory: directory}).List()
	if err != nil || len(values) != 0 {
		t.Fatalf("list = %#v, %v", values, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "not-created")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only list created state: %v", err)
	}
}

func TestConcurrentPrepareSelectsOneCanonicalProviderID(t *testing.T) {
	t.Parallel()
	store := &MemoryLineageStore{}
	candidates := []Lineage{testLineage(t, "id-one"), testLineage(t, "id-two")}
	results := make([]Lineage, 2)
	errors := make([]error, 2)
	var wait sync.WaitGroup
	for index := range candidates {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			results[index], errors[index] = store.Prepare(candidates[index])
		}(index)
	}
	wait.Wait()
	if errors[0] != nil || errors[1] != nil || results[0].ProviderSessionID != results[1].ProviderSessionID {
		t.Fatalf("results=%#v errors=%v", results, errors)
	}
}

func TestFileLineageRejectsSymlinkAndBindingMismatch(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "lineage")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	intent := testLineage(t, "id-one")
	if err := os.Symlink("/etc/passwd", filepath.Join(directory, intent.TmuxName+".json")); err != nil {
		t.Fatal(err)
	}
	store := FileLineageStore{Directory: directory}
	if _, err := store.Prepare(intent); err == nil {
		t.Fatal("symlink lineage unexpectedly accepted")
	}
}

func TestFileLineageListFindAndExactJSON(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "lineage")
	store := FileLineageStore{Directory: directory}
	intent := testLineage(t, "exact-provider-id")
	if _, err := store.Prepare(intent); err != nil {
		t.Fatal(err)
	}
	values, err := store.List()
	if err != nil || len(values) != 1 || values[0].ResumeCapability != ResumeExactProviderID {
		t.Fatalf("list = %#v, %v", values, err)
	}
	found, err := store.Find(intent.Provider, intent.SessionKey, intent.ProviderSessionID, intent.ManifestDigest)
	if err != nil || found.TmuxName != intent.TmuxName {
		t.Fatalf("find = %#v, %v", found, err)
	}

	path := filepath.Join(directory, intent.TmuxName+".json")
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(payload, &raw); err != nil {
		t.Fatal(err)
	}
	raw["unexpected"] = true
	payload, err = json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Lookup(intent.TmuxName); err == nil {
		t.Fatal("unknown lineage field unexpectedly accepted")
	}
}

func TestFileLineageRejectsSymlinkAncestorAndLockLeaf(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	realDirectory := filepath.Join(root, "real")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	ancestor := filepath.Join(root, "linked")
	if err := os.Symlink(realDirectory, ancestor); err != nil {
		t.Fatal(err)
	}
	if _, err := (FileLineageStore{Directory: filepath.Join(ancestor, "sessions")}).Prepare(testLineage(t, "id")); err == nil {
		t.Fatal("symlink ancestor unexpectedly accepted")
	}

	directory := filepath.Join(root, "safe")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(directory, ".lock")); err != nil {
		t.Fatal(err)
	}
	if _, err := (FileLineageStore{Directory: directory}).Prepare(testLineage(t, "id")); err == nil {
		t.Fatal("symlink lock unexpectedly accepted")
	}
}

func TestWriteAllHandlesShortWritesAndRejectsZeroProgress(t *testing.T) {
	t.Parallel()
	writer := &shortWriter{limit: 2}
	payload := []byte("complete durable payload")
	if err := writeAll(writer, payload); err != nil || !bytes.Equal(writer.buffer.Bytes(), payload) {
		t.Fatalf("short writes: bytes=%q err=%v", writer.buffer.Bytes(), err)
	}
	if err := writeAll(io.Discard, nil); err != nil {
		t.Fatalf("empty payload: %v", err)
	}
	if err := writeAll(zeroWriter{}, []byte("x")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("zero progress error = %v", err)
	}
}

type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) { return 0, nil }
