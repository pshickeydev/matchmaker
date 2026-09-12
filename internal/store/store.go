// Package store is the durable SQLite task store: the source of truth for
// goals, steps, frozen target expansion, runs, notes, cursors, and
// permission decisions (DESIGN §5.2, §5.5, §9.2).
//
// Invariants:
//   - Goal acceptance, target expansion, and initial attempts persist in
//     one transaction before any dispatch (§5.2).
//   - Every state transition validates through the model state machines.
//   - Permission decisions and note-cursor advancement commit in the same
//     transaction as the actions they gate (§5.3, §5.4).
//   - The store is mutable by the invoking OS user and provides neither
//     tamper evidence nor non-repudiation (§5.5).
package store

import (
	"context"

	"github.com/pshickeydev/matchmaker/internal/model"
	_ "modernc.org/sqlite" // pure-Go embedded SQLite driver (DESIGN §9.2)
)

const notImplemented = "not implemented"

// Store wraps the embedded SQLite database.
type Store struct{}

// Open acquires the store file, runs migrations, and performs the
// integrity check required after an unclean shutdown (DESIGN §7: a locked
// or corrupt store crashes the orchestrator loudly). The OS-level
// exclusive lock is acquired by the daemon before calling Open. The store
// file and its parent state directory are created with 0600/0700
// permissions (DESIGN §6).
func Open(ctx context.Context, path string) (*Store, error) {
	panic(notImplemented)
}

// Close releases the database.
func (s *Store) Close() error { panic(notImplemented) }

// IntegrityCheck verifies the database; on restart it runs before
// reconciliation resumes (DESIGN §7).
func (s *Store) IntegrityCheck(ctx context.Context) error { panic(notImplemented) }

// WithinTx runs fn atomically; an error rolls back every change.
func (s *Store) WithinTx(ctx context.Context, fn func(tx *Tx) error) error {
	panic(notImplemented)
}

// IdempotencyLookup returns the recorded response for an idempotency key
// so a client retry cannot duplicate a goal or operator action
// (DESIGN §3). The second return is false for unseen keys.
func (s *Store) IdempotencyLookup(ctx context.Context, key string) ([]byte, bool, error) {
	panic(notImplemented)
}

// RecordIdempotency stores the serialized response under its idempotency
// key in the same transaction as the action it deduplicates.
func (tx *Tx) RecordIdempotency(ctx context.Context, key string, response []byte) error {
	panic(notImplemented)
}

// Tx is the transactional view handed to WithinTx. All mutation methods on
// Tx validate state transitions before writing.
type Tx struct{}

// CreateGoal persists the goal, steps, frozen target expansion, target
// executions, and initial queued attempts in one transaction (§5.2).
// On any validation or write failure nothing is persisted.
func (tx *Tx) CreateGoal(ctx context.Context, goal model.Goal) error { panic(notImplemented) }

// Goal returns one goal with steps and frozen targets.
func (s *Store) Goal(ctx context.Context, goalID string) (model.Goal, error) {
	panic(notImplemented)
}

// ListGoals returns goals by status filter.
func (s *Store) ListGoals(ctx context.Context, status model.GoalStatus) ([]model.Goal, error) {
	panic(notImplemented)
}

// UpdateGoalStatus records the aggregated goal status.
func (tx *Tx) UpdateGoalStatus(ctx context.Context, goalID string, status model.GoalStatus) error {
	panic(notImplemented)
}

// CreatePlanningGoal persists a plan-type goal with its originating request
// metadata (§5.7); a planning run persists no goal state beyond its own
// audit records.
func (tx *Tx) CreatePlanningGoal(ctx context.Context, goal model.Goal, objective string) error {
	panic(notImplemented)
}

// Step returns one step.
func (s *Store) Step(ctx context.Context, stepID string) (model.Step, error) {
	panic(notImplemented)
}

// UpdateStepStatus records the derived step rollup (§5.5).
func (tx *Tx) UpdateStepStatus(ctx context.Context, goalID, stepID string, status model.StepStatus) error {
	panic(notImplemented)
}

// TargetExecution returns one target execution with its attempts.
func (s *Store) TargetExecution(ctx context.Context, id string) (model.TargetExecution, error) {
	panic(notImplemented)
}

// CreateRun appends a new numbered attempt to a target execution. A retry
// is not created until the prior attempt is terminal (§5.3).
func (tx *Tx) CreateRun(ctx context.Context, run model.Run) error { panic(notImplemented) }

// GetRun returns one run.
func (s *Store) GetRun(ctx context.Context, runID string) (model.Run, error) {
	panic(notImplemented)
}

// RunBySession correlates a run by its dedicated session on a workspace
// (used by permission correlation and recovery, §5.3).
func (s *Store) RunBySession(ctx context.Context, workspaceID, sessionID string) (model.Run, error) {
	panic(notImplemented)
}

// UpdateRunStatus transitions a run after RunTransitionAllowed validates
// from -> to; terminal transitions are immutable.
func (tx *Tx) UpdateRunStatus(ctx context.Context, runID string, from, to model.RunStatus) error {
	panic(notImplemented)
}

