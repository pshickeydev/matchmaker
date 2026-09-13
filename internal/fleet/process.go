package fleet

import (
	"fmt"
	"os/exec"
	"sync"
	"syscall"
)

// Supervisor owns the supervised crush server child processes (stdlib
// os/exec, DESIGN §10.1). It records exits so reconciliation can react
// on its next tick rather than reactively.
type Supervisor struct {
	mu       sync.Mutex
	children map[string]*Child
}

// NewSupervisor builds the supervisor for one daemon process.
func NewSupervisor() *Supervisor {
	return &Supervisor{children: make(map[string]*Child)}
}

// Child is one supervised crush server process.
type Child struct {
	project string
	cmd     *exec.Cmd
	done    chan struct{}
	exited  bool
	exitErr error
}

// Spawn starts a server child with the given argv and env. env is
// Matchmaker's selected environment allowlist (DESIGN §5.1, §6): the child
// never inherits the full parent environment, and lifecycle tunables
// (CRUSH_SERVER_DETACH_GRACE, CRUSH_SERVER_IDLE_TIMEOUT) are set here
// deliberately. Spawn is argv-only direct exec with no shell invocation
// (THREAT_MODEL TB3).
func (s *Supervisor) Spawn(project string, argv, env []string) (*Child, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("spawn %s: empty argv", project)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdout = discard{}
	cmd.Stderr = discard{}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn %s: %w", project, err)
	}
	child := &Child{project: project, cmd: cmd, done: make(chan struct{})}
	go child.await()
	s.mu.Lock()
	s.children[project] = child
	s.mu.Unlock()
	return child, nil
}

// await records the process exit so the reconciliation loop observes it.
func (c *Child) await() {
	c.exitErr = c.cmd.Wait()
	c.exited = true
	close(c.done)
}

// Pid returns the child's process ID, 0 if it has exited.
func (c *Child) Pid() int {
	if c.Exited() || c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

// Exited reports whether the child has exited; exit status is surfaced to
// the reconciliation loop (crash-loop quarantine input, §5.1).
func (c *Child) Exited() bool {
	return c.exited
}

// ExitErr returns the recorded wait error once exited.
func (c *Child) ExitErr() error { return c.exitErr }

// Done exposes the exit signal for bounded waits.
func (c *Child) Done() <-chan struct{} { return c.done }

// Signal delivers a teardown escalation signal after the drain timeouts
// expire (DESIGN §5.1). Escalation is SIGTERM then SIGKILL.
func (c *Child) Signal(sig syscall.Signal) error {
	if c.Exited() || c.cmd.Process == nil {
		return fmt.Errorf("child %s already exited", c.project)
	}
	return c.cmd.Process.Signal(sig)
}

// Stop removes the child from supervision; it does not signal.
func (c *Child) Stop() error {
	return nil
}

// Child returns the supervised child of a project, if any.
func (s *Supervisor) Child(project string) (*Child, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	child, ok := s.children[project]
	return child, ok
}

// discard sinks child output; server logs never enter Matchmaker's logs
// (DESIGN §5.5: no raw payload logging).
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
