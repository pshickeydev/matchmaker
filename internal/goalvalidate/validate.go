// Package goalvalidate implements atomic goal submission validation
// (DESIGN §5.2). A submission is accepted only after the complete goal
// passes validation against one immutable fleet snapshot; any single
// error rejects the entire submission, persists nothing, and dispatches
// nothing.
package goalvalidate

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/template"
	"time"

	"github.com/pshickeydev/matchmaker/internal/config"
	"github.com/pshickeydev/matchmaker/internal/model"
)

// DefaultStepTimeout fills an omitted step timeout (DESIGN §5.2:
// timeout must be present or defaultable).
const DefaultStepTimeout = 10 * time.Minute

// ValidationError is one detected error with step context (DESIGN §5.2:
// validation returns all detected errors, not just the first).
type ValidationError struct {
	StepID string
	Field  string
	Err    string
}

// Error renders one validation error with its step context.
func (e ValidationError) Error() string {
	if e.StepID == "" {
		return e.Field + ": " + e.Err
	}
	return "step " + e.StepID + " " + e.Field + ": " + e.Err
}

// ValidateDraft checks a goal draft (operator-authored or planner-extracted)
// against the fleet snapshot and limits, returning every detected error.
// Checks per DESIGN §5.2:
//   - step IDs present and unique; every needs reference names another step
//   - the dependency graph is acyclic
//   - explicit projects exist, tag expressions are valid, every target
//     resolves to at least one instance in the snapshot
//   - prompts parse as Go text/template with only the documented minimal
//     function surface, and render against validation data
//   - timeout/retries present or defaultable and within configured ranges;
//     supervision recognized; session reuse or pinned-session fields rejected
//   - limits: total steps, deps per step, prompt bytes, resolved targets per
//     step, total expanded runs (fan-out consumes the same limits)
//
// Target expansion is returned frozen so the caller can persist it in the
// acceptance transaction.
func ValidateDraft(draft GoalDraft, snapshot *config.FleetSnapshot, limits config.GoalLimits) ([]ValidationError, map[string][]model.ResolvedTarget) {
	var all []ValidationError
	all = append(all, ValidateDAG(draft.Steps)...)
	all = append(all, ValidatePolicies(draft.Steps, limits)...)
	all = append(all, ValidateTemplates(draft.Steps)...)
	frozen, expandErrs := ExpandTargets(draft.Steps, snapshot, limits)
	all = append(all, expandErrs...)
	all = append(all, checkGoalLimits(draft, limits)...)
	if len(all) > 0 {
		return all, nil
	}
	return nil, frozen
}

// checkGoalLimits enforces goal-wide and per-step submission limits
// (§5.2). Per-step limit errors carry step context like all others.
func checkGoalLimits(draft GoalDraft, limits config.GoalLimits) []ValidationError {
	if len(draft.Steps) == 0 {
		return []ValidationError{{Field: "steps", Err: "goal has no steps"}}
	}
	if len(draft.Steps) > limits.MaxSteps {
		return []ValidationError{{Field: "steps", Err: fmt.Sprintf("%d steps exceed max_steps %d", len(draft.Steps), limits.MaxSteps)}}
	}
	var errs []ValidationError
	for _, step := range draft.Steps {
		if step.ID == "" {
			continue
		}
		if len(step.Needs) > limits.MaxDepsPerStep {
			errs = append(errs, ValidationError{
				StepID: step.ID, Field: "needs",
				Err: fmt.Sprintf("%d dependencies exceed max_deps_per_step %d", len(step.Needs), limits.MaxDepsPerStep),
			})
		}
		if len(step.PromptTemplate) > limits.MaxPromptBytes {
			errs = append(errs, ValidationError{
				StepID: step.ID, Field: "prompt",
				Err: fmt.Sprintf("%d bytes exceed max_prompt_bytes %d", len(step.PromptTemplate), limits.MaxPromptBytes),
			})
		}
	}
	return errs
}

// GoalDraft is a not-yet-persisted goal: steps in submission schema plus
// metadata. It may be operator-authored or attacker-adjacent planner
// output (DESIGN §5.7) and is never trusted.
type GoalDraft struct {
	Objective string
	Steps     []model.Step
}

// ValidateDAG checks ID presence, uniqueness, needs references, acyclicity.
func ValidateDAG(steps []model.Step) []ValidationError {
	var errs []ValidationError
	ids := make(map[string]bool, len(steps))
	for _, step := range steps {
		if step.ID == "" {
			errs = append(errs, ValidationError{Field: "id", Err: "step id is empty"})
			continue
		}
		if ids[step.ID] {
			errs = append(errs, ValidationError{StepID: step.ID, Field: "id", Err: "duplicate step id"})
		}
		ids[step.ID] = true
	}
	for _, step := range steps {
		if step.ID == "" {
			continue
		}
		seen := make(map[string]bool)
		for _, need := range step.Needs {
			if need == step.ID {
				errs = append(errs, ValidationError{StepID: step.ID, Field: "needs", Err: "step depends on itself"})
				continue
			}
			if !ids[need] {
				errs = append(errs, ValidationError{StepID: step.ID, Field: "needs", Err: fmt.Sprintf("unknown step %q", need)})
				continue
			}
			if seen[need] {
				errs = append(errs, ValidationError{StepID: step.ID, Field: "needs", Err: fmt.Sprintf("duplicate dependency %q", need)})
			}
			seen[need] = true
		}
	}
	errs = append(errs, checkAcyclic(steps)...)
	return errs
}

