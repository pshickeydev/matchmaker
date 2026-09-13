package aggregate

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/pshickeydev/matchmaker/internal/model"
	"github.com/pshickeydev/matchmaker/internal/store"
)

// stubSource resolves fixed outputs per run.
type stubSource struct {
	outputs map[string][]byte
}

func (s stubSource) FinalOutput(_ context.Context, run model.Run) ([]byte, bool, error) {
	output, ok := s.outputs[run.ID]
	return output, ok, nil
}

// rig seeds a store with a two-target step and drives attempts terminal.
type rig struct {
	t    *testing.T
	st   *store.Store
	agg  *Aggregator
	goal model.Goal
	runs map[string]model.Run
}

func newRig(t *testing.T, retries int) *rig {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	goal := model.Goal{
		ID: "g1", Type: model.GoalTypeWork, Status: model.GoalActive,
		Steps: []model.Step{{ID: "a", GoalID: "g1", PromptTemplate: "p",
			Supervision: model.SupervisionDeny, Timeout: time.Minute, Retries: retries}},
		FrozenTargets: map[string][]model.ResolvedTarget{
			"a": {
				{StepID: "a", Project: "api", Instance: "api", ServerURL: "http://127.0.0.1:1"},
				{StepID: "a", Project: "web", Instance: "web", ServerURL: "http://127.0.0.1:2"},
			},
		},
	}
	if err := st.WithinTx(ctx, func(tx *store.Tx) error { return tx.CreateGoal(ctx, goal) }); err != nil {
		t.Fatal(err)
	}
	loaded, _ := st.Goal(ctx, "g1")
	runs := make(map[string]model.Run)
	for _, target := range loaded.FrozenTargets["a"] {
		exec, _ := st.TargetExecution(ctx, target.ExecutionID)
		runs[target.Project] = exec.Attempts[0]
	}
	return &rig{t: t, st: st, agg: New(st, stubSource{outputs: map[string][]byte{}}), goal: loaded, runs: runs}
}

// drive takes one project's attempt to a terminal state through the store.
func (r *rig) drive(project string, terminal model.RunStatus) {
	r.t.Helper()
	ctx := context.Background()
	run := r.runs[project]
	err := r.st.WithinTx(ctx, func(tx *store.Tx) error {
		if err := tx.SetRunIdentifiers(ctx, run.ID, "sess-"+project, "crush-"+project, "hash"); err != nil {
			return err
		}
		if err := tx.UpdateRunStatus(ctx, run.ID, model.RunDispatching, model.RunRunning); err != nil {
			return err
		}
		return tx.UpdateRunStatus(ctx, run.ID, model.RunRunning, terminal)
	})
	if err != nil {
		r.t.Fatalf("drive %s: %v", project, err)
	}
}

func (r *rig) stepStatus() (model.StepStatus, bool) {
	r.t.Helper()
	status, terminal, err := r.agg.StepStatus(context.Background(), r.goal.ID, "a")
	if err != nil {
		r.t.Fatal(err)
	}
	return status, terminal
}

func TestStepStatusPendingWhileNonterminal(t *testing.T) {
	r := newRig(t, 0)
	if status, terminal := r.stepStatus(); terminal || status != model.StepPending {
		t.Fatalf("status = %s terminal=%v, want pending", status, terminal)
	}
}

