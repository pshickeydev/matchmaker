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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pshickeydev/matchmaker/internal/model"
	_ "modernc.org/sqlite" // pure-Go embedded SQLite driver (DESIGN §9.2)
)

const (
	// storeFileMode and stateDirMode are the operator-only permissions of
	// DESIGN §6.
	storeFileMode os.FileMode = 0o600
	stateDirMode  os.FileMode = 0o700
	// busyTimeout keeps concurrent daemon-side writers from failing fast.
	busyTimeout = 5 * time.Second
)

// migrations are versioned, forward-only schema steps applied under the
// daemon lock (plan M2). Index n in this slice is schema version n+1.
var migrations = []string{schemaV1}

const schemaV1 = `
CREATE TABLE goals (
	id                TEXT PRIMARY KEY,
	type              TEXT NOT NULL,
	objective         TEXT NOT NULL DEFAULT '',
	status            TEXT NOT NULL,
	fleet_snapshot_id TEXT NOT NULL DEFAULT '',
	planning_run_id   TEXT NOT NULL DEFAULT '',
	created_at        TEXT NOT NULL,
	finalized_at      TEXT
);
CREATE TABLE steps (
	id                   TEXT NOT NULL,
	goal_id              TEXT NOT NULL,
	prompt_template      TEXT NOT NULL,
	target_spec          TEXT NOT NULL,
	needs                TEXT NOT NULL DEFAULT '[]',
	accept_partial_needs INTEGER NOT NULL DEFAULT 0,
	supervision          TEXT NOT NULL DEFAULT 'deny',
	timeout_ns           INTEGER NOT NULL DEFAULT 0,
	retries              INTEGER NOT NULL DEFAULT 0,
	status               TEXT NOT NULL DEFAULT 'pending',
	PRIMARY KEY (goal_id, id)
);
CREATE INDEX steps_by_id ON steps (id);
CREATE TABLE frozen_targets (
	goal_id   TEXT NOT NULL,
	step_id   TEXT NOT NULL,
	project   TEXT NOT NULL,
	instance  TEXT NOT NULL,
	server_url TEXT NOT NULL,
	workspace TEXT NOT NULL DEFAULT '',
	tags      TEXT NOT NULL DEFAULT '[]',
	execution_id TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (goal_id, step_id, project)
);
CREATE TABLE target_executions (
	id      TEXT PRIMARY KEY,
	goal_id TEXT NOT NULL,
	step_id TEXT NOT NULL,
	project TEXT NOT NULL
);
CREATE INDEX executions_by_step ON target_executions (goal_id, step_id);
CREATE TABLE runs (
	id                  TEXT PRIMARY KEY,
	goal_id             TEXT NOT NULL,
	step_id             TEXT NOT NULL,
	target_execution_id TEXT NOT NULL,
	attempt             INTEGER NOT NULL,
	project             TEXT NOT NULL,
	server_url          TEXT NOT NULL DEFAULT '',
	workspace_id        TEXT NOT NULL DEFAULT '',
	instance_generation INTEGER NOT NULL DEFAULT 0,
	crush_run_id        TEXT NOT NULL DEFAULT '',
	session_id          TEXT NOT NULL DEFAULT '',
	rendered_prompt_hash TEXT NOT NULL DEFAULT '',
	status              TEXT NOT NULL,
	cancel_cause        TEXT NOT NULL DEFAULT '',
	notes_high_water    INTEGER NOT NULL DEFAULT 0,
	cancel_requested_at TEXT,
	created_at          TEXT NOT NULL,
	finalized_at        TEXT,
	UNIQUE (target_execution_id, attempt)
);
CREATE INDEX runs_by_session ON runs (workspace_id, session_id) WHERE session_id != '';
CREATE INDEX runs_by_status ON runs (status);
CREATE INDEX runs_by_execution ON runs (target_execution_id);
CREATE TABLE notes (
	note_id     INTEGER PRIMARY KEY AUTOINCREMENT,
	goal_id     TEXT NOT NULL,
	from_project TEXT NOT NULL,
	to_project  TEXT NOT NULL DEFAULT '',
	to_tag      TEXT NOT NULL DEFAULT '',
	to_all      INTEGER NOT NULL DEFAULT 0,
	body        TEXT NOT NULL,
	created_at  TEXT NOT NULL
);
CREATE INDEX notes_by_goal ON notes (goal_id, note_id);
CREATE TABLE note_audience (
	note_id INTEGER NOT NULL,
	goal_id TEXT NOT NULL,
	project TEXT NOT NULL,
	PRIMARY KEY (note_id, project)
);
CREATE INDEX audience_by_project ON note_audience (goal_id, project, note_id);
CREATE TABLE note_cursors (
	goal_id TEXT NOT NULL,
	project TEXT NOT NULL,
	note_id INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (goal_id, project)
);
CREATE TABLE permission_decisions (
	id                  TEXT PRIMARY KEY,
	goal_id             TEXT NOT NULL DEFAULT '',
	run_id              TEXT NOT NULL DEFAULT '',
	project             TEXT NOT NULL DEFAULT '',
	workspace_id        TEXT NOT NULL DEFAULT '',
	session_id          TEXT NOT NULL DEFAULT '',
	instance_generation INTEGER NOT NULL DEFAULT 0,
	request_id          TEXT NOT NULL DEFAULT '',
	request_type        TEXT NOT NULL DEFAULT '',
	policy              TEXT NOT NULL DEFAULT '',
	decision            TEXT NOT NULL,
	correlation_failed  INTEGER NOT NULL DEFAULT 0,
	decided_at          TEXT NOT NULL
);
CREATE UNIQUE INDEX decisions_by_request
	ON permission_decisions (request_id) WHERE request_id != '';
CREATE TABLE question_events (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	goal_id     TEXT NOT NULL DEFAULT '',
	run_id      TEXT NOT NULL DEFAULT '',
	question_id TEXT NOT NULL,
	created_at  TEXT NOT NULL
);
CREATE TABLE fingerprints (
	project     TEXT PRIMARY KEY,
	approved_by TEXT NOT NULL,
	approved_at TEXT NOT NULL
);
CREATE TABLE fingerprint_entries (
	project TEXT NOT NULL,
	path    TEXT NOT NULL,
	digest  TEXT NOT NULL,
	PRIMARY KEY (project, path)
);
CREATE TABLE compatibility_overrides (
	version     TEXT NOT NULL,
	build_id    TEXT NOT NULL,
	approved_by TEXT NOT NULL,
	approved_at TEXT NOT NULL,
	PRIMARY KEY (version, build_id)
);
CREATE TABLE instances (
	project        TEXT PRIMARY KEY,
	state          TEXT NOT NULL,
	generation     INTEGER NOT NULL DEFAULT 0,
	port           INTEGER NOT NULL DEFAULT 0,
	server_url     TEXT NOT NULL DEFAULT '',
	workspace_id   TEXT NOT NULL DEFAULT '',
	failed_reason  TEXT NOT NULL DEFAULT ''
);
CREATE TABLE idempotency (
	key        TEXT PRIMARY KEY,
	response   BLOB NOT NULL,
	created_at TEXT NOT NULL
);
`

