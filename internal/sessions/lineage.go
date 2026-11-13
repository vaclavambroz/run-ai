package sessions

import (
	"bytes"
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
	"sync"
	"syscall"
	"time"
)

const (
	lineageSchema   = "run_ai_session_lineage/v1"
	maxLineageBytes = 64 << 10
	maxLineages     = 4096

	ResumeExactProviderID  = "exact_provider_id"
	ResumeTmuxLifetimeOnly = "tmux_lifetime_only"
)

var ErrLineageNotFound = errors.New("session lineage not found")

type Lineage struct {
	Schema            string `json:"schema"`
	TmuxName          string `json:"tmux_name"`
	Provider          string `json:"provider"`
	SessionKey        string `json:"session_key"`
	ProviderSessionID string `json:"provider_session_id,omitempty"`
	ManifestDigest    string `json:"manifest_digest"`
	CreatedAt         string `json:"created_at"`
	ExplicitNew       bool   `json:"explicit_new"`
	ResumeCapability  string `json:"resume_capability"`
	PromotionState    string `json:"promotion_state"`
	State             string `json:"state"`
	LineageDigest     string `json:"lineage_digest"`
}

type LineageStore interface {
	// Prepare atomically creates an intent, or returns the already existing
	// compatible intent/active lineage selected by a concurrent launcher.
	Prepare(Lineage) (Lineage, error)
	Lookup(string) (Lineage, error)
	List() ([]Lineage, error)
	Find(provider, key, providerSessionID, manifestDigest string) (Lineage, error)
	Activate(string, string) (Lineage, error)
	BindProviderSessionID(string, string, string) (Lineage, error)
}

func NewLineageIntent(tmuxName, provider, key, providerSessionID, manifestDigest string, explicitNew bool, created time.Time) (Lineage, error) {
	lineage := Lineage{
		Schema: lineageSchema, TmuxName: tmuxName, Provider: provider, SessionKey: key,
		ProviderSessionID: providerSessionID, ManifestDigest: manifestDigest,
		CreatedAt: created.UTC().Format(time.RFC3339Nano), ExplicitNew: explicitNew,
		PromotionState: "unpromoted", State: "intent",
	}
	if providerSessionID == "" {
		lineage.ResumeCapability = ResumeTmuxLifetimeOnly
	} else {
		lineage.ResumeCapability = ResumeExactProviderID
	}
	return sealLineage(lineage)
}

func (lineage Lineage) Validate() error {
	if lineage.Schema != lineageSchema || lineage.PromotionState != "unpromoted" || (lineage.State != "intent" && lineage.State != "active") {
		return errors.New("invalid lineage schema or state")
	}
	if err := validateTmuxName(lineage.TmuxName); err != nil {
		return err
	}
	if err := ValidateKey(lineage.SessionKey); err != nil {
		return err
	}
	if lineage.Provider == "" || strings.ContainsAny(lineage.Provider, "\x00\r\n\t") {
		return errors.New("invalid lineage provider")
	}
	if lineage.ProviderSessionID != "" {
		if err := ValidateID(lineage.ProviderSessionID); err != nil {
			return err
		}
	}
	wantCapability := ResumeTmuxLifetimeOnly
	if lineage.ProviderSessionID != "" {
		wantCapability = ResumeExactProviderID
	}
	if lineage.ResumeCapability != wantCapability {
		return errors.New("lineage resume capability does not match provider session identity")
	}
	if _, err := time.Parse(time.RFC3339Nano, lineage.CreatedAt); err != nil {
		return errors.New("invalid lineage creation time")
	}
	for _, digest := range []string{lineage.ManifestDigest, lineage.LineageDigest} {
		if !validDigest(digest) {
			return errors.New("invalid lineage digest")
		}
	}
	digest, err := lineageDigest(lineage)
	if err != nil || digest != lineage.LineageDigest {
		return errors.New("lineage self-digest mismatch")
	}
	return nil
}

