package audit

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const maxAuditLine = 64 << 10

// Append writes one already validated canonical receipt to a private JSONL
// file. It uses a sibling flock and O_NOFOLLOW so concurrent launchers cannot
// interleave records or redirect a privileged writer through a symlink.
func Append(path string, receipt Receipt) error {
	if !filepath.IsAbs(path) {
		return errors.New("audit path must be absolute")
	}
	payload, err := receipt.CanonicalJSON()
	if err != nil {
		return err
	}
	if len(payload)+1 > maxAuditLine {
		return errors.New("audit receipt exceeds line limit")
	}
	directory := filepath.Dir(path)
	if err := ensurePrivateDirectory(directory); err != nil {
		return err
	}
	lock, err := openNoFollow(path+".lock", syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("open audit lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock audit log: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck

	logFile, err := openNoFollow(path, syscall.O_CREAT|syscall.O_WRONLY|syscall.O_APPEND|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("open audit log: %w", err)
	}
	defer logFile.Close()
	if err := requirePrivateRegular(logFile); err != nil {
		return err
	}
	payload = append(payload, '\n')
	if err := writeAll(logFile, payload); err != nil {
		return fmt.Errorf("append audit receipt: %w", err)
	}
	if err := logFile.Sync(); err != nil {
		return fmt.Errorf("sync audit receipt: %w", err)
	}
	return nil
}

// ReadAll is a bounded verifier used by doctor and tests. It never repairs or
// skips a malformed line.
func ReadAll(path string, maxBytes int64) ([]Receipt, error) {
	if maxBytes <= 0 {
		return nil, errors.New("audit read limit must be positive")
	}
	if !filepath.IsAbs(path) {
		return nil, errors.New("audit path must be absolute")
	}
	if err := requirePrivateDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	file, err := openNoFollow(path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if err := requirePrivateRegular(file); err != nil {
		return nil, err
	}
	limited := io.LimitReader(file, maxBytes+1)
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), maxAuditLine)
	var receipts []Receipt
	var total int64
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		total += int64(len(line) + 1)
		if total > maxBytes || len(bytes.TrimSpace(line)) == 0 {
			return nil, errors.New("audit log is oversized or contains an empty line")
		}
		var receipt Receipt
		if err := strictJSON(line, &receipt); err != nil {
			return nil, fmt.Errorf("invalid audit line %d: %w", len(receipts)+1, err)
		}
		if err := receipt.Validate(); err != nil {
			return nil, fmt.Errorf("invalid audit receipt %d: %w", len(receipts)+1, err)
		}
		receipts = append(receipts, receipt)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return receipts, nil
}

func ensurePrivateDirectory(path string) error {
	return walkDirectoryChain(path, true)
}

func requirePrivateDirectory(path string) error {
	return walkDirectoryChain(path, false)
}

// walkDirectoryChain rejects symlinks in every path component. O_NOFOLLOW on
// the leaf file alone is insufficient because an attacker-controlled ancestor
// could otherwise redirect the audit log. Newly created components are always
// private; pre-existing ancestors may be shared, but the final state directory
// must already be private.
func walkDirectoryChain(path string, create bool) error {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return errors.New("audit directory must be absolute")
	}
	current := string(filepath.Separator)
	parts := strings.Split(strings.TrimPrefix(path, current), string(filepath.Separator))
	for index, part := range parts {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) && create {
			if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return fmt.Errorf("create audit directory component: %w", err)
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return fmt.Errorf("inspect audit directory component: %w", err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("audit directory chain contains a non-directory or symlink")
		}
		if index == len(parts)-1 && info.Mode().Perm()&0o077 != 0 {
			return errors.New("audit directory must be private")
		}
	}
	return nil
}

func openNoFollow(path string, flags int, mode uint32) (*os.File, error) {
	fd, err := syscall.Open(path, flags|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func requirePrivateRegular(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return errors.New("audit file must be a regular 0600 file")
	}
	return nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written <= 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}
