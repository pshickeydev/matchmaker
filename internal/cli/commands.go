// Package cli defines the matchmaker command surface (DESIGN §10.1):
// goal planning and submission, fleet management, report viewing, plus
// daemon control. Commands are short-lived RPC clients of the daemon.
package cli

import "github.com/spf13/cobra"

const notImplemented = "not implemented"

// Execute runs the root command with args, returning its error.
func Execute(args []string) error {
	panic(notImplemented)
}

// NewRootCmd builds the command tree.
func NewRootCmd() *cobra.Command { panic(notImplemented) }

// newDaemonCmd runs the long-lived daemon in the foreground: acquire the
// state-dir lock, open the store, start reconciliation, dispatch,
// supervision, the coordination MCP server, and the local RPC socket.
func newDaemonCmd() *cobra.Command { panic(notImplemented) }

// newGoalCmd groups: submit, plan, status, report, abandon.
func newGoalCmd() *cobra.Command { panic(notImplemented) }

// newGoalSubmitCmd submits a goal file through the daemon; the daemon
// validates atomically (§5.2) and rejects with all validation errors,
// persisting nothing on any error.
func newGoalSubmitCmd() *cobra.Command { panic(notImplemented) }

// newGoalPlanCmd implements `matchmaker goal plan "<objective>" --on
// <project>` (§5.7): sends the plan request, waits for the planning run,
// returns the draft for operator review rendered through the terminal
// renderer, and writes it to an operator-selected path with operator-only
// permissions. --auto-submit submits a valid draft immediately but never
// one whose steps use grant_all.
func newGoalPlanCmd() *cobra.Command { panic(notImplemented) }

// newGoalStatusCmd shows live goal, step, target, and run status.
func newGoalStatusCmd() *cobra.Command { panic(notImplemented) }

// newGoalReportCmd builds and optionally exports the per-goal report
// (§5.5); export streams lazily resolved Crush content to an
// operator-selected file and never inserts it into the store.
func newGoalReportCmd() *cobra.Command { panic(notImplemented) }

// newGoalAbandonCmd performs the explicit operator abandonment of an
// unresolved run, accepting the outcome and releasing serialization.
func newGoalAbandonCmd() *cobra.Command { panic(notImplemented) }

// newFleetCmd groups: list, approve, override-version, reset.
func newFleetCmd() *cobra.Command { panic(notImplemented) }

// newFleetListCmd shows instance states, generations, and stream health.
func newFleetListCmd() *cobra.Command { panic(notImplemented) }

// newFleetApproveCmd records explicit operator approval of a changed
// project config fingerprint, resuming reconciliation (§5.1).
func newFleetApproveCmd() *cobra.Command { panic(notImplemented) }

// newFleetOverrideCmd records an explicit compatibility override for an
// observed Crush version and build_id (§9.3).
func newFleetOverrideCmd() *cobra.Command { panic(notImplemented) }

// newFleetResetCmd moves a failed instance back to stopped for explicit
// operator reset (§4.2).
func newFleetResetCmd() *cobra.Command { panic(notImplemented) }

// newOnboardCmd shows the generated crushrc fragment and, after explicit
// approval, installs the Matchmaker-owned registration block (§5.4).
// Projects that opt out lose live tools but still receive injected notes.
func newOnboardCmd() *cobra.Command { panic(notImplemented) }

// newPruneCmd removes Matchmaker metadata per operator policy; Crush owns
// session-log retention (§5.5).
func newPruneCmd() *cobra.Command { panic(notImplemented) }

// newShutdownCmd requests a clean daemon shutdown through the RPC:
// parallel instance drain under the global deadline, then client-claim
// retirement.
func newShutdownCmd() *cobra.Command { panic(notImplemented) }

// newTUICmd runs the bubbletea dashboard: a live view of goals, runs,
// and notes (§10.1).
func newTUICmd() *cobra.Command { panic(notImplemented) }