func lineageCompatible(left, right Lineage) bool {
	return left.Schema == right.Schema && left.TmuxName == right.TmuxName && left.Provider == right.Provider &&
		left.SessionKey == right.SessionKey && left.ManifestDigest == right.ManifestDigest && left.ExplicitNew == right.ExplicitNew
}

func sealLineage(lineage Lineage) (Lineage, error) {
	digest, err := lineageDigest(lineage)
	if err != nil {
		return Lineage{}, err
	}
	lineage.LineageDigest = digest
	if err := lineage.Validate(); err != nil {
		return Lineage{}, err
	}
	return lineage, nil
}

func lineageDigest(lineage Lineage) (string, error) {
	lineage.LineageDigest = ""
	payload, err := json.Marshal(lineage)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}

type FileLineageStore struct{ Directory string }

func (store FileLineageStore) Prepare(candidate Lineage) (Lineage, error) {
	if candidate.State != "intent" {
		return Lineage{}, errors.New("lineage prepare requires intent state")
	}
	if err := candidate.Validate(); err != nil {
		return Lineage{}, err
	}
	var result Lineage
	err := store.withLock(func() error {
		existing, err := store.lookupUnlocked(candidate.TmuxName)
		if err == nil {
			if !lineageCompatible(existing, candidate) {
				return errors.New("existing lineage belongs to a different request")
			}
			result = existing
			return nil
		}
		if !errors.Is(err, ErrLineageNotFound) {
			return err
		}
		if err := store.publishNew(candidate); err != nil {
			return err
		}
		result = candidate
		return nil
	})
	return result, err
}

func (store FileLineageStore) Lookup(tmuxName string) (Lineage, error) {
	var result Lineage
	err := store.withLock(func() error {
		var err error
		result, err = store.lookupUnlocked(tmuxName)
		return err
	})
	return result, err
}

func (store FileLineageStore) List() ([]Lineage, error) {
	result := make([]Lineage, 0)
	exists, err := existingDirectoryWithoutSymlinkAncestors(store.Directory)
	if err != nil {
		return nil, err
	}
	if !exists {
		return result, nil
	}
	err = store.withExistingLock(func() error {
		entries, err := os.ReadDir(store.Directory)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name := entry.Name()
			if name == ".lock" {
				continue
			}
			if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() || !strings.HasSuffix(name, ".json") {
				return fmt.Errorf("unexpected lineage directory entry %q requires review", name)
			}
			tmuxName := strings.TrimSuffix(name, ".json")
			if len(result) >= maxLineages {
				return errors.New("lineage directory exceeds bounded entry count")
			}
			lineage, err := store.lookupUnlocked(tmuxName)
			if err != nil {
				return err
			}
			result = append(result, lineage)
		}
		sort.Slice(result, func(i, j int) bool { return result[i].TmuxName < result[j].TmuxName })
		return nil
	})
	return result, err
}

