package sessions

// Persistent app-server sessions deliberately use a separate sealed record
// from tmux lineages.  A Codex thread ID is not a tmux identity and must never
// be guessed or silently promoted into one.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const persistentSchema = "run_ai_persistent_session/v1"

var ErrPersistentNotFound = errors.New("persistent session not found")

// ErrDigestMismatch is returned when a session file has a valid structure and
// schema but its stored digest does not match the value computed from the
// current struct fields. This arises when metadata was written by a binary
// version that included additional fields in the digest input. Enumerate-only
// callers may degrade these entries rather than failing; all mutating
// operations (Lookup, BindThread, Create) treat it as a hard error.
var ErrDigestMismatch = errors.New("persistent session digest mismatch")

type PersistentSession struct {
	Schema           string `json:"schema"`
	ID               string `json:"id"`
	Provider         string `json:"provider"`
	ThreadID         string `json:"thread_id,omitempty"`
	WorkingDirectory string `json:"working_directory"`
	SocketPath       string `json:"socket_path,omitempty"`
	Model            string `json:"model,omitempty"`
	ModelTrack       string `json:"model_track"`
	WorkflowDigest   string `json:"workflow_digest"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
	ResumeCapability string `json:"resume_capability"`
	Digest           string `json:"digest"`
}

func NewPersistentSession(id, provider, cwd, socketPath, model, modelTrack, workflowDigest string, now time.Time) (PersistentSession, error) {
	value := PersistentSession{Schema: persistentSchema, ID: id, Provider: provider, WorkingDirectory: filepath.Clean(cwd), SocketPath: socketPath, Model: model, ModelTrack: modelTrack, WorkflowDigest: workflowDigest, CreatedAt: now.UTC().Format(time.RFC3339Nano), UpdatedAt: now.UTC().Format(time.RFC3339Nano), ResumeCapability: "exact_persistent_id"}
	return sealPersistent(value)
}

func (value PersistentSession) Validate() error {
	if value.Schema != persistentSchema || !strings.HasPrefix(value.ID, "persistent-") || value.Provider != "codex" || !filepath.IsAbs(value.WorkingDirectory) || value.ResumeCapability != "exact_persistent_id" {
		return errors.New("invalid persistent session metadata")
	}
	if err := ValidateID(value.ID); err != nil {
		return err
	}
	if value.ThreadID != "" {
		if err := ValidateID(value.ThreadID); err != nil {
			return err
		}
	}
	if value.SocketPath != "" {
		if !filepath.IsAbs(value.SocketPath) {
			return errors.New("persistent socket path must be absolute")
		}
		socketDigest := sha256.Sum256([]byte(value.ID))
		if filepath.Base(value.SocketPath) != hex.EncodeToString(socketDigest[:12])+".sock" {
			return errors.New("persistent socket path is not bound to session ID")
		}
	}
	if len(value.WorkflowDigest) != 64 || !validDigest(value.WorkflowDigest) {
		return errors.New("invalid persistent workflow digest")
	}
	if _, err := time.Parse(time.RFC3339Nano, value.CreatedAt); err != nil {
		return errors.New("invalid persistent creation time")
	}
	if _, err := time.Parse(time.RFC3339Nano, value.UpdatedAt); err != nil {
		return errors.New("invalid persistent update time")
	}
	want, computeErr := persistentDigest(value)
	if computeErr != nil {
		return fmt.Errorf("persistent session digest computation: %w", computeErr)
	}
	if value.Digest != want {
		return fmt.Errorf("persistent session: %w", ErrDigestMismatch)
	}
	return nil
}

func (value PersistentSession) WithThread(thread string, now time.Time) (PersistentSession, error) {
	if value.ThreadID != "" && value.ThreadID != thread {
		return PersistentSession{}, errors.New("persistent session is already bound to a different Codex thread")
	}
	if err := ValidateID(thread); err != nil {
		return PersistentSession{}, fmt.Errorf("invalid Codex thread ID: %w", err)
	}
	value.ThreadID = thread
	value.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
	return sealPersistent(value)
}

func sealPersistent(value PersistentSession) (PersistentSession, error) {
	digest, err := persistentDigest(value)
	if err != nil {
		return PersistentSession{}, err
	}
	value.Digest = digest
	if err := value.Validate(); err != nil {
		return PersistentSession{}, err
	}
	return value, nil
}

func persistentDigest(value PersistentSession) (string, error) {
	value.Digest = ""
	b, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(b)
	return hex.EncodeToString(digest[:]), nil
}

type PersistentStore struct{ Directory string }

func (store PersistentStore) StateDirectory(id string) (string, error) {
	if err := ValidateID(id); err != nil || !strings.HasPrefix(id, "persistent-") {
		return "", errors.New("invalid persistent session ID")
	}
	if !filepath.IsAbs(store.Directory) {
		return "", errors.New("persistent session directory must be absolute")
	}
	return filepath.Join(filepath.Clean(store.Directory), id), nil
}

func (store PersistentStore) Create(value PersistentSession) error {
	if err := value.Validate(); err != nil {
		return err
	}
	dir, err := store.StateDirectory(value.ID)
	if err != nil {
		return err
	}
	if err := ensurePrivateDirectory(filepath.Dir(dir)); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return errors.New("persistent session already exists")
		}
		return err
	}
	if err := verifyPrivateDirectory(dir); err != nil {
		return err
	}
	if err := writePersistent(filepath.Join(dir, "session.json"), value); err != nil {
		return err
	}
	return nil
}

func (store PersistentStore) Lookup(id string) (PersistentSession, error) {
	dir, err := store.StateDirectory(id)
	if err != nil {
		return PersistentSession{}, err
	}
	if err := verifyPrivateDirectory(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PersistentSession{}, ErrPersistentNotFound
		}
		return PersistentSession{}, err
	}
	return readPersistent(filepath.Join(dir, "session.json"))
}

func (store PersistentStore) BindThread(id, thread string, now time.Time) (PersistentSession, error) {
	value, err := store.Lookup(id)
	if err != nil {
		return PersistentSession{}, err
	}
	value, err = value.WithThread(thread, now)
	if err != nil {
		return PersistentSession{}, err
	}
	if err := writePersistent(filepath.Join(mustStateDir(store, id), "session.json"), value); err != nil {
		return PersistentSession{}, err
	}
	return value, nil
}

func mustStateDir(store PersistentStore, id string) string {
	dir, _ := store.StateDirectory(id)
	return dir
}

func (store PersistentStore) List() ([]PersistentSession, error) {
	if !filepath.IsAbs(store.Directory) {
		return nil, errors.New("persistent session directory must be absolute")
	}
	if err := verifyPrivateDirectory(store.Directory); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	entries, err := os.ReadDir(store.Directory)
	if err != nil {
		return nil, err
	}
	values := make([]PersistentSession, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "persistent-") {
			continue
		}
		value, readErr := store.Lookup(entry.Name())
		if readErr != nil {
			return nil, fmt.Errorf("read persistent session %s: %w", entry.Name(), readErr)
		}
		values = append(values, value)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].CreatedAt < values[j].CreatedAt })
	return values, nil
}

// ListWithStale enumerates all persistent sessions in the store directory.
// Unlike List, it treats ErrDigestMismatch as a recoverable condition: sessions
// whose stored digest does not match the computed value are returned as stale
// IDs rather than causing the entire list to fail. All other errors remain
// fatal. Stale sessions MUST NOT be resumed, terminated, or used for any
// authenticated operation; this method is for read-only inventory only.
func (store PersistentStore) ListWithStale() (valid []PersistentSession, staleIDs []string, err error) {
	if !filepath.IsAbs(store.Directory) {
		return nil, nil, errors.New("persistent session directory must be absolute")
	}
	if err := verifyPrivateDirectory(store.Directory); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	entries, err := os.ReadDir(store.Directory)
	if err != nil {
		return nil, nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "persistent-") {
			continue
		}
		value, readErr := store.Lookup(entry.Name())
		if readErr != nil {
			if errors.Is(readErr, ErrDigestMismatch) {
				staleIDs = append(staleIDs, entry.Name())
				continue
			}
			return nil, nil, fmt.Errorf("read persistent session %s: %w", entry.Name(), readErr)
		}
		valid = append(valid, value)
	}
	sort.Slice(valid, func(i, j int) bool { return valid[i].CreatedAt < valid[j].CreatedAt })
	sort.Strings(staleIDs)
	return valid, staleIDs, nil
}

func ensurePrivateDirectory(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("private directory must be absolute")
	}
	if err := createDirectoryWithoutSymlinkAncestors(path); err != nil {
		return err
	}
	return verifyPrivateDirectory(path)
}

func verifyPrivateDirectory(path string) error {
	exists, err := existingDirectoryWithoutSymlinkAncestors(path)
	if err != nil || !exists {
		if err != nil {
			return err
		}
		return os.ErrNotExist
	}
	if err := verifyOwnedAncestors(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 || !persistentOwnedByCurrentUser(info) {
		return errors.New("persistent state directory is not a private owned directory")
	}
	return nil
}

func verifyOwnedAncestors(path string) error {
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(filepath.Clean(path), string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || (int(stat.Uid) != os.Getuid() && stat.Uid != 0) {
			return errors.New("persistent state ancestor has unexpected owner")
		}
	}
	return nil
}

func persistentOwnedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid()
}

func readPersistent(path string) (PersistentSession, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return PersistentSession{}, ErrPersistentNotFound
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 || !persistentOwnedByCurrentUser(info) {
		return PersistentSession{}, errors.New("persistent metadata is not a private regular file")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return PersistentSession{}, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return PersistentSession{}, err
	}
	var value PersistentSession
	if err := json.Unmarshal(b, &value); err != nil {
		return PersistentSession{}, err
	}
	if err := value.Validate(); err != nil {
		return PersistentSession{}, err
	}
	return value, nil
}

func writePersistent(path string, value PersistentSession) error {
	if err := value.Validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + fmt.Sprintf(".tmp.%d", os.Getpid())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
