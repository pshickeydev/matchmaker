package daemonlock

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAcquireReleaseCycle(t *testing.T) {
	dir := t.TempDir()
	lock, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got := lock.SocketPath(); got != filepath.Join(dir, sockFile) {
		t.Errorf("SocketPath = %q", got)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// After release the lock is acquirable again.
	lock2, err := Acquire(dir)
	if err != nil {
		t.Fatalf("re-Acquire after release: %v", err)
	}
	lock2.Release()
}

func TestTwoDaemonContention(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(dir)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer first.Release()
	if _, err := Acquire(dir); err == nil {
		t.Fatal("second Acquire succeeded while first daemon holds the lock")
	}
}

func TestLockFilePermissionsAndIdentity(t *testing.T) {
	dir := t.TempDir()
	lock, err := Acquire(dir)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer lock.Release()
	info, err := os.Stat(filepath.Join(dir, lockFile))
	if err != nil {
		t.Fatalf("stat lock file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("lock file perms = %o, want 0600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat state dir: %v", err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("state dir perms = %o, want 0700", dirInfo.Mode().Perm())
	}
	data, err := os.ReadFile(filepath.Join(dir, lockFile))
	if err != nil {
		t.Fatalf("read lock file: %v", err)
	}
	if len(data) == 0 {
		t.Error("lock file carries no identity record")
	}
}