func TestStepStatusUnknownBlocksAggregation(t *testing.T) {
	// A failed attempt with retry budget keeps the step pending until the
	// retry settles (§5.5: no retry remains eligible).
	r := newRig(t, 1)
	// A retried execution whose latest attempt is unknown: the terminal
	// first attempt cannot roll up while a nonterminal attempt exists.
	r.drive("api", model.RunFailed)
	ctx := context.Background()
	exec, _ := r.st.TargetExecution(ctx, r.goal.FrozenTargets["a"][0].ExecutionID)
	if err := r.st.WithinTx(ctx, func(tx *store.Tx) error {
		if err := tx.CreateRun(ctx, model.Run{GoalID: r.goal.ID, StepID: "a",
			TargetExecutionID: exec.ID, Project: "api", Status: model.RunQueued}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	loaded, _ := r.st.Goal(ctx, r.goal.ID)
	_ = loaded
	// The queued retry keeps the target pending (no terminal attempt is
	// the latest).
	if status, terminal := r.stepStatus(); terminal {
		t.Fatalf("queued retry rolled up: %s", status)
	}
}

func TestStepStatusRollups(t *testing.T) {
	t.Run("succeeded", func(t *testing.T) {
		r := newRig(t, 0)
		r.drive("api", model.RunCompleted)
		r.drive("web", model.RunCompleted)
		if status, terminal := r.stepStatus(); !terminal || status != model.StepSucceeded {
			t.Fatalf("status = %s terminal=%v", status, terminal)
		}
	})
	t.Run("failed", func(t *testing.T) {
		r := newRig(t, 0)
		r.drive("api", model.RunFailed)
		r.drive("web", model.RunCancelled)
		if status, terminal := r.stepStatus(); !terminal || status != model.StepFailed {
			t.Fatalf("status = %s terminal=%v", status, terminal)
		}
	})
	t.Run("partial", func(t *testing.T) {
		r := newRig(t, 0)
		r.drive("api", model.RunCompleted)
		r.drive("web", model.RunFailed)
		if status, terminal := r.stepStatus(); !terminal || status != model.StepPartial {
			t.Fatalf("status = %s terminal=%v", status, terminal)
		}
	})
}

func TestWakePersistsAndFinalizes(t *testing.T) {
	r := newRig(t, 0)
	ctx := context.Background()
	r.drive("api", model.RunCompleted)
	r.drive("web", model.RunCompleted)
	if err := r.agg.Wake(ctx, r.goal.ID); err != nil {
		t.Fatal(err)
	}
	goal, err := r.st.Goal(ctx, r.goal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if goal.Status != model.GoalSucceeded || goal.FinalizedAt == nil {
		t.Fatalf("goal = %s finalized=%v", goal.Status, goal.FinalizedAt)
	}
	step, err := r.st.Step(ctx, "a")
	if err != nil || step.ID != "a" {
		t.Fatalf("step = %+v err=%v", step, err)
	}
}

func TestBuildReportAndExport(t *testing.T) {
	r := newRig(t, 0)
	ctx := context.Background()
	r.drive("api", model.RunCompleted)
	r.drive("web", model.RunFailed)
	r.agg.sessions = stubSource{outputs: map[string][]byte{r.runs["api"].ID: []byte("full api output")}}

	report, err := r.agg.BuildReport(ctx, r.goal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != model.GoalPartial || len(report.Steps) != 1 {
		t.Fatalf("report = %+v", report)
	}
	targets := report.Steps[0].Targets
	if len(targets) != 2 || targets[0].Status != model.RunCompleted || targets[1].Status != model.RunFailed {
		t.Fatalf("targets = %+v", targets)
	}

	var buf bytes.Buffer
	if err := r.agg.Export(ctx, r.goal.ID, &buf); err != nil {
		t.Fatal(err)
	}
	var exported map[string]any
	if err := json.Unmarshal(buf.Bytes(), &exported); err != nil {
		t.Fatalf("export not JSON: %v", err)
	}
	outputs, _ := exported["outputs"].([]any)
	if len(outputs) != 1 {
		t.Fatalf("outputs = %v (want only the completed run)", outputs)
	}
	first := outputs[0].(map[string]any)
	if first["output"] != "full api output" || first["source_data_available"] != true {
		t.Fatalf("output entry = %v", first)
	}
}

func TestExportReportsUnavailableSource(t *testing.T) {
	r := newRig(t, 0)
	r.drive("api", model.RunCompleted)
	r.drive("web", model.RunFailed)
	// No resolved output for the completed run: explicit unavailable.
	var buf bytes.Buffer
	if err := r.agg.Export(context.Background(), r.goal.ID, &buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(buf.Bytes(), []byte(`"source_data_available": false`)) {
		t.Fatalf("export lacked unavailable status: %s", buf.String())
	}
}
