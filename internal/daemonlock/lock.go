// Package daemonlock implements the OS-level exclusive lock guarding the
// state directory: exactly one Matchmaker daemon owns the store,
// reconciliation, children, SSE claims, and the coordination MCP listener
// (DESIGN §3).
package daemonlock

const notImplemented = "not implemented"

// Lock is the held exclusive lock. The lock file records daemon identity
// and socket path for diagnostics, but that record is never treated as
// proof of ownership without the OS lock.
type Lock struct{}

// Acquire takes the OS-level exclusive lock on the state directory and
// writes daemon identity diagnostics. It returns an error if another
// owner is live.
func Acquire(stateDir string) (*Lock, error) { panic(notImplemented) }

// Release drops the lock on clean shutdown.
func (l *Lock) Release() error { panic(notImplemented) }

// SocketPath returns the local RPC socket path recorded by the lock
// holder.
func (l *Lock) SocketPath() string { panic(notImplemented) }