// ID prefixes keep store-generated identifiers recognizable in logs and
// operator output.
const (
	goalPrefix      = "g"
	executionPrefix = "te"
	runPrefix       = "r"
	decisionPrefix  = "pd"
	snapPrefix      = "snap"
)

// Store wraps the embedded SQLite database.
type Store struct {
	db *sql.DB
}

// Open acquires the store file, runs migrations, and performs the
// integrity check required after an unclean shutdown (DESIGN §7: a locked
// or corrupt store crashes the orchestrator loudly). The OS-level
// exclusive lock is acquired by the daemon before calling Open. The store
// file and its parent state directory are created with 0600/0700
// permissions (DESIGN §6).
func Open(ctx context.Context, path string) (*Store, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, stateDirMode); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	if err := os.Chmod(dir, stateDirMode); err != nil {
		return nil, fmt.Errorf("secure state dir: %w", err)
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)",
		path, busyTimeout.Milliseconds())
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	st := &Store{db: db}
	if err := st.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate store: %w", err)
	}
	// The database file is created lazily by the first connection, so the
	// permission is enforced once migrations have materialized it.
	if err := os.Chmod(path, storeFileMode); err != nil {
		db.Close()
		return nil, fmt.Errorf("secure store file: %w", err)
	}
	if err := st.IntegrityCheck(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store integrity check: %w", err)
	}
	return st, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// IntegrityCheck verifies the database; on restart it runs before
