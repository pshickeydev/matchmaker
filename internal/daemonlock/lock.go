// Package daemonlock implements the OS-level exclusive lock guarding the
// state directory: exactly one Matchmaker daemon owns the store,
// reconciliation, children, SSE claims, and the coordination MCP listener
// (DESIGN §3).
package daemonlock

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const (
	// lockFile holds the OS-level exclusive lock and identity record.
	lockFile = "lock"
	// sockFile is the local RPC socket created by the daemon inside the
	// state directory; its path is recorded for diagnostics.
	sockFile = "matchmaker.sock"
)

// Lock is the held exclusive lock. The lock file records daemon identity
// and socket path for diagnostics, but that record is never treated as
// proof of ownership without the OS lock.
type Lock struct {
	mu         sync.Mutex
	file       *os.File
	socketPath string
}

// identity is the diagnostics record written under the OS lock.
type identity struct {
	PID        int    `json:"pid"`
	Host       string `json:"host"`
	StartedAt  string `json:"started_at"`
	SocketPath string `json:"socket_path"`
}

// Acquire takes the OS-level exclusive lock on the state directory and
// writes daemon identity diagnostics. It returns an error if another
// owner is live.
func Acquire(stateDir string) (*Lock, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir %s: %w", stateDir, err)
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("secure state dir %s: %w", stateDir, err)
	}
	path := filepath.Join(stateDir, lockFile)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("state directory %s is owned by another matchmaker daemon", stateDir)
	}
	lock := &Lock{file: file, socketPath: filepath.Join(stateDir, sockFile)}
	if err := lock.writeIdentity(); err != nil {
		lock.Release()
		return nil, fmt.Errorf("write lock identity: %w", err)
	}
	return lock, nil
}

// Release drops the lock on clean shutdown. It is idempotent and safe
// for concurrent calls: the daemon's Shutdown and an embedding caller's
// cleanup may both invoke it.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if unlockErr != nil {
		return fmt.Errorf("unlock: %w", unlockErr)
	}
	return closeErr
}

// SocketPath returns the local RPC socket path recorded by the lock
// holder.
func (l *Lock) SocketPath() string {
	return l.socketPath
}

// writeIdentity truncates the lock file and writes the diagnostics
// record while holding the OS lock.
func (l *Lock) writeIdentity() error {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	record := identity{
		PID:        os.Getpid(),
		Host:       host,
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
		SocketPath: l.socketPath,
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := l.file.Truncate(0); err != nil {
		return err
	}
	if _, err := l.file.WriteAt(append(data, '\n'), 0); err != nil {
		return err
	}
	return l.file.Sync()
}
