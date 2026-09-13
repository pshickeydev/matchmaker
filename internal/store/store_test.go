package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pshickeydev/matchmaker/internal/model"
)

// statFile is os.Stat with test-side fatality left to the caller.
func statFile(path string) (os.FileInfo, error) { return os.Stat(path) }

func openStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// fixtureGoal builds a two-step goal: step a on api, step b (needs a)
// fanned out to api and web, with frozen participant tags.
func fixtureGoal() model.Goal {
	return model.Goal{
		ID:        "g1",
		Type:      model.GoalTypeWork,
		Objective: "ship it",
		Steps: []model.Step{
			{ID: "a", GoalID: "g1", PromptTemplate: "do {{.Objective}}", Supervision: model.SupervisionDeny, Timeout: 10 * time.Minute},
			{ID: "b", GoalID: "g1", PromptTemplate: "review", Needs: []string{"a"}, Supervision: model.SupervisionGrantAll, Timeout: 5 * time.Minute, Retries: 2},
		},
		FrozenTargets: map[string][]model.ResolvedTarget{
			"a": {
				{StepID: "a", Project: "api", Instance: "api", ServerURL: "http://127.0.0.1:41001", Tags: []string{"lang:go", "team:platform"}},
			},
			"b": {
				{StepID: "b", Project: "api", Instance: "api", ServerURL: "http://127.0.0.1:41001", Tags: []string{"lang:go", "team:platform"}},
				{StepID: "b", Project: "web", Instance: "web", ServerURL: "http://127.0.0.1:41002", Tags: []string{"lang:ts", "team:platform"}},
			},
		},
	}
}

func acceptGoal(t *testing.T, st *Store, goal model.Goal) string {
	t.Helper()
	ctx := context.Background()
	err := st.WithinTx(ctx, func(tx *Tx) error {
		return tx.CreateGoal(ctx, goal)
	})
	if err != nil {
		t.Fatalf("CreateGoal: %v", err)
	}
	return goal.ID
}

func TestOpenCreatesSecuredStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state", "store.db")
	if _, err := Open(context.Background(), path); err != nil {
		t.Fatalf("Open: %v", err)
	}
	info, err := statFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("store file perms = %o", info.Mode().Perm())
	}
	dirInfo, err := statFile(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("state dir perms = %o", dirInfo.Mode().Perm())
	}
}

func TestCreateGoalRoundTrip(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	goalID := acceptGoal(t, st, fixtureGoal())
	goal, err := st.Goal(ctx, goalID)
	if err != nil {
		t.Fatalf("Goal: %v", err)
	}
	if goal.Status != model.GoalActive || goal.Type != model.GoalTypeWork {
		t.Errorf("goal = %+v", goal)
	}
	if len(goal.Steps) != 2 {
		t.Fatalf("steps = %d", len(goal.Steps))
	}
	stepB := goal.Steps[1]
	if stepB.ID != "b" || !stepB.AcceptPartialNeeds && stepB.Needs[0] != "a" {
		t.Errorf("step b = %+v", stepB)
	}
	if stepB.Supervision != model.SupervisionGrantAll || stepB.Retries != 2 {
		t.Errorf("step b policy = %+v", stepB)
	}
	targets := goal.FrozenTargets["b"]
	if len(targets) != 2 || targets[0].Project != "api" || targets[1].Project != "web" {
		t.Errorf("frozen targets b = %+v", targets)
	}
	if len(targets[0].Tags) != 2 {
		t.Errorf("frozen tags lost: %+v", targets[0])
	}

	runs, err := st.ActiveRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 {
		t.Fatalf("initial attempts = %d, want 3", len(runs))
	}
	for _, run := range runs {
		if run.Status != model.RunQueued || run.Attempt != 1 {
			t.Errorf("initial run = %+v", run)
		}
	}
}

func TestCreatePlanningGoal(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	goal := fixtureGoal()
	goal.Type = model.GoalTypePlan
	err := st.WithinTx(ctx, func(tx *Tx) error {
		return tx.CreatePlanningGoal(ctx, goal, "plan the thing")
	})
	if err != nil {
		t.Fatalf("CreatePlanningGoal: %v", err)
	}
}

