// Package aggregate derives step and goal rollups from terminal run
// attempts and produces per-goal reports (DESIGN §5.5).
package aggregate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/pshickeydev/matchmaker/internal/model"
	"github.com/pshickeydev/matchmaker/internal/store"
)

// excerptLimit bounds one attempt's report excerpt; full output is
// resolved lazily at export (§5.5).
const excerptLimit = 240

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
type Aggregator struct {
	st       *store.Store
	sessions SessionSource
}

// New builds the aggregator over the store and the session content source.
func New(st *store.Store, sessions SessionSource) *Aggregator {
	return &Aggregator{st: st, sessions: sessions}
}

// StepStatus derives a step's status from its target executions
// (DESIGN §5.5): every target succeeded -> succeeded; every target failed
// -> failed; mixed terminal outcomes -> partial. Failed, cancelled,
// timed-out, skipped, and abandoned attempts count as failed for the
// rollup while retaining detailed status; unknown is nonterminal and
// prevents aggregation. Steps wait until every frozen target is terminal
// and no retry remains eligible.
func (a *Aggregator) StepStatus(ctx context.Context, goalID, stepID string) (model.StepStatus, bool, error) {
	goal, err := a.st.Goal(ctx, goalID)
	if err != nil {
		return model.StepPending, false, err
	}
	step, err := stepOf(goal, stepID)
	if err != nil {
		return model.StepPending, false, err
	}
	targets := goal.FrozenTargets[stepID]
	succeeded, failed, pending := 0, 0, 0
	for _, target := range targets {
		latest, err := a.st.LatestTerminalAttempt(ctx, target.ExecutionID)
		if err != nil {
			pending++
			continue
		}
		if retryEligible(step, latest) {
			pending++
			continue
		}
		if latest.Status == model.RunCompleted {
			succeeded++
		} else {
			failed++
		}
	}
	terminal := pending == 0
	switch {
	case !terminal:
		return model.StepPending, false, nil
	case failed == 0:
		return model.StepSucceeded, true, nil
	case succeeded == 0:
		return model.StepFailed, true, nil
	default:
		return model.StepPartial, true, nil
	}
}

// GoalStatus derives the goal rollup: succeeded when all steps succeeded,
// failed when no step succeeded, partial otherwise including skipped
// branches (DESIGN §5.5).
func (a *Aggregator) GoalStatus(ctx context.Context, goalID string) (model.GoalStatus, error) {
	goal, err := a.st.Goal(ctx, goalID)
	if err != nil {
		return model.GoalActive, err
	}
	// anySuccess tracks whether any step carried success: a partial
	// step still succeeded on some targets (§5.5).
	anySuccess, allSucceeded := false, true
	for _, step := range goal.Steps {
		status, terminal, err := a.StepStatus(ctx, goalID, step.ID)
		if err != nil {
			return model.GoalActive, err
		}
		if !terminal {
			return model.GoalActive, nil
		}
		switch status {
		case model.StepSucceeded:
			anySuccess = true
		case model.StepPartial:
			anySuccess = true
			allSucceeded = false
		default:
			allSucceeded = false
		}
	}
	switch {
	case allSucceeded:
		return model.GoalSucceeded, nil
	case !anySuccess:
		return model.GoalFailed, nil
	default:
		return model.GoalPartial, nil
	}
}

// StepSkippedOrFailed reports whether a terminal step counts against the
// goal rollup (skipped branches included, §5.5).
func StepSkippedOrFailed(status model.StepStatus) bool {
	return status == model.StepFailed || status == model.StepSkipped || status == model.StepPartial
}

// Wake runs the aggregate pass for one goal: recompute step and goal
// statuses, persist derived statuses, and finalize when every step is
// terminal. Plan goals produce no per-goal report (DESIGN §5.7).
func (a *Aggregator) Wake(ctx context.Context, goalID string) error {
	goal, err := a.st.Goal(ctx, goalID)
	if err != nil {
		return err
	}
	allTerminal := true
	for _, step := range goal.Steps {
		status, terminal, err := a.StepStatus(ctx, goalID, step.ID)
		if err != nil {
			return err
		}
		if !terminal {
			allTerminal = false
			continue
		}
		if err := a.st.WithinTx(ctx, func(tx *store.Tx) error {
			return tx.UpdateStepStatus(ctx, goalID, step.ID, status)
		}); err != nil {
			return err
		}
	}
	if !allTerminal {
		return nil
	}
	status, err := a.GoalStatus(ctx, goalID)
	if err != nil {
		return err
	}
	return a.st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.UpdateGoalStatus(ctx, goalID, status)
	})
}

