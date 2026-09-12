package fleet

import (
	"os/exec"
	"syscall"
)

const notImplemented = "not implemented"

// Supervisor owns the supervised crush server child processes (stdlib
// os/exec, DESIGN §10.1). It records exits so reconciliation can react
// on its next tick rather than reactively.
type Supervisor struct{}

// NewSupervisor builds the supervisor for one daemon process.
func NewSupervisor() *Supervisor { panic(notImplemented) }

// Child is one supervised crush server process.
type Child struct{}

// Spawn starts a server child with the given argv and env. env is
// Matchmaker's selected environment allowlist (DESIGN §5.1, §6): the child
// never inherits the full parent environment, and lifecycle tunables
// (CRUSH_SERVER_DETACH_GRACE, CRUSH_SERVER_IDLE_TIMEOUT) are set here
// deliberately. Spawn is argv-only direct exec with no shell invocation
// (THREAT_MODEL TB3).
func (s *Supervisor) Spawn(project string, argv, env []string) (*Child, error) {
	panic(notImplemented)
}

// Pid returns the child's process ID, 0 if it has exited.
func (c *Child) Pid() int { panic(notImplemented) }

// Exited reports whether the child has exited; exit status is surfaced to
// the reconciliation loop (crash-loop quarantine input, §5.1).
func (c *Child) Exited() bool { panic(notImplemented) }

// Signal delivers a teardown escalation signal after the drain timeouts
// expire (DESIGN §5.1). Escalation is SIGTERM then SIGKILL.
func (c *Child) Signal(sig syscall.Signal) error { panic(notImplemented) }

// Stop removes the child from supervision; it does not signal.
func (c *Child) Stop() error { panic(notImplemented) }

// Ensure exec is referenced for the scaffold; Spawn uses exec.Command in
// the implementation.
var _ = exec.Command