// SetRunIdentifiers persists the dedicated session ID, caller-supplied
// RunID, rendered-prompt hash, and dispatching state before the prompt is
// submitted (§5.3).
func (tx *Tx) SetRunIdentifiers(ctx context.Context, runID string, sessionID, crushRunID, promptHash string) error {
	panic(notImplemented)
}

// SetRunCancel records cause and request time atomically with the
// cancelling transition so recovery does not re-issue the cancel (§5.3).
func (tx *Tx) SetRunCancel(ctx context.Context, runID string, cause model.CancelCause) error {
	panic(notImplemented)
}

// ActiveRuns lists nonterminal runs; used by startup adoption and the
// dispatch gate (§5.1, §5.2).
func (s *Store) ActiveRuns(ctx context.Context) ([]model.Run, error) { panic(notImplemented) }

// LatestTerminalAttempt returns the highest-numbered terminal attempt of a
// target execution; it supplies the target's current status and result
// reference; earlier attempts are audit-only (§5.3).
func (s *Store) LatestTerminalAttempt(ctx context.Context, targetExecutionID string) (model.Run, error) {
	panic(notImplemented)
}

// AppendNote stores an accepted note and assigns its store-generated
// monotonically increasing note_id. Audience expansion is frozen here
// (§5.4).
func (tx *Tx) AppendNote(ctx context.Context, note model.Note) (int64, error) {
	panic(notImplemented)
}

// NotesFor returns notes addressed to project with note_id greater than
// since, ascending, up to the page limits. Reads never advance the durable
// delivery cursor (§5.4).
func (s *Store) NotesFor(ctx context.Context, goalID, project string, since int64) ([]model.Note, error) {
	panic(notImplemented)
}

// NoteCursor returns the durable high-water note_id for a (goal, instance)
// audience (§5.4).
func (s *Store) NoteCursor(ctx context.Context, goalID, project string) (int64, error) {
	panic(notImplemented)
}

// AdvanceNoteCursor moves the delivery cursor forward; it commits in the
// same transaction as accepted prompt submission (§5.4).
func (tx *Tx) AdvanceNoteCursor(ctx context.Context, goalID, project string, noteID int64) error {
	panic(notImplemented)
}

// RecordPermissionDecision transactionally records workspace ID, session
// ID, run ID, instance generation, request ID, request type, policy, and
// decision before Matchmaker responds. If recording fails, supervision
// sends deny; never grant an unaudited request (§5.3, §6).
func (tx *Tx) RecordPermissionDecision(ctx context.Context, d model.PermissionDecision) error {
	panic(notImplemented)
}

// PermissionDecisionByRequest returns the recorded decision for a
// duplicate request ID; duplicates reuse the recorded decision (§5.3).
func (s *Store) PermissionDecisionByRequest(ctx context.Context, requestID string) (model.PermissionDecision, bool, error) {
	panic(notImplemented)
}

// RecordQuestionEvent persists bounded question metadata (never raw text
// or choices) atomically with issuing the cancel (§5.3).
func (tx *Tx) RecordQuestionEvent(ctx context.Context, goalID, runID, questionID string) error {
	panic(notImplemented)
}

// ApprovedFingerprint returns the operator-approved config fingerprint of
// a project (§5.1).
func (s *Store) ApprovedFingerprint(ctx context.Context, project string) (model.Fingerprint, bool, error) {
	panic(notImplemented)
}

// SetApprovedFingerprint records an explicit operator approval of a new
// fingerprint (§5.1).
func (tx *Tx) SetApprovedFingerprint(ctx context.Context, f model.Fingerprint) error {
	panic(notImplemented)
}

// RecordCompatibilityOverride records an explicit operator compatibility
// override for an observed Crush version and build_id (§9.3). Overrides
// are deployment-specific and audited.
func (tx *Tx) RecordCompatibilityOverride(ctx context.Context, version, buildID, approvedBy string) error {
	panic(notImplemented)
}

// CompatibilityOverride returns any recorded override for a version/build
// pair.
func (s *Store) CompatibilityOverride(ctx context.Context, version, buildID string) (bool, error) {
	panic(notImplemented)
}

// InstanceState persists the current instance state and generation.
func (tx *Tx) SetInstanceState(ctx context.Context, project string, state model.InstanceState, generation int64) error {
	panic(notImplemented)
}

// Instance loads the persisted instance record.
func (s *Store) Instance(ctx context.Context, project string) (model.Instance, error) {
	panic(notImplemented)
}

// Prune removes Matchmaker metadata according to operator policy; Crush
// owns session-log retention (§5.5).
func (s *Store) Prune(ctx context.Context, policy PrunePolicy) error { panic(notImplemented) }

// PrunePolicy selects what the prune/gc subcommand removes.
type PrunePolicy struct {
	RetainDays   int
	KeepLast     int
	IncludeAudit bool
}

// CountNotes returns the note count for limit enforcement before writes
// (§5.4).
func (s *Store) CountNotes(ctx context.Context, goalID string) (int, error) { panic(notImplemented) }
