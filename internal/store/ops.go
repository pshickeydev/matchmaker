package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pshickeydev/matchmaker/internal/model"
)

// newID generates one store identifier with the given prefix.
func newID(prefix string) string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		// Fall back to a time-derived identifier; uniqueness within one
		// process is still monotonic enough for diagnostics.
		return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(bytes[:])
}

// nowISO stamps one UTC timestamp.
func nowISO() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// parseISO decodes one stored timestamp.
func parseISO(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

// boolToInt maps a bool onto an INTEGER column.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// goalParticipants loads the goal's participating projects with their
// frozen tags, the input to audience expansion (§5.4).
func (tx *Tx) goalParticipants(ctx context.Context, goalID string) (map[string][]string, error) {
	rows, err := tx.db.QueryContext(ctx,
		"SELECT project, tags FROM frozen_targets WHERE goal_id = ?", goalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	participants := make(map[string][]string)
	for rows.Next() {
		var project, tagsJSON string
		if err := rows.Scan(&project, &tagsJSON); err != nil {
			return nil, err
		}
		if _, seen := participants[project]; seen {
			continue
		}
		var tags []string
		if err := json.Unmarshal([]byte(tagsJSON), &tags); err != nil {
			return nil, fmt.Errorf("frozen tags for %s: %w", project, err)
		}
		participants[project] = tags
	}
	return participants, rows.Err()
}

// NotesFor returns notes addressed to project with note_id greater than
// since, ascending, up to the page limits. Reads never advance the durable
// delivery cursor (§5.4).
func (s *Store) NotesFor(ctx context.Context, goalID, project string, since int64) ([]model.Note, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.note_id, n.goal_id, n.from_project, n.to_project, n.to_tag, n.to_all,
			n.body, n.created_at
		FROM notes n
		JOIN note_audience a ON a.note_id = n.note_id AND a.goal_id = n.goal_id
		WHERE n.goal_id = ? AND a.project = ? AND n.note_id > ?
		ORDER BY n.note_id ASC`, goalID, project, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var notes []model.Note
	for rows.Next() {
		var note model.Note
		var toAll int
		var createdAt string
		if err := rows.Scan(&note.NoteID, &note.GoalID, &note.From, &note.To.Project,
			&note.To.Tag, &toAll, &note.Body, &createdAt); err != nil {
			return nil, err
		}
		note.To.All = toAll != 0
		note.CreatedAt = parseISO(createdAt)
		notes = append(notes, note)
	}
	return notes, rows.Err()
}

// NoteCursor returns the durable high-water note_id for a (goal, instance)
// audience (§5.4).
func (s *Store) NoteCursor(ctx context.Context, goalID, project string) (int64, error) {
	var cursor int64
	err := s.db.QueryRowContext(ctx,
		"SELECT note_id FROM note_cursors WHERE goal_id = ? AND project = ?",
		goalID, project).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return cursor, nil
}

// AdvanceNoteCursor moves the delivery cursor forward; it commits in the
// same transaction as accepted prompt submission (§5.4).
func (tx *Tx) AdvanceNoteCursor(ctx context.Context, goalID, project string, noteID int64) error {
	_, err := tx.db.ExecContext(ctx, `
		INSERT INTO note_cursors (goal_id, project, note_id) VALUES (?, ?, ?)
		ON CONFLICT (goal_id, project) DO UPDATE SET note_id = MAX(note_cursors.note_id, excluded.note_id)`,
		goalID, project, noteID)
	return err
}

// RecordPermissionDecision transactionally records workspace ID, session
// ID, run ID, instance generation, request ID, request type, policy, and
// decision before Matchmaker responds. If recording fails, supervision
// sends deny; never grant an unaudited request (§5.3, §6).
func (tx *Tx) RecordPermissionDecision(ctx context.Context, d model.PermissionDecision) error {
	if d.ID == "" {
		d.ID = newID(decisionPrefix)
	}
	_, err := tx.db.ExecContext(ctx, `
		INSERT INTO permission_decisions (id, goal_id, run_id, project, workspace_id,
			session_id, instance_generation, request_id, request_type, policy, decision,
			correlation_failed, decided_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.GoalID, d.RunID, d.Project, d.WorkspaceID, d.SessionID,
		d.InstanceGeneration, d.RequestID, d.RequestType, d.Policy, d.Decision,
		boolToInt(d.CorrelationFailed), nowISO())
	return err
}

// PermissionDecisionByRequest returns the recorded decision for a
// duplicate request ID; duplicates reuse the recorded decision (§5.3).
func (s *Store) PermissionDecisionByRequest(ctx context.Context, requestID string) (model.PermissionDecision, bool, error) {
	if requestID == "" {
		return model.PermissionDecision{}, false, nil
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT id, goal_id, run_id, project, workspace_id, session_id, instance_generation,
			request_id, request_type, policy, decision, correlation_failed, decided_at
		FROM permission_decisions WHERE request_id = ?`, requestID)
	var d model.PermissionDecision
	var correlationFailed int
	var decidedAt string
	err := row.Scan(&d.ID, &d.GoalID, &d.RunID, &d.Project, &d.WorkspaceID, &d.SessionID,
		&d.InstanceGeneration, &d.RequestID, &d.RequestType, &d.Policy, &d.Decision,
		&correlationFailed, &decidedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.PermissionDecision{}, false, nil
	}
	if err != nil {
		return model.PermissionDecision{}, false, err
	}
	d.CorrelationFailed = correlationFailed != 0
	d.DecidedAt = parseISO(decidedAt)
	return d, true, nil
}

// RecordQuestionEvent persists bounded question metadata (never raw text
// or choices) atomically with issuing the cancel (§5.3).
func (tx *Tx) RecordQuestionEvent(ctx context.Context, goalID, runID, questionID string) error {
	_, err := tx.db.ExecContext(ctx, `
		INSERT INTO question_events (goal_id, run_id, question_id, created_at)
		VALUES (?, ?, ?, ?)`, goalID, runID, questionID, nowISO())
	return err
}

// ApprovedFingerprint returns the operator-approved config fingerprint of
// a project (§5.1).
func (s *Store) ApprovedFingerprint(ctx context.Context, project string) (model.Fingerprint, bool, error) {
	var fingerprint model.Fingerprint
	var approvedAt string
	err := s.db.QueryRowContext(ctx,
		"SELECT project, approved_by, approved_at FROM fingerprints WHERE project = ?",
		project).Scan(&fingerprint.Project, &fingerprint.ApprovedBy, &approvedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Fingerprint{}, false, nil
	}
	if err != nil {
		return model.Fingerprint{}, false, err
	}
	fingerprint.ApprovedAt = parseISO(approvedAt)
	rows, err := s.db.QueryContext(ctx,
		"SELECT path, digest FROM fingerprint_entries WHERE project = ? ORDER BY path", project)
	if err != nil {
		return model.Fingerprint{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var entry model.FingerprintEntry
		if err := rows.Scan(&entry.Path, &entry.Digest); err != nil {
			return model.Fingerprint{}, false, err
		}
		fingerprint.Entries = append(fingerprint.Entries, entry)
	}
	return fingerprint, true, rows.Err()
}

// SetApprovedFingerprint records an explicit operator approval of a new
// fingerprint (§5.1).
func (tx *Tx) SetApprovedFingerprint(ctx context.Context, f model.Fingerprint) error {
	if _, err := tx.db.ExecContext(ctx, `
		INSERT INTO fingerprints (project, approved_by, approved_at) VALUES (?, ?, ?)
		ON CONFLICT (project) DO UPDATE SET approved_by = excluded.approved_by,
			approved_at = excluded.approved_at`,
		f.Project, f.ApprovedBy, nowISO()); err != nil {
		return err
	}
	if _, err := tx.db.ExecContext(ctx,
		"DELETE FROM fingerprint_entries WHERE project = ?", f.Project); err != nil {
		return err
	}
	for _, entry := range f.Entries {
		if _, err := tx.db.ExecContext(ctx,
			"INSERT INTO fingerprint_entries (project, path, digest) VALUES (?, ?, ?)",
			f.Project, entry.Path, entry.Digest); err != nil {
			return err
		}
	}
	return nil
}

// RecordCompatibilityOverride records an explicit operator compatibility
// override for an observed Crush version and build_id (§9.3). Overrides
// are deployment-specific and audited.
func (tx *Tx) RecordCompatibilityOverride(ctx context.Context, version, buildID, approvedBy string) error {
	_, err := tx.db.ExecContext(ctx, `
		INSERT INTO compatibility_overrides (version, build_id, approved_by, approved_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (version, build_id) DO UPDATE SET approved_by = excluded.approved_by,
			approved_at = excluded.approved_at`,
		version, buildID, approvedBy, nowISO())
	return err
}

// CompatibilityOverride returns any recorded override for a version/build
// pair.
func (s *Store) CompatibilityOverride(ctx context.Context, version, buildID string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM compatibility_overrides WHERE version = ? AND build_id = ?",
		version, buildID).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// InstanceState persists the current instance state and generation.
func (tx *Tx) SetInstanceState(ctx context.Context, project string, state model.InstanceState, generation int64) error {
	var current model.InstanceState
	err := tx.db.QueryRowContext(ctx,
		"SELECT state FROM instances WHERE project = ?", project).Scan(&current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.db.ExecContext(ctx, `
			INSERT INTO instances (project, state, generation) VALUES (?, ?, ?)`,
			project, state, generation)
		return err
	case err != nil:
		return err
	}
	if err := model.InstanceTransitionAllowed(current, state); err != nil {
		return fmt.Errorf("instance %s: %w", project, err)
	}
	_, err = tx.db.ExecContext(ctx,
		"UPDATE instances SET state = ?, generation = ? WHERE project = ?",
		state, generation, project)
	return err
}

// SetInstanceEndpoint records the instance's live endpoint identity —
// server URL, port, and adopted workspace — after startup or adoption
// (DESIGN §5.1). Dispatch and SSE correlation read it back through
// SetRunIdentifiers and Instance.
func (tx *Tx) SetInstanceEndpoint(ctx context.Context, project, serverURL string, port int, workspaceID string) error {
	_, err := tx.db.ExecContext(ctx, `
		INSERT INTO instances (project, state, generation, port, server_url, workspace_id)
		VALUES (?, ?, 0, ?, ?, ?)
		ON CONFLICT (project) DO UPDATE SET
			port = excluded.port,
			server_url = excluded.server_url,
			workspace_id = CASE WHEN excluded.workspace_id != ''
				THEN excluded.workspace_id ELSE instances.workspace_id END`,
		project, model.InstanceStopped, port, serverURL, workspaceID)
	return err
}

// Instance loads the persisted instance record.
func (s *Store) Instance(ctx context.Context, project string) (model.Instance, error) {
	var instance model.Instance
	err := s.db.QueryRowContext(ctx, `
		SELECT project, state, generation, port, server_url, workspace_id, failed_reason
		FROM instances WHERE project = ?`, project).
		Scan(&instance.Project, &instance.State, &instance.Generation, &instance.Port,
			&instance.ServerURL, &instance.WorkspaceID, &instance.FailedReason)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Instance{}, fmt.Errorf("instance %q not recorded", project)
	}
	if err != nil {
		return model.Instance{}, err
	}
	fingerprint, found, err := s.ApprovedFingerprint(ctx, project)
	if err != nil {
		return model.Instance{}, err
	}
	if found {
		instance.Fingerprint = &fingerprint
	}
	return instance, nil
}

// Prune removes Matchmaker metadata according to operator policy; Crush
// owns session-log retention (§5.5).
func (s *Store) Prune(ctx context.Context, policy PrunePolicy) error {
	cutoff := ""
	if policy.RetainDays > 0 {
		cutoff = time.Now().UTC().AddDate(0, 0, -policy.RetainDays).Format(time.RFC3339Nano)
	}
	goalIDs, err := s.pruneGoalIDs(ctx, cutoff, policy.KeepLast)
	if err != nil {
		return err
	}
	for _, goalID := range goalIDs {
		if err := s.WithinTx(ctx, func(tx *Tx) error {
			for _, statement := range pruneGoalStatements(policy.IncludeAudit) {
				if _, err := tx.db.ExecContext(ctx, statement, goalID); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return fmt.Errorf("prune goal %s: %w", goalID, err)
		}
	}
	return nil
}

// pruneGoalIDs selects finalized goals older than cutoff and beyond the
// keep-last window.
func (s *Store) pruneGoalIDs(ctx context.Context, cutoff string, keepLast int) ([]string, error) {
	query := "SELECT id FROM goals WHERE status != ? AND finalized_at IS NOT NULL"
	args := []any{model.GoalActive}
	if cutoff != "" {
		query += " AND finalized_at < ?"
		args = append(args, cutoff)
	}
	if keepLast > 0 {
		query += " ORDER BY finalized_at DESC LIMIT -1 OFFSET ?"
		args = append(args, keepLast)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// pruneGoalStatements lists the per-goal deletions in dependency-safe
// order.
func pruneGoalStatements(includeAudit bool) []string {
	statements := []string{
		"DELETE FROM runs WHERE goal_id = ?",
		"DELETE FROM target_executions WHERE goal_id = ?",
		"DELETE FROM frozen_targets WHERE goal_id = ?",
		"DELETE FROM steps WHERE goal_id = ?",
		"DELETE FROM note_audience WHERE goal_id = ?",
		"DELETE FROM notes WHERE goal_id = ?",
		"DELETE FROM note_cursors WHERE goal_id = ?",
	}
	if includeAudit {
		statements = append(statements,
			"DELETE FROM permission_decisions WHERE goal_id = ?",
			"DELETE FROM question_events WHERE goal_id = ?")
	}
	statements = append(statements, "DELETE FROM goals WHERE id = ?")
	return statements
}

// PrunePolicy selects what the prune/gc subcommand removes.
type PrunePolicy struct {
	RetainDays   int
	KeepLast     int
	IncludeAudit bool
}

// SetRunNotes records the selected note high-water with the run; it
// commits in the same transaction as accepted prompt submission and the
// audience cursor advance (DESIGN §5.4).
func (tx *Tx) SetRunNotes(ctx context.Context, runID string, noteID int64) error {
	_, err := tx.db.ExecContext(ctx,
		"UPDATE runs SET notes_high_water = ? WHERE id = ?", noteID, runID)
	return err
}

// CountNotes returns the note count for limit enforcement before writes
// (§5.4).
func (s *Store) CountNotes(ctx context.Context, goalID string) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM notes WHERE goal_id = ?", goalID).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}
