package legacy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/vaclavambroz/run-ai/internal/providers"
	"github.com/vaclavambroz/run-ai/internal/sessions"
)

const maxLegacyMapBytes = 8 << 20

type MapPaths map[providers.Name]string

func CurrentMapPaths(codexRoot string) MapPaths {
	if codexRoot == "" {
		return MapPaths{}
	}
	access := filepath.Join(codexRoot, "access")
	return MapPaths{
		providers.Codex:       filepath.Join(access, "SESSIONS.md"),
		providers.Claude:      filepath.Join(access, "CLAUDE_SESSIONS.md"),
		providers.Antigravity: filepath.Join(access, "ANTIGRAVITY_SESSIONS.md"),
	}
}

type MapStore struct {
	Paths MapPaths
}

func NewMapStore(paths MapPaths) *MapStore {
	copyPaths := make(MapPaths, len(paths))
	for provider, path := range paths {
		copyPaths[provider] = path
	}
	return &MapStore{Paths: copyPaths}
}

func (s *MapStore) path(provider providers.Name) (string, error) {
	path := s.Paths[provider]
	if path == "" {
		return "", fmt.Errorf("legacy map path is not configured for provider %q", provider)
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("legacy map path for provider %q is not absolute", provider)
	}
	return path, nil
}

func (s *MapStore) Lookup(ctx context.Context, provider providers.Name, key string) (sessions.Mapping, error) {
	if err := ctx.Err(); err != nil {
		return sessions.Mapping{}, err
	}
	if err := sessions.ValidateKey(key); err != nil {
		return sessions.Mapping{}, err
	}
	path, err := s.path(provider)
	if err != nil {
		return sessions.Mapping{}, err
	}
	lock, err := openLock(path + ".lock")
	if err != nil {
		return sessions.Mapping{}, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_SH); err != nil {
		return sessions.Mapping{}, fmt.Errorf("lock legacy map: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck

	data, mode, err := readRegular(path)
	_ = mode
	if errors.Is(err, os.ErrNotExist) {
		return sessions.Mapping{}, sessions.ErrNotFound
	}
	if err != nil {
		return sessions.Mapping{}, err
	}
	values, err := parseMap(data)
	if err != nil {
		return sessions.Mapping{}, fmt.Errorf("parse %s: %w", path, err)
	}
	id, ok := values[key]
	if !ok {
		return sessions.Mapping{}, sessions.ErrNotFound
	}
	if err := sessions.ValidateID(id); err != nil {
		return sessions.Mapping{}, fmt.Errorf("legacy mapping %s: %w", key, err)
	}
	return sessions.Mapping{Provider: provider, Key: key, ID: id}, nil
}

func (s *MapStore) Put(ctx context.Context, mapping sessions.Mapping) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := providers.Lookup(mapping.Provider); !ok {
		return fmt.Errorf("unknown provider %q", mapping.Provider)
	}
	if err := sessions.ValidateKey(mapping.Key); err != nil {
		return err
	}
	if err := sessions.ValidateID(mapping.ID); err != nil {
		return err
	}
	path, err := s.path(mapping.Provider)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create legacy map directory: %w", err)
	}
	lock, err := openLock(path + ".lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock legacy map: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck

	data, mode, err := readRegular(path)
	if errors.Is(err, os.ErrNotExist) {
		data, mode = nil, 0o600
	} else if err != nil {
		return err
	}
	updated, err := updateMap(data, mapping.Key, mapping.ID)
	if err != nil {
		return fmt.Errorf("update %s: %w", path, err)
	}
	if string(updated) == string(data) {
		return nil
	}
	return atomicWrite(path, updated, mode.Perm())
}

func parseMap(data []byte) (map[string]string, error) {
	if len(data) > maxLegacyMapBytes {
		return nil, errors.New("legacy map exceeds 8 MiB")
	}
	values := make(map[string]string)
	for number, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("line %d is not key=value", number+1)
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if err := sessions.ValidateKey(key); err != nil {
			return nil, fmt.Errorf("line %d key: %w", number+1, err)
		}
		if err := sessions.ValidateID(value); err != nil {
			return nil, fmt.Errorf("line %d value: %w", number+1, err)
		}
		if _, duplicate := values[key]; duplicate {
			return nil, fmt.Errorf("line %d duplicates key %q", number+1, key)
		}
		values[key] = value
	}
	return values, nil
}

func updateMap(data []byte, key, value string) ([]byte, error) {
	if _, err := parseMap(data); err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	found := false
	for index, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if strings.TrimSpace(parts[0]) != key {
			continue
		}
		lines[index] = key + "=" + value
		found = true
	}
	if !found {
		if len(lines) == 1 && lines[0] == "" {
			lines[0] = key + "=" + value
		} else {
			if lines[len(lines)-1] != "" {
				lines = append(lines, "")
			}
			lines = append(lines, key+"="+value)
		}
	}
	result := strings.Join(lines, "\n")
	if !strings.HasSuffix(result, "\n") {
		result += "\n"
	}
	return []byte(result), nil
}

func openLock(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("lock path is a symlink: %s", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	opened, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	linked, err := os.Lstat(path)
	if err != nil || linked.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, linked) {
		file.Close()
		return nil, fmt.Errorf("lock identity changed: %s", path)
	}
	return file, nil
}

func readRegular(path string) ([]byte, os.FileMode, error) {
	linked, err := os.Lstat(path)
	if err != nil {
		return nil, 0, err
	}
	if !linked.Mode().IsRegular() || linked.Mode()&os.ModeSymlink != 0 {
		return nil, 0, fmt.Errorf("legacy map is not a regular non-symlink file: %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, 0, err
	}
	if !os.SameFile(linked, opened) {
		return nil, 0, fmt.Errorf("legacy map identity changed while opening: %s", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxLegacyMapBytes+1))
	if err != nil {
		return nil, 0, err
	}
	if len(data) > maxLegacyMapBytes {
		return nil, 0, errors.New("legacy map exceeds 8 MiB")
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(opened, after) || opened.Size() != after.Size() || opened.ModTime() != after.ModTime() {
		return nil, 0, fmt.Errorf("legacy map changed while reading: %s", path)
	}
	return data, linked.Mode(), nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	remove = false
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
