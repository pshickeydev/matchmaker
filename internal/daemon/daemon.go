// Package daemon wires and runs the single Matchmaker daemon: it owns the
// SQLite store, reconciliation, Crush children, SSE claims, and the
// coordination MCP listener exclusively (DESIGN §3).
package daemon

import (
	"context"

	"github.com/pshickeydev/matchmaker/internal/config"
	"github.com/pshickeydev/matchmaker/internal/model"
	"github.com/pshickeydev/matchmaker/internal/rpc"
)

const notImplemented = "not implemented"

// Daemon is the long-running process.
type Daemon struct{}

// New builds the daemon: acquire the OS-level exclusive lock on the state
// directory, open and integrity-check the store, resolve the crush
// binary once, build the client with a fresh process-lifetime UUID
// identity, and construct the reconciler, dispatcher, supervisor,
// coordination server, aggregator, and local RPC server.
func New(stateDir string, fleetCfg *config.Fleet, mmCfg *config.Matchmaker) (*Daemon, error) {
	panic(notImplemented)
}

// Run starts all components and blocks until ctx is canceled: startup
// adoption and reconciliation, the periodic reconcile loop, dispatch,
// supervision streams, cancellation monitors, the coordination MCP
// server, and the local RPC listener. A locked or corrupt store crashes
// the daemon loudly (DESIGN §7).
func (d *Daemon) Run(ctx context.Context) error { panic(notImplemented) }

// Handle implements rpc.Handler for CLI/TUI clients: goal submission
// (atomic §5.2 validation then persistence), plan requests (§5.7), status,
// operator approvals (fingerprint and compatibility override), run
// abandonment, instance reset, report export, and shutdown. All methods
// enforce idempotency keys.
func (d *Daemon) Handle(ctx context.Context, req rpc.Request) (rpc.Response, error) {
	panic(notImplemented)
}

// Shutdown performs the drain: stop the RPC listener, drain all instances
// in parallel under the global deadline, close streams and release
// workspace holds, then retire the process-wide client claim as final
// cleanup (DESIGN §5.1). Forced mode cancels active runs; graceful mode
// awaits their terminal states.
func (d *Daemon) Shutdown(ctx context.Context, mode model.DrainMode) error { panic(notImplemented) }
