// Package goalvalidate implements atomic goal submission validation
// (DESIGN §5.2). A submission is accepted only after the complete goal
// passes validation against one immutable fleet snapshot; any single
// error rejects the entire submission, persists nothing, and dispatches
// nothing.
package goalvalidate

import (
	"github.com/pshickeydev/matchmaker/internal/config"
	"github.com/pshickeydev/matchmaker/internal/model"
)

const notImplemented = "not implemented"

// ValidationError is one detected error with step context (DESIGN §5.2:
// validation returns all detected errors, not just the first).
type ValidationError struct {
	StepID string
	Field  string
	Err    string
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
	panic(notImplemented)
}

// GoalDraft is a not-yet-persisted goal: steps in submission schema plus
// metadata. It may be operator-authored or attacker-adjacent planner
// output (DESIGN §5.7) and is never trusted.
type GoalDraft struct {
	Objective string
	Steps     []model.Step
}

// ValidateDAG checks ID presence, uniqueness, needs references, acyclicity.
func ValidateDAG(steps []model.Step) []ValidationError { panic(notImplemented) }

// ValidateTemplates parses and render-checks each step prompt against
// validation data. Only the documented minimal template function surface
// is permitted.
func ValidateTemplates(steps []model.Step) []ValidationError { panic(notImplemented) }

// ExpandTargets resolves each step's TargetSpec against the snapshot,
// enforcing per-step and total-run limits.
func ExpandTargets(steps []model.Step, snapshot *config.FleetSnapshot, limits config.GoalLimits) (map[string][]model.ResolvedTarget, []ValidationError) {
	panic(notImplemented)
}

// ValidatePolicies checks timeout/retry ranges and recognized supervision
// (rejecting the reserved ask policy and any session reuse fields).
func ValidatePolicies(steps []model.Step, limits config.GoalLimits) []ValidationError {
	panic(notImplemented)
}

// ParseSubmission parses the JSON submission schema into a draft (DESIGN
// §4.4, §5.7): the same schema operator-authored goal files and planner
// draft files use. Any session reuse or pinned-session field is rejected
// in v1.
func ParseSubmission(data []byte) (GoalDraft, error) { panic(notImplemented) }

// AllowedTemplateFuncs returns the documented minimal template function
// surface (DESIGN §4.4, §5.2). Validation and dispatch rendering must use
// exactly this set; templates referencing any other function are invalid.
func AllowedTemplateFuncs() []string { panic(notImplemented) }
