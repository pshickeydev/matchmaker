// Package supervise consumes each instance's workspace SSE stream and
// handles permission requests, question batches, run completion, stream
// loss, cancellation, and run recovery (DESIGN §5.3).
package supervise

import (
	"context"

	"github.com/pshickeydev/matchmaker/internal/crushapi"
	"github.com/pshickeydev/matchmaker/internal/model"
	"github.com/pshickeydev/matchmaker/internal/store"
)

const notImplemented = "not implemented"

// Supervisor owns one goroutine per open instance stream.
type Supervisor struct{}

// New builds the supervisor over the store and Crush client.
func New(st *store.Store, client *crushapi.Client) *Supervisor { panic(notImplemented) }

// Consume pumps one instance's SSE stream until stream loss, applying the
// event handlers below. It returns when the stream is lost so
// reconciliation can reconnect with backoff (DESIGN §5.3).
func (s *Supervisor) Consume(ctx context.Context, stream *crushapi.EventStream, instance model.Instance) error {
	panic(notImplemented)
}

// handlePermissionRequest correlates a permission_request to exactly one
// persisted running attempt (DESIGN §5.3): the dedicated-session invariant
// binds the event's session ID to one attempt, and workspace and instance
// generation must match the stream. Unknown, stale, mismatched,
// dispatching, cancelling, or post-terminal requests fail closed to deny
// without granting. Permission events carry no RunID, so policy is never
// inferred from workspace activity alone.
func (s *Supervisor) handlePermissionRequest(ctx context.Context, instance model.Instance, ev crushapi.Event) error {
	panic(notImplemented)
}

// applyPolicy records workspace ID, session ID, run ID, instance
// generation, request ID, request type, policy, and decision
// transactionally before responding; raw tool parameters are not
// retained (the grant call echoes the event's request back, but only the
// typed correlation subset is ever stored). If recording fails, deny is
// sent; an unaudited request is never granted. Duplicate request IDs
// reuse the recorded decision. deny rejects and lets the run continue or
// fail naturally; grant_all sends action allow per event, never
// allow_session, permissions/skip, or yolo (C6, C8).
func (s *Supervisor) applyPolicy(ctx context.Context, run model.Run, ev crushapi.Event) error {
	panic(notImplemented)
}

// handleQuestionBatch correlates a question_batch_request by dedicated
// session, persists bounded event metadata (never raw text or choices),
// and immediately calls the workspace-scoped question-cancel endpoint.
// V1 never waits for operator answers (DESIGN §5.3).
func (s *Supervisor) handleQuestionBatch(ctx context.Context, instance model.Instance, ev crushapi.Event) error {
	panic(notImplemented)
}

// handleRunComplete marks the run terminal from the authoritative
// completion event (matched by the echoed RunID), extracts the result
// reference (MessageID), and wakes dependents (DESIGN §5.3). The event's
// Error and Cancelled fields carry the outcome; success is error empty
// and cancelled false.
func (s *Supervisor) handleRunComplete(ctx context.Context, instance model.Instance, ev crushapi.Event) error {
	panic(notImplemented)
}

// BeginCancellation starts cancellation rather than completing it
// (DESIGN §5.3): atomically transition to cancelling, persist the cause,
// keep the serialization slot occupied, stop new dispatch, and send the
// session cancel once. While cancelling, only a matching run_complete
// establishes the outcome: cancelled=true becomes timed_out for cause
// timeout and cancelled for cause operator; a normal completion records
// its actual status. Session idle alone is not terminal evidence.
func (s *Supervisor) BeginCancellation(ctx context.Context, runID string, cause model.CancelCause) error {
	panic(notImplemented)
}

// MonitorCancellations enforces the cancellation grace: if a cancelling
// run sees no matching terminal event within the grace period, drain and
// restart the instance before releasing serialization, then reconcile
// the run from its durable session references; an unknowable outcome
// stays nonterminal unknown until evidence or explicit abandonment
// (DESIGN §5.3, §7).
func (s *Supervisor) MonitorCancellations(ctx context.Context) error { panic(notImplemented) }

// ReconcileUnknownRun inspects the attempt's dedicated session through
// the workspace session API (DESIGN §5.3): a persisted matching user
// message proves prompt acceptance and a busy session returns the
// attempt to running; absence of that message does not prove
// non-acceptance. Only a matching live run_complete proves terminal
// status; lost events leave the attempt unknown, and Matchmaker never
// automatically resubmits it.
func (s *Supervisor) ReconcileUnknownRun(ctx context.Context, run model.Run) error {
	panic(notImplemented)
}

// OnStreamLoss handles a lost stream (DESIGN §5.3): mark in-flight runs
// unknown, let reconciliation recreate and re-attach the workspace if it
// was torn down (C1), and reconcile runs from persisted session IDs
// without ambiguous resubmission.
func (s *Supervisor) OnStreamLoss(ctx context.Context, instance model.Instance) error {
	panic(notImplemented)
}

// Abandon performs the explicit operator action that accepts an
// unresolved outcome: transition to abandoned and release any held
// serialization (DESIGN §4.5). Retrying an unknown attempt abandons it
// first.
func (s *Supervisor) Abandon(ctx context.Context, runID string) error { panic(notImplemented) }