func (store FileLineageStore) withExistingLock(action func() error) error {
	if !filepath.IsAbs(store.Directory) {
		return errors.New("lineage directory must be absolute")
	}
	exists, err := existingDirectoryWithoutSymlinkAncestors(store.Directory)
	if err != nil {
		return err
	}
	if !exists {
		return ErrLineageNotFound
	}
	before, err := os.Lstat(store.Directory)
	if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 || before.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(before) {
		return errors.New("lineage directory is not a private non-symlink directory")
	}
	lockPath := filepath.Join(store.Directory, ".lock")
	lockFD, err := syscall.Open(lockPath, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	var lock *os.File
	if err == nil {
		lock = os.NewFile(uintptr(lockFD), lockPath)
		defer lock.Close()
		lockInfo, statErr := lock.Stat()
		if statErr != nil || !lockInfo.Mode().IsRegular() || lockInfo.Mode().Perm() != 0o600 || !ownedByCurrentUser(lockInfo) {
			return errors.New("lineage lock is not a private regular file")
		}
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_SH); err != nil {
			return err
		}
		defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := action(); err != nil {
		return err
	}
	after, err := os.Lstat(store.Directory)
	if err != nil || !os.SameFile(before, after) || before.ModTime() != after.ModTime() || before.Mode() != after.Mode() {
		return errors.New("lineage directory changed while listing")
	}
	return nil
}

func (store FileLineageStore) Find(provider, key, providerSessionID, manifestDigest string) (Lineage, error) {
	if provider == "" || providerSessionID == "" || !validDigest(manifestDigest) {
		return Lineage{}, errors.New("invalid lineage lookup identity")
	}
	if err := ValidateKey(key); err != nil {
		return Lineage{}, err
	}
	if err := ValidateID(providerSessionID); err != nil {
		return Lineage{}, err
	}
	values, err := store.List()
	if err != nil {
		return Lineage{}, err
	}
	var match *Lineage
	for index := range values {
		lineage := values[index]
		if lineage.Provider != provider || lineage.SessionKey != key || lineage.ProviderSessionID != providerSessionID || lineage.ManifestDigest != manifestDigest {
			continue
		}
		if match != nil {
			return Lineage{}, errors.New("ambiguous lineage lookup identity")
		}
		copy := lineage
		match = &copy
	}
	if match == nil {
		return Lineage{}, ErrLineageNotFound
	}
	return *match, nil
}

func (store FileLineageStore) Activate(tmuxName, manifestDigest string) (Lineage, error) {
	if !validDigest(manifestDigest) {
		return Lineage{}, errors.New("invalid activation manifest digest")
	}
	var result Lineage
	err := store.withLock(func() error {
		lineage, err := store.lookupUnlocked(tmuxName)
		if err != nil {
			return err
		}
		if lineage.ManifestDigest != manifestDigest {
			return errors.New("lineage activation manifest mismatch")
		}
		if lineage.State == "active" {
			result = lineage
			return nil
		}
		lineage.State = "active"
		lineage, err = sealLineage(lineage)
		if err != nil {
			return err
		}
		if err := store.replace(lineage); err != nil {
			return err
		}
		result = lineage
		return nil
	})
	return result, err
}

// BindProviderSessionID records the exact provider identity discovered after a
// fresh provider launch. It is intentionally monotonic: an already bound
// lineage may only be bound to the identical ID.
func (store FileLineageStore) BindProviderSessionID(tmuxName, manifestDigest, providerSessionID string) (Lineage, error) {
	if err := ValidateID(providerSessionID); err != nil {
		return Lineage{}, err
	}
	var result Lineage
	err := store.withLock(func() error {
		lineage, err := store.lookupUnlocked(tmuxName)
		if err != nil {
			return err
		}
		if lineage.ManifestDigest != manifestDigest {
			return errors.New("lineage provider binding manifest mismatch")
		}
		if lineage.ProviderSessionID != "" && lineage.ProviderSessionID != providerSessionID {
			return errors.New("lineage provider session ID is already bound differently")
		}
		if lineage.ProviderSessionID == providerSessionID {
			result = lineage
			return nil
		}
		lineage.ProviderSessionID = providerSessionID
		lineage.ResumeCapability = ResumeExactProviderID
		lineage, err = sealLineage(lineage)
		if err != nil {
			return err
		}
		if err := store.replace(lineage); err != nil {
			return err
		}
		result = lineage
		return nil
	})
	return result, err
}

func (store FileLineageStore) withLock(action func() error) error {
	if !filepath.IsAbs(store.Directory) {
		return errors.New("lineage directory must be absolute")
	}
	if err := createDirectoryWithoutSymlinkAncestors(store.Directory); err != nil {
		return err
	}
	info, err := os.Lstat(store.Directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(info) {
		return errors.New("lineage directory is not a private non-symlink directory")
	}
	lockPath := filepath.Join(store.Directory, ".lock")
	lockFD, err := syscall.Open(lockPath, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	lock := os.NewFile(uintptr(lockFD), lockPath)
	defer lock.Close()
	lockInfo, err := lock.Stat()
	if err != nil || !lockInfo.Mode().IsRegular() || lockInfo.Mode().Perm() != 0o600 || !ownedByCurrentUser(lockInfo) {
		return errors.New("lineage lock is not a private regular file")
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return action()
}

func createDirectoryWithoutSymlinkAncestors(path string) error {
	clean := filepath.Clean(path)
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(clean, string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return fmt.Errorf("create lineage directory component: %w", err)
			}
			info, err = os.Lstat(current)
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("lineage directory ancestor %q is not a real directory", current)
		}
	}
	return nil
}

func existingDirectoryWithoutSymlinkAncestors(path string) (bool, error) {
	if !filepath.IsAbs(path) {
		return false, errors.New("lineage directory must be absolute")
	}
	clean := filepath.Clean(path)
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(clean, string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return false, fmt.Errorf("lineage directory ancestor %q is not a real directory", current)
		}
	}
	return true, nil
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid()
}

func (store FileLineageStore) lookupUnlocked(tmuxName string) (Lineage, error) {
	if err := validateTmuxName(tmuxName); err != nil {
		return Lineage{}, err
	}
	path := filepath.Join(store.Directory, tmuxName+".json")
	linked, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Lineage{}, ErrLineageNotFound
	}
	if err != nil || !linked.Mode().IsRegular() || linked.Mode()&os.ModeSymlink != 0 || linked.Mode().Perm() != 0o600 || !ownedByCurrentUser(linked) {
		return Lineage{}, errors.New("lineage is not a private regular file")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return Lineage{}, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(linked, opened) || !ownedByCurrentUser(opened) {
		return Lineage{}, errors.New("lineage identity changed while opening")
	}
	payload, err := io.ReadAll(io.LimitReader(file, maxLineageBytes+1))
	if err != nil || len(payload) > maxLineageBytes {
		return Lineage{}, errors.New("lineage read failed or exceeded limit")
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(opened, after) || opened.Size() != after.Size() || opened.ModTime() != after.ModTime() {
		return Lineage{}, errors.New("lineage changed while reading")
	}
	var lineage Lineage
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&lineage); err != nil {
		return Lineage{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Lineage{}, errors.New("lineage contains trailing JSON data")
	}
	if lineage.TmuxName != tmuxName || lineage.Validate() != nil {
		return Lineage{}, errors.New("invalid lineage content")
	}
	return lineage, nil
}

func (store FileLineageStore) publishNew(lineage Lineage) error {
	payload, err := json.MarshalIndent(lineage, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	temporary, err := store.writeTemp(payload)
	if err != nil {
		return err
	}
	defer os.Remove(temporary) //nolint:errcheck
	path := filepath.Join(store.Directory, lineage.TmuxName+".json")
	if err := os.Link(temporary, path); err != nil {
		return fmt.Errorf("publish lineage without overwrite: %w", err)
	}
	if err := syncDirectory(store.Directory); err != nil {
		return err
	}
	if err := os.Remove(temporary); err != nil {
		return err
	}
	return syncDirectory(store.Directory)
}

func (store FileLineageStore) replace(lineage Lineage) error {
	payload, err := json.MarshalIndent(lineage, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	temporary, err := store.writeTemp(payload)
	if err != nil {
		return err
	}
	defer os.Remove(temporary) //nolint:errcheck
	path := filepath.Join(store.Directory, lineage.TmuxName+".json")
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	return syncDirectory(store.Directory)
}

func (store FileLineageStore) writeTemp(payload []byte) (string, error) {
	file, err := os.CreateTemp(store.Directory, ".lineage.tmp-")
	if err != nil {
		return "", err
	}
	path := file.Name()
	fail := func(err error) (string, error) { file.Close(); os.Remove(path); return "", err }
	if err := file.Chmod(0o600); err != nil {
		return fail(err)
	}
	if err := writeAll(file, payload); err != nil {
		return fail(err)
	}
	if err := file.Sync(); err != nil {
		return fail(err)
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

func writeAll(writer io.Writer, payload []byte) error {
	written := 0
	for written < len(payload) {
		count, err := writer.Write(payload[written:])
		if count < 0 || count > len(payload)-written {
			return errors.New("lineage writer returned invalid byte count")
		}
		written += count
		if err != nil {
			return err
		}
		if count == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

type MemoryLineageStore struct {
	mu     sync.Mutex
	Values map[string]Lineage
}

func (store *MemoryLineageStore) Prepare(candidate Lineage) (Lineage, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if candidate.State != "intent" {
		return Lineage{}, errors.New("lineage prepare requires intent state")
	}
	if err := candidate.Validate(); err != nil {
		return Lineage{}, err
	}
	if store.Values == nil {
		store.Values = make(map[string]Lineage)
	}
	if existing, ok := store.Values[candidate.TmuxName]; ok {
		if !lineageCompatible(existing, candidate) {
			return Lineage{}, errors.New("existing lineage belongs to a different request")
		}
		return existing, nil
	}
	store.Values[candidate.TmuxName] = candidate
	return candidate, nil
}

func (store *MemoryLineageStore) Lookup(tmuxName string) (Lineage, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	lineage, ok := store.Values[tmuxName]
	if !ok {
		return Lineage{}, ErrLineageNotFound
	}
	if err := lineage.Validate(); err != nil {
		return Lineage{}, err
	}
	return lineage, nil
}

func (store *MemoryLineageStore) List() ([]Lineage, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	values := make([]Lineage, 0, len(store.Values))
	for _, lineage := range store.Values {
		if err := lineage.Validate(); err != nil {
			return nil, err
		}
		values = append(values, lineage)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].TmuxName < values[j].TmuxName })
	return values, nil
}

func (store *MemoryLineageStore) Find(provider, key, providerSessionID, manifestDigest string) (Lineage, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var result *Lineage
	for _, lineage := range store.Values {
		if lineage.Provider != provider || lineage.SessionKey != key || lineage.ProviderSessionID != providerSessionID || lineage.ManifestDigest != manifestDigest {
			continue
		}
		if err := lineage.Validate(); err != nil {
			return Lineage{}, err
		}
		if result != nil {
			return Lineage{}, errors.New("ambiguous lineage lookup identity")
		}
		copy := lineage
		result = &copy
	}
	if result == nil {
		return Lineage{}, ErrLineageNotFound
	}
	return *result, nil
}

func (store *MemoryLineageStore) Activate(tmuxName, manifestDigest string) (Lineage, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	lineage, ok := store.Values[tmuxName]
	if !ok {
		return Lineage{}, ErrLineageNotFound
	}
	if lineage.ManifestDigest != manifestDigest {
		return Lineage{}, errors.New("lineage activation manifest mismatch")
	}
	if lineage.State != "active" {
		lineage.State = "active"
		var err error
		lineage, err = sealLineage(lineage)
		if err != nil {
			return Lineage{}, err
		}
		store.Values[tmuxName] = lineage
	}
	return lineage, nil
}

func (store *MemoryLineageStore) BindProviderSessionID(tmuxName, manifestDigest, providerSessionID string) (Lineage, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := ValidateID(providerSessionID); err != nil {
		return Lineage{}, err
	}
	lineage, ok := store.Values[tmuxName]
	if !ok {
		return Lineage{}, ErrLineageNotFound
	}
	if lineage.ManifestDigest != manifestDigest {
		return Lineage{}, errors.New("lineage provider binding manifest mismatch")
	}
	if lineage.ProviderSessionID != "" && lineage.ProviderSessionID != providerSessionID {
		return Lineage{}, errors.New("lineage provider session ID is already bound differently")
	}
	if lineage.ProviderSessionID == providerSessionID {
		return lineage, nil
	}
	lineage.ProviderSessionID = providerSessionID
	lineage.ResumeCapability = ResumeExactProviderID
	var err error
	lineage, err = sealLineage(lineage)
	if err != nil {
		return Lineage{}, err
	}
	store.Values[tmuxName] = lineage
	return lineage, nil
}