// checkAcyclic reports every step that participates in a dependency cycle.
func checkAcyclic(steps []model.Step) []ValidationError {
	needs := make(map[string][]string, len(steps))
	for _, step := range steps {
		needs[step.ID] = step.Needs
	}
	var errs []ValidationError
	reported := make(map[string]bool)
	for _, step := range steps {
		if !reaches(needs, step.ID, step.ID, make(map[string]bool)) {
			continue
		}
		if !reported[step.ID] {
			errs = append(errs, ValidationError{StepID: step.ID, Field: "needs", Err: "dependency cycle"})
			reported[step.ID] = true
		}
	}
	return errs
}

// reaches reports whether target is reachable from start by following needs.
func reaches(needs map[string][]string, start, target string, visited map[string]bool) bool {
	if visited[start] {
		return false
	}
	visited[start] = true
	for _, need := range needs[start] {
		if need == target {
			return true
		}
		if reaches(needs, need, target, visited) {
			return true
		}
	}
	return false
}

// ValidateTemplates parses and render-checks each step prompt against
// validation data. Only the documented minimal template function surface
// is permitted.
func ValidateTemplates(steps []model.Step) []ValidationError {
	var errs []ValidationError
	for _, step := range steps {
		parsed, err := template.New(step.ID).Parse(step.PromptTemplate)
		if err != nil {
			errs = append(errs, ValidationError{StepID: step.ID, Field: "prompt", Err: "template parse: " + err.Error()})
			continue
		}
		if err := parsed.Execute(&strings.Builder{}, validationData()); err != nil {
			errs = append(errs, ValidationError{StepID: step.ID, Field: "prompt", Err: "template render: " + err.Error()})
		}
	}
	return errs
}

// validationData is the synthetic render target: the same field surface
// dispatch renders against, so missing-field references fail here
// (DESIGN §5.2).
func validationData() any {
	return struct {
		Objective string
		Upstream  []upstreamView
		Notes     []model.Note
	}{
		Objective: "validation objective",
		Upstream: []upstreamView{{
			StepID:    "upstream",
			Project:   "api",
			Status:    model.RunCompleted,
			Excerpt:   "validation excerpt",
			RunHandle: "validation-handle",
		}},
		Notes: []model.Note{{
			NoteID: 1, GoalID: "validation", From: "api",
			To: model.Audience{Project: "web"}, Body: "validation note",
		}},
	}
}

// upstreamView mirrors dispatch.UpstreamResult's field surface so both
// validation and dispatch render against the same template data shape.
type upstreamView struct {
	StepID    string
	Project   string
	Status    model.RunStatus
	Excerpt   string
	RunHandle string
}