// reconciliation resumes (DESIGN §7).
func (s *Store) IntegrityCheck(ctx context.Context) error {
	var result string
	err := s.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result)
	if err != nil {
		return err
	}
	if !strings.EqualFold(result, "ok") {
		return fmt.Errorf("quick_check reported %q", result)
	}
	return nil
}

// migrate applies every not-yet-applied migration in order, each inside
// its own transaction.
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL
		)`); err != nil {
		return err
	}
	for i, migration := range migrations {
		version := i + 1
		var applied int
		if err := s.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM schema_migrations WHERE version = ?", version).Scan(&applied); err != nil {
			return err
		}
		if applied > 0 {
			continue
		}
		if err := s.WithinTx(ctx, func(tx *Tx) error {
			if _, err := tx.db.ExecContext(ctx, migration); err != nil {
				return err
			}
			_, err := tx.db.ExecContext(ctx,
				"INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)",
				version, nowISO())
			return err
		}); err != nil {
			return fmt.Errorf("apply migration %d: %w", version, err)
		}
	}
	return nil
}

// WithinTx runs fn atomically; an error rolls back every change.
func (s *Store) WithinTx(ctx context.Context, fn func(tx *Tx) error) error {
	dbTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(&Tx{db: dbTx}); err != nil {
		dbTx.Rollback()
		return err
	}
	return dbTx.Commit()
}

// IdempotencyLookup returns the recorded response for an idempotency key
// so a client retry cannot duplicate a goal or operator action
// (DESIGN §3). The second return is false for unseen keys.
func (s *Store) IdempotencyLookup(ctx context.Context, key string) ([]byte, bool, error) {
	if key == "" {
		return nil, false, nil
	}
	var response []byte
	err := s.db.QueryRowContext(ctx,
		"SELECT response FROM idempotency WHERE key = ?", key).Scan(&response)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return response, true, nil
}

// RecordIdempotency stores the serialized response under its idempotency
// key in the same transaction as the action it deduplicates.
func (tx *Tx) RecordIdempotency(ctx context.Context, key string, response []byte) error {
	if key == "" {
		return nil
	}
	_, err := tx.db.ExecContext(ctx,
		"INSERT OR IGNORE INTO idempotency (key, response, created_at) VALUES (?, ?, ?)",
		key, response, nowISO())
	return err
}

// Tx is the transactional view handed to WithinTx. All mutation methods on
// Tx validate state transitions before writing.
type Tx struct{ db *sql.Tx }

// CreateGoal persists the goal, steps, frozen target expansion, target
// executions, and initial queued attempts in one transaction (§5.2).
// On any validation or write failure nothing is persisted.
func (tx *Tx) CreateGoal(ctx context.Context, goal model.Goal) error {
	return tx.createGoal(ctx, goal, "")
}

// CreatePlanningGoal persists a plan-type goal with its originating request
// metadata (§5.7); a planning run persists no goal state beyond its own
// audit records.
func (tx *Tx) CreatePlanningGoal(ctx context.Context, goal model.Goal, objective string) error {
	if goal.Type != model.GoalTypePlan {
		return fmt.Errorf("planning goal creation requires type %q", model.GoalTypePlan)
	}
	return tx.createGoal(ctx, goal, objective)
}

// createGoal is the shared acceptance path for work and plan goals. The
// planning objective overrides the draft's objective field for plan-type
// goals (§5.7); work goals persist their own objective.
func (tx *Tx) createGoal(ctx context.Context, goal model.Goal, objective string) error {
	if objective == "" {
		objective = goal.Objective
	}
	if goal.ID == "" {
		goal.ID = newID(goalPrefix)
	}
	if goal.Status == "" {
		goal.Status = model.GoalActive
	}
	if goal.FleetSnapshotID == "" {
		goal.FleetSnapshotID = newID(snapPrefix)
	}
	if goal.Type == "" {
		goal.Type = model.GoalTypeWork
	}
	if len(goal.Steps) == 0 {
		return errors.New("goal has no steps")
	}
	if _, err := tx.db.ExecContext(ctx, `
		INSERT INTO goals (id, type, objective, status, fleet_snapshot_id, planning_run_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		goal.ID, goal.Type, objective, goal.Status, goal.FleetSnapshotID,
		goal.PlanningRunID, nowISO()); err != nil {
		return err
	}
	stepIDs := make(map[string]bool, len(goal.Steps))
	for _, step := range goal.Steps {
		if stepIDs[step.ID] {
			return fmt.Errorf("duplicate step id %q", step.ID)
		}
		stepIDs[step.ID] = true
		if err := tx.insertStep(ctx, goal.ID, step); err != nil {
			return err
		}
	}
	for stepID, targets := range goal.FrozenTargets {
		if !stepIDs[stepID] {
			return fmt.Errorf("frozen targets reference unknown step %q", stepID)
		}
		if len(targets) == 0 {
			return fmt.Errorf("step %q has no frozen targets", stepID)
		}
		for _, target := range targets {
			if err := tx.insertExecution(ctx, goal, stepID, target); err != nil {
				return err
			}
		}
	}
	for _, step := range goal.Steps {
		if _, ok := goal.FrozenTargets[step.ID]; !ok {
			return fmt.Errorf("step %q missing frozen target expansion", step.ID)
		}
	}
	return nil
}