func TestCreateGoalAtomicRollback(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	goal := fixtureGoal()
	goal.Steps = append(goal.Steps, model.Step{ID: "a", GoalID: "g1", PromptTemplate: "dup"})
	err := st.WithinTx(ctx, func(tx *Tx) error { return tx.CreateGoal(ctx, goal) })
	if err == nil {
		t.Fatal("duplicate step accepted")
	}
	if goals, _ := st.ListGoals(ctx, model.GoalActive); len(goals) != 0 {
		t.Errorf("goal persisted after failed transaction: %d goals", len(goals))
	}
	if runs, _ := st.ActiveRuns(ctx); len(runs) != 0 {
		t.Errorf("runs persisted after failed transaction: %d runs", len(runs))
	}
}

func TestCreateGoalRejectsMissingExpansion(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	goal := fixtureGoal()
	delete(goal.FrozenTargets, "b")
	if err := st.WithinTx(ctx, func(tx *Tx) error { return tx.CreateGoal(ctx, goal) }); err == nil {
		t.Fatal("step without frozen targets accepted")
	}
}

func runForProject(t *testing.T, st *Store, stepID, project string) model.Run {
	t.Helper()
	runs, err := st.ActiveRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if run.Project == project && run.StepID == stepID {
			return run
		}
	}
	t.Fatalf("no queued run for step %s project %s", stepID, project)
	return model.Run{}
}

