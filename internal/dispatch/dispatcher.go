// Package dispatch drives ready steps of in-flight goals: it reads frozen
// targets from the durable store, enforces per-instance serialization,
// gates steps on needs, renders prompts, injects pending notes, creates
// dedicated sessions, and submits runs (DESIGN §5.3).
package dispatch

import (
	"context"

	"github.com/pshickeydev/matchmaker/internal/config"
	"github.com/pshickeydev/matchmaker/internal/crushapi"
	"github.com/pshickeydev/matchmaker/internal/model"
	"github.com/pshickeydev/matchmaker/internal/notes"
	"github.com/pshickeydev/matchmaker/internal/store"
)

const notImplemented = "not implemented"

// Dispatcher is the durable dispatch loop.
type Dispatcher struct{}

// New builds the dispatcher over the store, fleet state source, note
// selection, and Crush client.
func New(st *store.Store, client *crushapi.Client, noteSel *notes.Selector, mm *config.Matchmaker) *Dispatcher {
	panic(notImplemented)
}

// Run loops until ctx is canceled: for each in-flight goal, find steps
// whose needs are satisfied, and drive one run attempt per ready target
// execution.
func (d *Dispatcher) Run(ctx context.Context) error { panic(notImplemented) }

// NeedsVerdict is the outcome of evaluating one step's needs against
// upstream rollups (DESIGN §5.3).
type NeedsVerdict string

const (
	NeedsSatisfied NeedsVerdict = "satisfied"
	NeedsBlocked   NeedsVerdict = "blocked" // upstream still nonterminal
	NeedsSkip      NeedsVerdict = "skip"    // unsatisfiable: skip the step
)

// StepNeedsSatisfied evaluates one step's needs against upstream step
// rollups (DESIGN §5.3): default requires every upstream succeeded;
// accept_partial_needs also accepts partial, giving the downstream
// template each target's latest-attempt status and result reference;
// failed never satisfies. When all upstream steps are terminal and the
// dependency is unsatisfied, the verdict is NeedsSkip and the step and
// its queued attempts transition atomically to skipped so aggregation can
// finish.
func (d *Dispatcher) StepNeedsSatisfied(ctx context.Context, goal model.Goal, step model.Step) (NeedsVerdict, error) {
	panic(notImplemented)
}

// DispatchRun drives one ready run attempt (DESIGN §5.3): demand-start the
// instance if stopped, acquire the instance serialization slot, select
// pending notes after the target's durable cursor within the injection
// byte limit, create a dedicated Crush session and persist its ID,
// persist RunID + rendered-prompt hash + dispatching before submission,
// submit, transition to running, and advance the audience note cursor in
// the same transaction that records accepted prompt submission.
// Matchmaker never uses Crush's queued-prompts endpoint.
func (d *Dispatcher) DispatchRun(ctx context.Context, run model.Run) error { panic(notImplemented) }

// RenderPrompt renders the step template with its documented minimal
// function surface: upstream outputs appear as short excerpts plus run
// handles, with instructions to fetch full results through result_read
// (DESIGN §4.4, §9.5). Selected notes are included in an untrusted
// "notes from other agents" section carrying claimed-sender and goal
// metadata — never presented as trusted operator instructions (DESIGN
// §5.4, §6).
func RenderPrompt(step model.Step, upstream []UpstreamResult, injectedNotes []model.Note) (string, []byte, error) {
	panic(notImplemented)
}

// UpstreamResult is the reference-shaped view of an upstream target's
// latest terminal attempt passed to downstream templates.
type UpstreamResult struct {
	StepID    string
	Project   string
	Status    model.RunStatus
	Excerpt   string
	RunHandle string
}

// AcquireSerialization acquires the one-active-run-per-workspace slot;
// additional runs queue durably in Matchmaker's own store (DESIGN §5.3).
func (d *Dispatcher) AcquireSerialization(ctx context.Context, project string) (release func(), err error) {
	panic(notImplemented)
}

// ScheduleRetry appends a monotonically numbered attempt to the same
// durable target execution and instance, with backoff, per step policy.
// A retry is not created or dispatched until the prior attempt is
// terminal; an unknown attempt requires explicit abandonment first.
func (d *Dispatcher) ScheduleRetry(ctx context.Context, exec model.TargetExecution, cause error) error {
	panic(notImplemented)
}

// NotifyInstanceDemand reports that a queued run targets a stopped
// instance so dispatch can demand-start it through reconciliation.
func (d *Dispatcher) NotifyInstanceDemand(project string) { panic(notImplemented) }
