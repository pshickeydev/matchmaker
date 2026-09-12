// Package aggregate derives step and goal rollups from terminal run
// attempts and produces per-goal reports (DESIGN §5.5).
package aggregate

import (
	"context"
	"io"

	"github.com/pshickeydev/matchmaker/internal/model"
	"github.com/pshickeydev/matchmaker/internal/store"
)

const notImplemented = "not implemented"

// SessionSource is the lazily-resolved Crush session content seam used by
// reports and exports (DESIGN §5.5). The crushapi-based implementation is
// injected by the daemon; aggregate never touches the wire layer directly
// (AGENTS.md layering rule).
type SessionSource interface {
	// FinalOutput returns the final output bytes of a run's session.
	// available is false with nil error when the referenced session data
	// has been pruned (explicit source-data-unavailable status, §5.5).
	FinalOutput(ctx context.Context, run model.Run) (output []byte, available bool, err error)
}

// Aggregator computes rollups and reports from the durable store.
type Aggregator struct{}

// New builds the aggregator over the store and the session content source.
func New(st *store.Store, sessions SessionSource) *Aggregator { panic(notImplemented) }

// StepStatus derives a step's status from its target executions
// (DESIGN §5.5): every target succeeded -> succeeded; every target failed
// -> failed; mixed terminal outcomes -> partial. Failed, cancelled,
// timed-out, skipped, and abandoned attempts count as failed for the
// rollup while retaining detailed status; unknown is nonterminal and
// prevents aggregation. Steps wait until every frozen target is terminal
// and no retry remains eligible.
func (a *Aggregator) StepStatus(ctx context.Context, goalID, stepID string) (model.StepStatus, bool, error) {
	panic(notImplemented)
}

// GoalStatus derives the goal rollup: succeeded when all steps succeeded,
// failed when no step succeeded, partial otherwise including skipped
// branches (DESIGN §5.5).
func (a *Aggregator) GoalStatus(ctx context.Context, goalID string) (model.GoalStatus, error) {
	panic(notImplemented)
}

// Wake runs the aggregate pass for one goal: recompute step and goal
// statuses, persist derived statuses, and finalize when every step is
// terminal. Plan goals produce no per-goal report (DESIGN §5.7).
func (a *Aggregator) Wake(ctx context.Context, goalID string) error { panic(notImplemented) }

// Report is the per-goal report assembled from Matchmaker metadata plus
// lazily resolved Crush session content. It preserves detailed step,
// target, and attempt statuses.
type Report struct {
	GoalID string
	Status model.GoalStatus
	Steps  []StepReport
}

// StepReport is one step's report entry.
type StepReport struct {
	StepID  string
	Status  model.StepStatus
	Targets []TargetReport
}

// TargetReport is one target execution's report entry, derived from its
// latest terminal attempt.
type TargetReport struct {
	Project  string
	Status   model.RunStatus
	Attempts int
	Excerpt  string
}

// BuildReport assembles the in-memory report for one goal.
func (a *Aggregator) BuildReport(ctx context.Context, goalID string) (Report, error) {
	panic(notImplemented)
}

// Export streams the report to an operator-selected file only on explicit
// request (DESIGN §5.5): it lazily resolves Crush session content
// directly to the writer without inserting it into the store, writes with
// operator-only permissions, never previews raw content, and reports an
// explicit source-data-unavailable status when referenced session data
// has been pruned. Exported files are independent artifacts outside store
// retention and may contain terminal escapes and workspace-derived
// secrets.
func (a *Aggregator) Export(ctx context.Context, goalID string, w io.Writer) error {
	panic(notImplemented)
}