func TestRunTransitionsAndSerialization(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	acceptGoal(t, st, fixtureGoal())
	apiRun := runForProject(t, st, "a", "api")
	// webRun holds the instance slot for project web.
	webRun := runForProject(t, st, "b", "web")
	stepBRun := runForProject(t, st, "b", "api")

	// Serialization: dispatching apiRun acquires the api slot.
	if err := st.WithinTx(ctx, func(tx *Tx) error {
		return tx.SetRunIdentifiers(ctx, apiRun.ID, "sess-a", "crush-a", "hash-a")
	}); err != nil {
		t.Fatalf("SetRunIdentifiers: %v", err)
	}
	// A second api attempt cannot acquire the slot while one is held.
	if err := st.WithinTx(ctx, func(tx *Tx) error {
		return tx.SetRunIdentifiers(ctx, stepBRun.ID, "sess-b", "crush-b", "hash-b")
	}); err == nil {
		t.Fatal("serialization slot double-acquired")
	}

	if err := st.WithinTx(ctx, func(tx *Tx) error {
		return tx.UpdateRunStatus(ctx, apiRun.ID, model.RunDispatching, model.RunRunning)
	}); err != nil {
		t.Fatalf("dispatching -> running: %v", err)
	}

	// Timeout cancellation: running -> cancelling with cause and request time.
	if err := st.WithinTx(ctx, func(tx *Tx) error {
		return tx.SetRunCancel(ctx, apiRun.ID, model.CancelCauseTimeout)
	}); err != nil {
		t.Fatalf("SetRunCancel: %v", err)
	}
	cancelled, err := st.GetRun(ctx, apiRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != model.RunCancelling || cancelled.CancelCause != model.CancelCauseTimeout || cancelled.CancelRequestedAt == nil {
		t.Errorf("cancelled run = %+v", cancelled)
	}

	if err := st.WithinTx(ctx, func(tx *Tx) error {
		return tx.UpdateRunStatus(ctx, apiRun.ID, model.RunCancelling, model.RunTimedOut)
	}); err != nil {
		t.Fatalf("cancelling -> timed_out: %v", err)
	}

	// Terminal is immutable.
	if err := st.WithinTx(ctx, func(tx *Tx) error {
		return tx.UpdateRunStatus(ctx, apiRun.ID, model.RunTimedOut, model.RunRunning)
	}); err == nil {
		t.Fatal("terminal run transitioned")
	}

	// The slot is free again after the holder went terminal.
	if err := st.WithinTx(ctx, func(tx *Tx) error {
		return tx.SetRunIdentifiers(ctx, stepBRun.ID, "sess-b2", "crush-b2", "hash-b2")
	}); err != nil {
		t.Errorf("slot not released after terminal: %v", err)
	}

	// webRun (different project) was never blocked.
	if err := st.WithinTx(ctx, func(tx *Tx) error {
		return tx.SetRunIdentifiers(ctx, webRun.ID, "sess-w", "crush-w", "hash-w")
	}); err != nil {
		t.Errorf("other project blocked by api holder: %v", err)
	}
}

func TestIllegalRunTransitionRejected(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	acceptGoal(t, st, fixtureGoal())
	run := runForProject(t, st, "a", "api")
	if err := st.WithinTx(ctx, func(tx *Tx) error {
		return tx.UpdateRunStatus(ctx, run.ID, model.RunQueued, model.RunCompleted)
	}); err == nil {
		t.Fatal("queued -> completed permitted")
	}
}

func TestCreateRunRequiresTerminalPrior(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	acceptGoal(t, st, fixtureGoal())
	run := runForProject(t, st, "a", "api")
	err := st.WithinTx(ctx, func(tx *Tx) error {
		return tx.CreateRun(ctx, model.Run{
			GoalID: run.GoalID, StepID: run.StepID,
			TargetExecutionID: run.TargetExecutionID, Project: run.Project,
			ServerURL: run.ServerURL, Status: model.RunQueued,
		})
	})
	if err == nil {
		t.Fatal("retry created while prior attempt nonterminal")
	}
	// Terminal the prior attempt, then the retry appends as attempt 2.
	if err := st.WithinTx(ctx, func(tx *Tx) error {
		if err := tx.SetRunIdentifiers(ctx, run.ID, "s", "c", "h"); err != nil {
			return err
		}
		if err := tx.UpdateRunStatus(ctx, run.ID, model.RunDispatching, model.RunRunning); err != nil {
			return err
		}
		return tx.UpdateRunStatus(ctx, run.ID, model.RunRunning, model.RunFailed)
	}); err != nil {
		t.Fatal(err)
	}
	err = st.WithinTx(ctx, func(tx *Tx) error {
		return tx.CreateRun(ctx, model.Run{
			GoalID: run.GoalID, StepID: run.StepID,
			TargetExecutionID: run.TargetExecutionID, Project: run.Project,
			ServerURL: run.ServerURL, Status: model.RunQueued,
		})
	})
	if err != nil {
		t.Fatalf("retry after terminal: %v", err)
	}
	exec, err := st.TargetExecution(ctx, run.TargetExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(exec.Attempts) != 2 || exec.Attempts[1].Attempt != 2 {
		t.Errorf("attempts = %+v", exec.Attempts)
	}
	latest, err := st.LatestTerminalAttempt(ctx, run.TargetExecutionID)
	if err != nil || latest.Attempt != 1 || latest.Status != model.RunFailed {
		t.Errorf("latest terminal = %+v err=%v", latest, err)
	}
}

func TestRunBySession(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	acceptGoal(t, st, fixtureGoal())
	run := runForProject(t, st, "a", "api")
	// The reconciler records the instance endpoint before dispatch.
	if err := st.WithinTx(ctx, func(tx *Tx) error {
		return tx.SetInstanceEndpoint(ctx, "api", "http://127.0.0.1:41001", 41001, "ws-api")
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.WithinTx(ctx, func(tx *Tx) error {
		if err := tx.SetInstanceState(ctx, "api", model.InstanceStarting, 7); err != nil {
			return err
		}
		return tx.SetInstanceState(ctx, "api", model.InstanceReady, 7)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.WithinTx(ctx, func(tx *Tx) error {
		return tx.SetRunIdentifiers(ctx, run.ID, "sess-a", "crush-a", "hash-a")
	}); err != nil {
		t.Fatal(err)
	}
	updated, err := st.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.WorkspaceID != "ws-api" || updated.InstanceGeneration != 7 {
		t.Errorf("run did not inherit endpoint identity: %+v", updated)
	}
	got, err := st.RunBySession(ctx, "ws-api", "sess-a")
	if err != nil || got.ID != run.ID {
		t.Errorf("RunBySession = %+v err=%v", got, err)
	}
	if _, err := st.RunBySession(ctx, "nope", "sess-a"); err == nil {
		t.Error("unknown workspace correlated")
	}
}

func TestNotesAudienceAndCursors(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	goalID := acceptGoal(t, st, fixtureGoal())
	var noteIDs []int64
	send := func(to model.Audience) {
		var id int64
		err := st.WithinTx(ctx, func(tx *Tx) error {
			var err error
			id, err = tx.AppendNote(ctx, model.Note{GoalID: goalID, From: "api", To: to, Body: "body"})
			return err
		})
		if err != nil {
			t.Fatalf("AppendNote: %v", err)
		}
		noteIDs = append(noteIDs, id)
	}
	send(model.Audience{Project: "web"})
	send(model.Audience{Tag: "team:platform"})
	send(model.Audience{All: true})
	if noteIDs[1] <= noteIDs[0] || noteIDs[2] <= noteIDs[1] {
		t.Errorf("note ids not monotonic: %v", noteIDs)
	}
	webNotes, err := st.NotesFor(ctx, goalID, "web", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(webNotes) != 3 {
		t.Fatalf("web notes = %d, want 3 (explicit, tag, all)", len(webNotes))
	}
	apiNotes, err := st.NotesFor(ctx, goalID, "api", noteIDs[1])
	if err != nil {
		t.Fatal(err)
	}
	if len(apiNotes) != 1 || apiNotes[0].NoteID != noteIDs[2] {
		t.Errorf("api notes after cursor = %+v", apiNotes)
	}
	if cursor, _ := st.NoteCursor(ctx, goalID, "web"); cursor != 0 {
		t.Errorf("reads advanced the durable cursor: %d", cursor)
	}
	if err := st.WithinTx(ctx, func(tx *Tx) error {
		return tx.AdvanceNoteCursor(ctx, goalID, "web", noteIDs[1])
	}); err != nil {
		t.Fatal(err)
	}
	if cursor, _ := st.NoteCursor(ctx, goalID, "web"); cursor != noteIDs[1] {
		t.Errorf("cursor = %d", cursor)
	}
	// Cursor never moves backward.
	if err := st.WithinTx(ctx, func(tx *Tx) error {
		return tx.AdvanceNoteCursor(ctx, goalID, "web", noteIDs[0])
	}); err != nil {
		t.Fatal(err)
	}
	if cursor, _ := st.NoteCursor(ctx, goalID, "web"); cursor != noteIDs[1] {
		t.Errorf("cursor moved backward: %d", cursor)
	}
	if count, _ := st.CountNotes(ctx, goalID); count != 3 {
		t.Errorf("CountNotes = %d", count)
	}
}

func TestPermissionDecisions(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	decision := model.PermissionDecision{
		GoalID: "g", RunID: "r", Project: "api", WorkspaceID: "w", SessionID: "s",
		InstanceGeneration: 3, RequestID: "req-1", RequestType: "bash",
		Policy: model.SupervisionDeny, Decision: model.DecisionDeny,
	}
	if err := st.WithinTx(ctx, func(tx *Tx) error {
		return tx.RecordPermissionDecision(ctx, decision)
	}); err != nil {
		t.Fatal(err)
	}
	got, found, err := st.PermissionDecisionByRequest(ctx, "req-1")
	if err != nil || !found || got.Decision != model.DecisionDeny || got.Policy != model.SupervisionDeny {
		t.Errorf("lookup = %+v found=%v err=%v", got, found, err)
	}
	if _, found, _ := st.PermissionDecisionByRequest(ctx, "req-2"); found {
		t.Error("unknown request id found")
	}
	// Duplicate request IDs cannot create a second decision.
	dup := decision
	dup.Decision = model.DecisionAllow
	if err := st.WithinTx(ctx, func(tx *Tx) error {
		return tx.RecordPermissionDecision(ctx, dup)
	}); err == nil {
		t.Fatal("duplicate request id recorded a second decision")
	}
}

func TestFingerprintsAndOverrides(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, found, _ := st.ApprovedFingerprint(ctx, "api"); found {
		t.Error("fresh store has an approved fingerprint")
	}
	fingerprint := model.Fingerprint{
		Project: "api", ApprovedBy: "op",
		Entries: []model.FingerprintEntry{
			{Path: "/repo/api/.crushrc", Digest: "d1"},
			{Path: "/repo/api/crushrc", Digest: "d2"},
		},
	}
	if err := st.WithinTx(ctx, func(tx *Tx) error {
		return tx.SetApprovedFingerprint(ctx, fingerprint)
	}); err != nil {
		t.Fatal(err)
	}
	got, found, err := st.ApprovedFingerprint(ctx, "api")
	if err != nil || !found || len(got.Entries) != 2 || got.ApprovedBy != "op" {
		t.Errorf("fingerprint = %+v found=%v err=%v", got, found, err)
	}
	fingerprint.Entries = []model.FingerprintEntry{{Path: "/repo/api/.crushrc", Digest: "d3"}}
	if err := st.WithinTx(ctx, func(tx *Tx) error {
		return tx.SetApprovedFingerprint(ctx, fingerprint)
	}); err != nil {
		t.Fatal(err)
	}
	got, _, _ = st.ApprovedFingerprint(ctx, "api")
	if len(got.Entries) != 1 || got.Entries[0].Digest != "d3" {
		t.Errorf("fingerprint entries not replaced: %+v", got.Entries)
	}

	if found, _ := st.CompatibilityOverride(ctx, "v0.94.1", "build9"); found {
		t.Error("unrecorded override found")
	}
	if err := st.WithinTx(ctx, func(tx *Tx) error {
		return tx.RecordCompatibilityOverride(ctx, "v0.94.1", "build9", "op")
	}); err != nil {
		t.Fatal(err)
	}
	if found, _ := st.CompatibilityOverride(ctx, "v0.94.1", "build9"); !found {
		t.Error("recorded override missing")
	}
}

func TestInstanceStateTransitions(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	set := func(state model.InstanceState, generation int64) error {
		return st.WithinTx(ctx, func(tx *Tx) error {
			return tx.SetInstanceState(ctx, "api", state, generation)
		})
	}
	if err := set(model.InstanceStopped, 0); err != nil {
		t.Fatalf("initial state: %v", err)
	}
	if err := set(model.InstanceStarting, 1); err != nil {
		t.Fatalf("stopped -> starting: %v", err)
	}
	if err := set(model.InstanceReady, 1); err != nil {
		t.Fatalf("starting -> ready: %v", err)
	}
	if err := set(model.InstanceStopped, 1); err == nil {
		t.Fatal("ready -> stopped permitted")
	}
	if err := set(model.InstanceBusy, 2); err != nil {
		t.Fatalf("ready -> busy: %v", err)
	}
	instance, err := st.Instance(ctx, "api")
	if err != nil || instance.State != model.InstanceBusy || instance.Generation != 2 {
		t.Errorf("instance = %+v err=%v", instance, err)
	}
	if instance.Fingerprint != nil {
		t.Errorf("instance unexpectedly carries fingerprint")
	}
}

func TestIdempotencyDedupe(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	if _, found, _ := st.IdempotencyLookup(ctx, "key-1"); found {
		t.Error("unseen key found")
	}
	if err := st.WithinTx(ctx, func(tx *Tx) error {
		return tx.RecordIdempotency(ctx, "key-1", []byte(`{"goal_id":"g1"}`))
	}); err != nil {
		t.Fatal(err)
	}
	got, found, err := st.IdempotencyLookup(ctx, "key-1")
	if err != nil || !found || string(got) != `{"goal_id":"g1"}` {
		t.Errorf("lookup = %q found=%v err=%v", got, found, err)
	}
}

func TestPruneRemovesFinalizedGoals(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	first := acceptGoal(t, st, fixtureGoal())
	secondGoal := fixtureGoal()
	secondGoal.ID = "g2"
	for i := range secondGoal.Steps {
		secondGoal.Steps[i].GoalID = "g2"
	}
	second := acceptGoal(t, st, secondGoal)
	finalize := func(goalID string) {
		if err := st.WithinTx(ctx, func(tx *Tx) error {
			return tx.UpdateGoalStatus(ctx, goalID, model.GoalSucceeded)
		}); err != nil {
			t.Fatal(err)
		}
	}
	finalize(first)
	finalize(second)
	if err := st.Prune(ctx, PrunePolicy{KeepLast: 1}); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if _, err := st.Goal(ctx, first); err == nil {
		t.Error("pruned goal still readable")
	}
	if _, err := st.Goal(ctx, second); err != nil {
		t.Errorf("kept goal pruned: %v", err)
	}
	runs, _ := st.ActiveRuns(ctx)
	for _, run := range runs {
		if run.GoalID == first {
			t.Errorf("run of pruned goal survived: %+v", run)
		}
	}
	if notes, _ := st.NotesFor(ctx, first, "api", 0); len(notes) != 0 {
		t.Errorf("notes of pruned goal survived: %d", len(notes))
	}
}

func TestListGoalsByStatus(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	goalID := acceptGoal(t, st, fixtureGoal())
	active, err := st.ListGoals(ctx, model.GoalActive)
	if err != nil || len(active) != 1 || active[0].ID != goalID {
		t.Fatalf("active goals = %+v err=%v", active, err)
	}
	done, _ := st.ListGoals(ctx, model.GoalSucceeded)
	if len(done) != 0 {
		t.Errorf("succeeded goals = %d", len(done))
	}
}
