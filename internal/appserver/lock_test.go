package appserver

import (
	"path/filepath"
	"testing"
)

func TestDaemonLockIsScopedToOneSocket(t *testing.T) {
	root := t.TempDir()
	first, err := acquireDaemonLock(filepath.Join(root, "one.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := acquireDaemonLock(filepath.Join(root, "one.sock")); err == nil {
		t.Fatal("same socket acquired twice")
	}
	second, err := acquireDaemonLock(filepath.Join(root, "two.sock"))
	if err != nil {
		t.Fatalf("independent socket blocked: %v", err)
	}
	defer second.Close()
}