// insertStep persists one step of the DAG.
func (tx *Tx) insertStep(ctx context.Context, goalID string, step model.Step) error {
	targetSpec, err := json.Marshal(step.Target)
	if err != nil {
		return err
	}
	needs, err := json.Marshal(step.Needs)
	if err != nil {
		return err
	}
	if step.Supervision == "" {
		step.Supervision = model.SupervisionDeny
	}
	_, err = tx.db.ExecContext(ctx, `
		INSERT INTO steps (id, goal_id, prompt_template, target_spec, needs,
			accept_partial_needs, supervision, timeout_ns, retries)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		step.ID, goalID, step.PromptTemplate, targetSpec, needs,
		boolToInt(step.AcceptPartialNeeds), step.Supervision,
		step.Timeout.Nanoseconds(), step.Retries)
	return err
}

// insertExecution persists one frozen target plus its target execution
// and initial queued attempt.
func (tx *Tx) insertExecution(ctx context.Context, goal model.Goal, stepID string, target model.ResolvedTarget) error {
	tags, err := json.Marshal(target.Tags)
	if err != nil {
		return err
	}
	executionID := newID(executionPrefix)
	if _, err = tx.db.ExecContext(ctx, `
		INSERT INTO frozen_targets (goal_id, step_id, project, instance, server_url, workspace, tags, execution_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		goal.ID, stepID, target.Project, target.Instance, target.ServerURL, target.Workspace, tags, executionID); err != nil {
		return err
	}
	if _, err := tx.db.ExecContext(ctx, `
		INSERT INTO target_executions (id, goal_id, step_id, project)
		VALUES (?, ?, ?, ?)`,
		executionID, goal.ID, stepID, target.Project); err != nil {
		return err
	}
	_, err = tx.db.ExecContext(ctx, `
		INSERT INTO runs (id, goal_id, step_id, target_execution_id, attempt, project,
			server_url, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		newID(runPrefix), goal.ID, stepID, executionID, 1, target.Project,
		target.ServerURL, model.RunQueued, nowISO())
	return err
}

// Goal returns one goal with steps and frozen targets.
func (s *Store) Goal(ctx context.Context, goalID string) (model.Goal, error) {
	var goal model.Goal
	var createdAt string
	var finalizedAt sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT id, type, objective, status, fleet_snapshot_id, planning_run_id, created_at, finalized_at
		FROM goals WHERE id = ?`, goalID).
		Scan(&goal.ID, &goal.Type, &goal.Objective, &goal.Status, &goal.FleetSnapshotID,
			&goal.PlanningRunID, &createdAt, &finalizedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Goal{}, fmt.Errorf("goal %q not found", goalID)
	}
	if err != nil {
		return model.Goal{}, err
	}
	goal.CreatedAt = parseISO(createdAt)
	if finalizedAt.Valid {
		t := parseISO(finalizedAt.String)
		goal.FinalizedAt = &t
	}
	goal.Steps, err = s.stepsFor(ctx, goalID)
	if err != nil {
		return model.Goal{}, err
	}
	goal.FrozenTargets, err = s.frozenTargetsFor(ctx, goalID)
	if err != nil {
		return model.Goal{}, err
	}
	return goal, nil
}

// ListGoals returns goals by status filter.
func (s *Store) ListGoals(ctx context.Context, status model.GoalStatus) ([]model.Goal, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, type, objective, status, fleet_snapshot_id, planning_run_id, created_at
		FROM goals WHERE status = ? ORDER BY created_at, id`, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var goals []model.Goal
	for rows.Next() {
		var goal model.Goal
		var createdAt string
		if err := rows.Scan(&goal.ID, &goal.Type, &goal.Objective, &goal.Status,
			&goal.FleetSnapshotID, &goal.PlanningRunID, &createdAt); err != nil {
			return nil, err
		}
		goal.CreatedAt = parseISO(createdAt)
		goals = append(goals, goal)
	}
	return goals, rows.Err()
}

// stepsFor loads a goal's steps ordered by id.
func (s *Store) stepsFor(ctx context.Context, goalID string) ([]model.Step, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, goal_id, prompt_template, target_spec, needs, accept_partial_needs,
			supervision, timeout_ns, retries
		FROM steps WHERE goal_id = ? ORDER BY id`, goalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var steps []model.Step
	for rows.Next() {
		step, err := scanStep(rows)
		if err != nil {
			return nil, err
		}
		step.GoalID = goalID
		steps = append(steps, step)
	}
	return steps, rows.Err()
}

// frozenTargetsFor loads the goal's frozen expansion map.
func (s *Store) frozenTargetsFor(ctx context.Context, goalID string) (map[string][]model.ResolvedTarget, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT step_id, project, instance, server_url, workspace, tags, execution_id
		FROM frozen_targets WHERE goal_id = ? ORDER BY step_id, project`, goalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	targets := make(map[string][]model.ResolvedTarget)
	for rows.Next() {
		var stepID string
		var target model.ResolvedTarget
		var tags string
		if err := rows.Scan(&stepID, &target.Project, &target.Instance,
			&target.ServerURL, &target.Workspace, &tags, &target.ExecutionID); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(tags), &target.Tags); err != nil {
			return nil, fmt.Errorf("frozen target tags: %w", err)
		}
		target.StepID = stepID
		targets[stepID] = append(targets[stepID], target)
	}
	return targets, rows.Err()
}

// UpdateGoalStatus records the aggregated goal status.
func (tx *Tx) UpdateGoalStatus(ctx context.Context, goalID string, status model.GoalStatus) error {
	finalizedAt := any(nil)
	if status != model.GoalActive {
		finalizedAt = nowISO()
	}
	_, err := tx.db.ExecContext(ctx,
		"UPDATE goals SET status = ?, finalized_at = ? WHERE id = ?",
		status, finalizedAt, goalID)
	return err
}

// Step returns one step. Step IDs are unique within a goal (DESIGN
// §5.2); when the same ID was reused across goals the lookup is
// ambiguous and callers must load the owning goal instead.
func (s *Store) Step(ctx context.Context, stepID string) (model.Step, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, goal_id, prompt_template, target_spec, needs, accept_partial_needs,
			supervision, timeout_ns, retries
		FROM steps WHERE id = ? ORDER BY goal_id`, stepID)
	if err != nil {
		return model.Step{}, err
	}
	defer rows.Close()
	var steps []model.Step
	for rows.Next() {
		step, err := scanStep(rows)
		if err != nil {
			return model.Step{}, err
		}
		steps = append(steps, step)
	}
	if err := rows.Err(); err != nil {
		return model.Step{}, err
	}
	switch len(steps) {
	case 0:
		return model.Step{}, fmt.Errorf("step %q not found", stepID)
	case 1:
		return steps[0], nil
	default:
		return model.Step{}, fmt.Errorf("step id %q is ambiguous across goals", stepID)
	}
}