// ExpandTargets resolves each step's TargetSpec against the snapshot,
// enforcing per-step and total-run limits.
func ExpandTargets(steps []model.Step, snapshot *config.FleetSnapshot, limits config.GoalLimits) (map[string][]model.ResolvedTarget, []ValidationError) {
	frozen := make(map[string][]model.ResolvedTarget, len(steps))
	var errs []ValidationError
	total := 0
	for _, step := range steps {
		if step.ID == "" {
			continue
		}
		targets, err := snapshot.ResolveTarget(step.Target)
		if err != nil {
			errs = append(errs, ValidationError{StepID: step.ID, Field: "target", Err: err.Error()})
			continue
		}
		for i := range targets {
			targets[i].StepID = step.ID
		}
		if len(targets) > limits.MaxTargetsPerStep {
			errs = append(errs, ValidationError{
				StepID: step.ID, Field: "target",
				Err: fmt.Sprintf("%d resolved targets exceed max_targets_per_step %d", len(targets), limits.MaxTargetsPerStep),
			})
			continue
		}
		total += len(targets)
		frozen[step.ID] = targets
	}
	if total > limits.MaxTotalRunsPerGoal {
		errs = append(errs, ValidationError{
			Field: "target",
			Err:   fmt.Sprintf("%d expanded runs exceed max_total_runs_per_goal %d", total, limits.MaxTotalRunsPerGoal),
		})
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return frozen, nil
}

// ValidatePolicies checks timeout/retry ranges and recognized supervision
// (rejecting the reserved ask policy and any session reuse fields).
func ValidatePolicies(steps []model.Step, limits config.GoalLimits) []ValidationError {
	var errs []ValidationError
	for _, step := range steps {
		if step.ID == "" {
			continue
		}
		if step.Timeout == 0 {
			step.Timeout = DefaultStepTimeout
		}
		if step.Timeout < limits.MinTimeout || step.Timeout > limits.MaxTimeout {
			errs = append(errs, ValidationError{
				StepID: step.ID, Field: "timeout",
				Err: fmt.Sprintf("%s outside [%s, %s]", step.Timeout, limits.MinTimeout, limits.MaxTimeout),
			})
		}
		if step.Retries < 0 || step.Retries > limits.MaxRetries {
			errs = append(errs, ValidationError{
				StepID: step.ID, Field: "retries",
				Err: fmt.Sprintf("%d exceeds max_retries %d", step.Retries, limits.MaxRetries),
			})
		}
		if step.Supervision == model.SupervisionAsk {
			errs = append(errs, ValidationError{
				StepID: step.ID, Field: "supervision",
				Err: "ask is reserved for future work and rejected in v1",
			})
			continue
		}
		if step.Supervision != "" && !model.SupervisionPolicyRecognized(step.Supervision) {
			errs = append(errs, ValidationError{
				StepID: step.ID, Field: "supervision",
				Err: fmt.Sprintf("unrecognized supervision %q", step.Supervision),
			})
		}
	}
	return errs
}

// submission is the wire schema of a goal file (DESIGN §4.4, §5.7).
type submission struct {
	Objective string       `json:"objective"`
	Steps     []stepSchema `json:"steps"`
}

// stepSchema is one step in the submission schema. Unknown fields are
// rejected so any session reuse or pinned-session field fails (v1).
type stepSchema struct {
	ID                 string       `json:"id"`
	Prompt             string       `json:"prompt"`
	Target             targetSchema `json:"target"`
	Needs              []string     `json:"needs"`
	AcceptPartialNeeds bool         `json:"accept_partial_needs"`
	Supervision        string       `json:"supervision"`
	Timeout            string       `json:"timeout"`
	Retries            int          `json:"retries"`
}

// targetSchema addresses work to explicit names, a tag expression, or all.
type targetSchema struct {
	Explicit []string `json:"explicit"`
	Tag      string   `json:"tag"`
	All      bool     `json:"all"`
}

// ParseSubmission parses the JSON submission schema into a draft (DESIGN
// §4.4, §5.7): the same schema operator-authored goal files and planner
// draft files use. Any session reuse or pinned-session field is rejected
// in v1.
func ParseSubmission(data []byte) (GoalDraft, error) {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var sub submission
	if err := decoder.Decode(&sub); err != nil {
		return GoalDraft{}, fmt.Errorf("submission schema: %w", err)
	}
	if strings.TrimSpace(sub.Objective) == "" {
		return GoalDraft{}, errors.New("submission schema: objective is required")
	}
	if len(sub.Steps) == 0 {
		return GoalDraft{}, errors.New("submission schema: at least one step is required")
	}
	steps := make([]model.Step, 0, len(sub.Steps))
	for i, schema := range sub.Steps {
		step, err := buildStep(schema)
		if err != nil {
			return GoalDraft{}, fmt.Errorf("submission schema: steps[%d]: %w", i, err)
		}
		steps = append(steps, step)
	}
	return GoalDraft{Objective: sub.Objective, Steps: steps}, nil
}

// buildStep converts one schema step into a model step, applying the
// documented defaults (timeout, supervision).
func buildStep(schema stepSchema) (model.Step, error) {
	step := model.Step{
		ID:                 schema.ID,
		PromptTemplate:     schema.Prompt,
		Target:             model.TargetSpec{Explicit: schema.Target.Explicit, TagExpr: schema.Target.Tag, All: schema.Target.All},
		Needs:              schema.Needs,
		AcceptPartialNeeds: schema.AcceptPartialNeeds,
		Supervision:        model.SupervisionPolicy(schema.Supervision),
		Retries:            schema.Retries,
	}
	if schema.Timeout != "" {
		timeout, err := time.ParseDuration(schema.Timeout)
		if err != nil {
			return model.Step{}, fmt.Errorf("timeout %q: %w", schema.Timeout, err)
		}
		step.Timeout = timeout
	}
	if step.Timeout == 0 {
		step.Timeout = DefaultStepTimeout
	}
	if step.Supervision == "" {
		step.Supervision = model.SupervisionDeny
	}
	return step, nil
}

// AllowedTemplateFuncs returns the documented minimal template function
// surface (DESIGN §4.4, §5.2, plan Decision 4). Validation and dispatch
// rendering must use exactly this set; templates referencing any other
// function are invalid.
func AllowedTemplateFuncs() []string {
	return []string{
		"and", "call", "html", "index", "slice", "js", "len", "not", "or",
		"print", "printf", "println", "urlquery", "eq", "ge", "gt", "le", "lt", "ne",
	}
}
