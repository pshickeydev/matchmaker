// Package planner implements planning runs: turning a natural-language
// objective into a reviewable goal DAG through an ordinary Crush run on
// an operator-chosen fleet instance (DESIGN §5.7). Matchmaker embeds no
// LLM; planner drafts are attacker-adjacent and never trusted.
package planner

import (
	"time"

	"github.com/pshickeydev/matchmaker/internal/config"
	"github.com/pshickeydev/matchmaker/internal/goalvalidate"
	"github.com/pshickeydev/matchmaker/internal/model"
)

const notImplemented = "not implemented"

// Request is a plan request over the local RPC: the objective, the
// explicitly named planner project (no default, no semantic selection),
// and the supervision choice for the planner run (default deny).
type Request struct {
	Objective   string
	On          string
	Supervision model.SupervisionPolicy
	AutoSubmit  bool
}

// ValidateRequest atomically validates before the plan goal is persisted:
// the project must exist in the fleet snapshot, the supervision choice
// must be recognized, and the objective must be within configured bounds
// (DESIGN §5.7). Rejections return explicit errors without partial
// persistence.
func ValidateRequest(req Request, snapshot *config.FleetSnapshot, limits config.PlannerConfig) error {
	panic(notImplemented)
}

// BuildGoal turns an accepted request into the internal plan-type goal: a
// single step with one explicit target, a Matchmaker-owned prompt
// template, a configured timeout, and a retry policy like any step. From
// there it is an ordinary goal reusing dispatch, serialization,
// supervision, timeout, recovery, and audit unchanged (DESIGN §5.7).
func BuildGoal(req Request, snapshot *config.FleetSnapshot, limits config.PlannerConfig) (PlanGoal, error) {
	panic(notImplemented)
}

// PlanGoal is the constructed planning goal plus request metadata.
type PlanGoal struct {
	Objective string
	Prompt    string
}

// RenderPlannerPrompt builds the planner prompt from the objective plus a
// read-only fleet snapshot (project names, tags) taken when the request
// was accepted. It instructs the planner to return, in its final message,
// one fenced `matchmaker-goal` block containing the complete goal in the
// submission schema. The prompt contains no secrets and no fleet data
// beyond the snapshot (DESIGN §5.7).
func RenderPlannerPrompt(objective string, snapshot *config.FleetSnapshot) string {
	panic(notImplemented)
}

// ExtractDraft reads the planner run's final assistant message and
// extracts the last fenced matchmaker-goal block (DESIGN §5.7). A missing
// or unparseable block is an invalid draft.
func ExtractDraft(finalMessage string) (goalvalidate.GoalDraft, error) { panic(notImplemented) }

// DraftStatus is the pre-validation outcome of an extracted draft,
// offered as operator feedback only; the authoritative validation happens
// at submission against a current fleet snapshot (DESIGN §5.7).
type DraftStatus struct {
	Draft  goalvalidate.GoalDraft
	Errors []goalvalidate.ValidationError
}

// PreValidateDraft runs all §5.2 checks and limits on an extracted draft.
func PreValidateDraft(draft goalvalidate.GoalDraft, snapshot *config.FleetSnapshot, limits config.GoalLimits) DraftStatus {
	panic(notImplemented)
}

// AutoSubmitEligible reports whether a valid draft may be auto-submitted:
// drafts whose steps use grant_all supervision always require review
// (DESIGN §5.7).
func AutoSubmitEligible(status DraftStatus) bool { panic(notImplemented) }

// RenderRevisionPrompt builds the prompt for a revision planning run: the
// planner prompt plus the prior draft and the validation errors appended
// (DESIGN §5.7). Revisions are ordinary planning attempts under the
// configured maximum revision count and total planning budget.
func RenderRevisionPrompt(objective string, snapshot *config.FleetSnapshot, priorDraft goalvalidate.GoalDraft, errors []goalvalidate.ValidationError) string {
	panic(notImplemented)
}

// CompletePlanningRun handles a planner run's completion (DESIGN §5.7):
// extract the matchmaker-goal block from the final assistant message
// (read through the workspace session API, the same path recovery uses),
// enforce the draft byte limit, and pre-validate for operator feedback.
// The authoritative validation happens at submission. A missing or
// unparseable block is an invalid draft. A planning run never persists
// goal state beyond its own audit records and never dispatches work.
func CompletePlanningRun(finalMessage string, snapshot *config.FleetSnapshot, limits config.PlannerConfig, goalLimits config.GoalLimits) (DraftStatus, error) {
	panic(notImplemented)
}

// RevisionAllowed reports whether a revision planning run may be
// dispatched: revision rounds and the total planning budget are bounded;
// once exhausted the operator files a new request (DESIGN §5.7).
func RevisionAllowed(revisionsUsed int, budgetUsed time.Duration, limits config.PlannerConfig) bool {
	panic(notImplemented)
}