// scanStep decodes one step row.
func scanStep(row interface{ Scan(...any) error }) (model.Step, error) {
	var step model.Step
	var targetSpec, needs string
	var acceptPartial int
	var timeoutNS int64
	err := row.Scan(&step.ID, &step.GoalID, &step.PromptTemplate, &targetSpec, &needs,
		&acceptPartial, &step.Supervision, &timeoutNS, &step.Retries)
	if err != nil {
		return model.Step{}, err
	}
	step.AcceptPartialNeeds = acceptPartial != 0
	step.Timeout = time.Duration(timeoutNS)
	if err := json.Unmarshal([]byte(targetSpec), &step.Target); err != nil {
		return model.Step{}, err
	}
	if err := json.Unmarshal([]byte(needs), &step.Needs); err != nil {
		return model.Step{}, err
	}
	return step, nil
}

// UpdateStepStatus records the derived step rollup (§5.5).
func (tx *Tx) UpdateStepStatus(ctx context.Context, goalID, stepID string, status model.StepStatus) error {
	_, err := tx.db.ExecContext(ctx,
		"UPDATE steps SET status = ? WHERE id = ? AND goal_id = ?",
		status, stepID, goalID)
	return err
}

// TargetExecution returns one target execution with its attempts.
func (s *Store) TargetExecution(ctx context.Context, id string) (model.TargetExecution, error) {
	var exec model.TargetExecution
	err := s.db.QueryRowContext(ctx, `
		SELECT id, goal_id, step_id, project FROM target_executions WHERE id = ?`, id).
		Scan(&exec.ID, &exec.GoalID, &exec.StepID, &exec.Project)
	if errors.Is(err, sql.ErrNoRows) {
		return model.TargetExecution{}, fmt.Errorf("target execution %q not found", id)
	}
	if err != nil {
		return model.TargetExecution{}, err
	}
	exec.Attempts, err = s.attemptsFor(ctx, id)
	return exec, err
}