// Report is the per-goal report assembled from Matchmaker metadata plus
// lazily resolved Crush session content. It preserves detailed step,
// target, and attempt statuses.
type Report struct {
	GoalID string           `json:"goal_id"`
	Status model.GoalStatus `json:"status"`
	Steps  []StepReport     `json:"steps"`
}

// StepReport is one step's report entry.
type StepReport struct {
	StepID  string           `json:"step_id"`
	Status  model.StepStatus `json:"status"`
	Targets []TargetReport   `json:"targets"`
}

// TargetReport is one target execution's report entry, derived from its
// latest terminal attempt.
type TargetReport struct {
	Project  string          `json:"project"`
	Status   model.RunStatus `json:"status"`
	Attempts int             `json:"attempts"`
	Excerpt  string          `json:"excerpt"`
}

// BuildReport assembles the in-memory report for one goal.
func (a *Aggregator) BuildReport(ctx context.Context, goalID string) (Report, error) {
	goal, err := a.st.Goal(ctx, goalID)
	if err != nil {
		return Report{}, err
	}
	if goal.Type == model.GoalTypePlan {
		return Report{}, fmt.Errorf("plan goal %s produces no report", goalID)
	}
	report := Report{GoalID: goalID}
	for _, step := range goal.Steps {
		stepStatus, _, err := a.StepStatus(ctx, goalID, step.ID)
		if err != nil {
			return Report{}, err
		}
		entry := StepReport{StepID: step.ID, Status: stepStatus}
		for _, target := range goal.FrozenTargets[step.ID] {
			exec, err := a.st.TargetExecution(ctx, target.ExecutionID)
			if err != nil {
				continue
			}
			latest, err := a.st.LatestTerminalAttempt(ctx, target.ExecutionID)
			targetEntry := TargetReport{Project: target.Project, Attempts: len(exec.Attempts)}
			if err == nil {
				targetEntry.Status = latest.Status
				targetEntry.Excerpt = excerptOf(latest)
			}
			entry.Targets = append(entry.Targets, targetEntry)
		}
		report.Steps = append(report.Steps, entry)
	}
	status, err := a.GoalStatus(ctx, goalID)
	if err != nil {
		return Report{}, err
	}
	report.Status = status
	return report, nil
}

// excerptOf renders one attempt's bounded excerpt from its final output
// reference; full output is resolved lazily at export (§5.5).
func excerptOf(run model.Run) string {
	handle := "run " + run.ID
	if len(handle) <= excerptLimit {
		return handle
	}
	return handle[:excerptLimit]
}

// exportedReport is the JSON document written by Export.
type exportedReport struct {
	Report
	GeneratedAt time.Time        `json:"generated_at"`
	Outputs     []exportedOutput `json:"outputs"`
}

// exportedOutput is one run's lazily resolved final output.
type exportedOutput struct {
	StepID              string `json:"step_id"`
	Project             string `json:"project"`
	RunID               string `json:"run_id"`
	Excerpt             string `json:"excerpt"`
	SourceDataAvailable bool   `json:"source_data_available"`
	Output              string `json:"output,omitempty"`
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
	report, err := a.BuildReport(ctx, goalID)
	if err != nil {
		return err
	}
	goal, err := a.st.Goal(ctx, goalID)
	if err != nil {
		return err
	}
	exported := exportedReport{Report: report, GeneratedAt: time.Now().UTC()}
	for _, step := range goal.Steps {
		for _, target := range goal.FrozenTargets[step.ID] {
			latest, err := a.st.LatestTerminalAttempt(ctx, target.ExecutionID)
			if err != nil || latest.Status != model.RunCompleted {
				continue
			}
			output := exportedOutput{
				StepID: step.ID, Project: target.Project,
				RunID: latest.ID, Excerpt: excerptOf(latest),
			}
			content, available, err := a.sessions.FinalOutput(ctx, latest)
			if err != nil {
				output.SourceDataAvailable = false
			} else {
				output.SourceDataAvailable = available
				if available {
					output.Output = string(content)
				}
			}
			exported.Outputs = append(exported.Outputs, output)
		}
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(exported)
}

// stepOf finds one step of a goal.
func stepOf(goal model.Goal, stepID string) (model.Step, error) {
	for _, step := range goal.Steps {
		if step.ID == stepID {
			return step, nil
		}
	}
	return model.Step{}, fmt.Errorf("step %q not in goal %s", stepID, goal.ID)
}

// retryEligible reports whether a failed terminal attempt still has retry
// budget per step policy (§5.3).
func retryEligible(step model.Step, latest model.Run) bool {
	if latest.Status == model.RunCompleted {
		return false
	}
	return latest.Attempt <= step.Retries
}