// attemptsFor loads one execution's attempts in attempt order.
func (s *Store) attemptsFor(ctx context.Context, executionID string) ([]model.Run, error) {
	rows, err := s.db.QueryContext(ctx, selectRunSQL+" WHERE r.target_execution_id = ? ORDER BY r.attempt", executionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRuns(rows)
}

// selectRunSQL is the shared run column list.
const selectRunSQL = `
	SELECT r.id, r.goal_id, r.step_id, r.target_execution_id, r.attempt, r.project,
		r.server_url, r.workspace_id, r.instance_generation, r.crush_run_id, r.session_id,
		r.rendered_prompt_hash, r.status, r.cancel_cause, r.notes_high_water,
		r.cancel_requested_at, r.created_at, r.finalized_at
	FROM runs r`

// scanRuns decodes run rows.
func scanRuns(rows *sql.Rows) ([]model.Run, error) {
	var runs []model.Run
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// scanRun decodes one run row.
func scanRun(row interface{ Scan(...any) error }) (model.Run, error) {
	var run model.Run
	var cancelCause string
	var cancelRequestedAt, finalizedAt sql.NullString
	var createdAt string
	err := row.Scan(&run.ID, &run.GoalID, &run.StepID, &run.TargetExecutionID, &run.Attempt,
		&run.Project, &run.ServerURL, &run.WorkspaceID, &run.InstanceGeneration,
		&run.CrushRunID, &run.SessionID, &run.RenderedPromptHash, &run.Status,
		&cancelCause, &run.NotesHighWater, &cancelRequestedAt, &createdAt, &finalizedAt)
	if err != nil {
		return model.Run{}, err
	}
	run.CancelCause = model.CancelCause(cancelCause)
	run.CreatedAt = parseISO(createdAt)
	if cancelRequestedAt.Valid {
		t := parseISO(cancelRequestedAt.String)
		run.CancelRequestedAt = &t
	}
	if finalizedAt.Valid {
		t := parseISO(finalizedAt.String)
		run.FinalizedAt = &t
	}
	return run, nil
}

// CreateRun appends a new numbered attempt to a target execution. A retry
// is not created until the prior attempt is terminal (§5.3).
func (tx *Tx) CreateRun(ctx context.Context, run model.Run) error {
	var priorStatus string
	err := tx.db.QueryRowContext(ctx,
		"SELECT status FROM runs WHERE target_execution_id = ? ORDER BY attempt DESC LIMIT 1",
		run.TargetExecutionID).Scan(&priorStatus)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("target execution %q not found", run.TargetExecutionID)
	case err != nil:
		return err
	case !model.RunIsTerminal(model.RunStatus(priorStatus)):
		return fmt.Errorf("cannot append attempt: prior attempt is %s (nonterminal)", priorStatus)
	}
	if run.ID == "" {
		run.ID = newID(runPrefix)
	}
	run.Attempt = nextAttempt(tx, ctx, run.TargetExecutionID)
	_, err = tx.db.ExecContext(ctx, `
		INSERT INTO runs (id, goal_id, step_id, target_execution_id, attempt, project,
			server_url, workspace_id, instance_generation, status, notes_high_water, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.GoalID, run.StepID, run.TargetExecutionID, run.Attempt, run.Project,
		run.ServerURL, run.WorkspaceID, run.InstanceGeneration, run.Status,
		run.NotesHighWater, nowISO())
	return err
}

// nextAttempt computes the next monotonically numbered attempt.
func nextAttempt(tx *Tx, ctx context.Context, executionID string) int {
	var highest int
	if err := tx.db.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(attempt), 0) FROM runs WHERE target_execution_id = ?",
		executionID).Scan(&highest); err != nil {
		return 1
	}
	return highest + 1
}

// GetRun returns one run.
func (s *Store) GetRun(ctx context.Context, runID string) (model.Run, error) {
	row := s.db.QueryRowContext(ctx, selectRunSQL+" WHERE r.id = ?", runID)
	run, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Run{}, fmt.Errorf("run %q not found", runID)
	}
	return run, err
}

// RunBySession correlates a run by its dedicated session on a workspace
// (used by permission correlation and recovery, §5.3).
func (s *Store) RunBySession(ctx context.Context, workspaceID, sessionID string) (model.Run, error) {
	if workspaceID == "" || sessionID == "" {
		return model.Run{}, fmt.Errorf("run correlation requires workspace and session IDs")
	}
	row := s.db.QueryRowContext(ctx,
		selectRunSQL+" WHERE r.workspace_id = ? AND r.session_id = ?", workspaceID, sessionID)
	run, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Run{}, fmt.Errorf("no run for workspace %s session %s", workspaceID, sessionID)
	}
	return run, err
}

// UpdateRunStatus transitions a run after RunTransitionAllowed validates
// from -> to; terminal transitions are immutable.
func (tx *Tx) UpdateRunStatus(ctx context.Context, runID string, from, to model.RunStatus) error {
	if err := model.RunTransitionAllowed(from, to); err != nil {
		return err
	}
	return tx.transitionRun(ctx, runID, string(from), to, "", nil)
}

// ErrSerializationHeld reports that another non-queued nonterminal
// attempt already holds the instance's serialization slot (§5.3);
// callers treat it as normal flow, not failure.
var ErrSerializationHeld = errors.New("serialization slot for instance is already held")

// SetRunIdentifiers persists the dedicated session ID, caller-supplied
// RunID, rendered-prompt hash, and dispatching state before the prompt is
// submitted (§5.3). The queued -> dispatching transition is the
// transactional acquisition of the instance serialization slot: at most
// one non-queued nonterminal attempt may exist per project. The run's
// workspace, generation, and server URL are filled from the persisted
// instance record so SSE events correlate to the live endpoint.
func (tx *Tx) SetRunIdentifiers(ctx context.Context, runID string, sessionID, crushRunID, promptHash string) error {
	var holderCount int
	if err := tx.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM runs
		WHERE project = (SELECT project FROM runs WHERE id = ?)
			AND status IN ('dispatching','running','cancelling','unknown')
			AND id != ?`, runID, runID).Scan(&holderCount); err != nil {
		return err
	}
	if holderCount > 0 {
		return ErrSerializationHeld
	}
	if err := model.RunTransitionAllowed(model.RunQueued, model.RunDispatching); err != nil {
		return err
	}
	_, err := tx.db.ExecContext(ctx, `
		UPDATE runs SET session_id = ?, crush_run_id = ?, rendered_prompt_hash = ?,
			status = ?,
			workspace_id = COALESCE((SELECT workspace_id FROM instances
				WHERE project = (SELECT project FROM runs WHERE id = ?)), workspace_id),
			instance_generation = COALESCE((SELECT generation FROM instances
				WHERE project = (SELECT project FROM runs WHERE id = ?)), instance_generation),
			server_url = COALESCE(NULLIF((SELECT server_url FROM instances
				WHERE project = (SELECT project FROM runs WHERE id = ?)), ''), server_url)
		WHERE id = ? AND status = ?`,
		sessionID, crushRunID, promptHash, model.RunDispatching,
		runID, runID, runID, runID, model.RunQueued)
	if err != nil {
		return err
	}
	return tx.requireUpdated(ctx, runID)
}

// SetRunCancel records cause and request time atomically with the
// cancelling transition so recovery does not re-issue the cancel (§5.3).
func (tx *Tx) SetRunCancel(ctx context.Context, runID string, cause model.CancelCause) error {
	if err := model.RunTransitionAllowed(model.RunRunning, model.RunCancelling); err != nil {
		return err
	}
	now := nowISO()
	res, err := tx.db.ExecContext(ctx, `
		UPDATE runs SET status = ?, cancel_cause = ?, cancel_requested_at = ?
		WHERE id = ? AND status = ?`,
		model.RunCancelling, cause, now, runID, model.RunRunning)
	if err != nil {
		return err
	}
	return requireRows(res, runID)
}

// transitionRun applies one guarded status transition, stamping
// finalization time and optional cause.
func (tx *Tx) transitionRun(ctx context.Context, runID, from string, to model.RunStatus, cause string, cancelRequestedAt any) error {
	finalizedAt := any(nil)
	if model.RunIsTerminal(to) {
		finalizedAt = nowISO()
	}
	res, err := tx.db.ExecContext(ctx, `
		UPDATE runs SET status = ?, cancel_cause = CASE WHEN ? != '' THEN ? ELSE cancel_cause END,
			cancel_requested_at = COALESCE(?, cancel_requested_at), finalized_at = ?
		WHERE id = ? AND status = ?`,
		to, cause, cause, cancelRequestedAt, finalizedAt, runID, from)
	if err != nil {
		return err
	}
	return requireRows(res, runID)
}

// requireRows errors when a guarded update matched no row.
func requireRows(res sql.Result, runID string) error {
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("run %q transition rejected: status changed concurrently", runID)
	}
	return nil
}

// requireUpdated is requireRows for statements without a result handle.
func (tx *Tx) requireUpdated(ctx context.Context, runID string) error {
	var status string
	if err := tx.db.QueryRowContext(ctx, "SELECT status FROM runs WHERE id = ?", runID).Scan(&status); err != nil {
		return err
	}
	if status != string(model.RunDispatching) {
		return fmt.Errorf("run %q did not enter dispatching (now %s)", runID, status)
	}
	return nil
}

// ActiveRuns lists nonterminal runs; used by startup adoption and the
// dispatch gate (§5.1, §5.2).
func (s *Store) ActiveRuns(ctx context.Context) ([]model.Run, error) {
	rows, err := s.db.QueryContext(ctx, selectRunSQL+`
		WHERE r.status IN ('`+string(model.RunQueued)+`','`+string(model.RunDispatching)+`','`+
		string(model.RunRunning)+`','`+string(model.RunCancelling)+`','`+string(model.RunUnknown)+`')
		ORDER BY r.created_at, r.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRuns(rows)
}

// LatestTerminalAttempt returns the highest-numbered terminal attempt of a
// target execution; it supplies the target's current status and result
// reference; earlier attempts are audit-only (§5.3).
func (s *Store) LatestTerminalAttempt(ctx context.Context, targetExecutionID string) (model.Run, error) {
	row := s.db.QueryRowContext(ctx, selectRunSQL+`
		WHERE r.target_execution_id = ? AND r.status IN
			('completed','failed','cancelled','timed_out','skipped','abandoned')
		ORDER BY r.attempt DESC LIMIT 1`, targetExecutionID)
	run, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Run{}, fmt.Errorf("no terminal attempt for execution %s", targetExecutionID)
	}
	return run, err
}

// AppendNote stores an accepted note and assigns its store-generated
// monotonically increasing note_id. Audience expansion is frozen here
// (§5.4).
func (tx *Tx) AppendNote(ctx context.Context, note model.Note) (int64, error) {
	participants, err := tx.goalParticipants(ctx, note.GoalID)
	if err != nil {
		return 0, err
	}
	audience := model.ExpandAudience(note.To, participants)
	if len(audience) == 0 {
		return 0, fmt.Errorf("note audience resolves to no participants of goal %s", note.GoalID)
	}
	res, err := tx.db.ExecContext(ctx, `
		INSERT INTO notes (goal_id, from_project, to_project, to_tag, to_all, body, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		note.GoalID, note.From, note.To.Project, note.To.Tag, boolToInt(note.To.All),
		note.Body, nowISO())
	if err != nil {
		return 0, err
	}
	noteID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for _, project := range audience {
		if _, err := tx.db.ExecContext(ctx,
			"INSERT OR IGNORE INTO note_audience (note_id, goal_id, project) VALUES (?, ?, ?)",
			noteID, note.GoalID, project); err != nil {
			return 0, err
		}
	}
	return noteID, nil
}
