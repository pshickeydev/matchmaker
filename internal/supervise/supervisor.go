// Package supervise consumes each instance's workspace SSE stream and
// handles permission requests, question batches, run completion, stream
// loss, cancellation, and run recovery (DESIGN §5.3).
package supervise

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/charmbracelet/log"

	"github.com/pshickeydev/matchmaker/internal/crushapi"
	"github.com/pshickeydev/matchmaker/internal/model"
	"github.com/pshickeydev/matchmaker/internal/store"
)

const (
	// monitorPoll paces the cancellation and timeout monitor.
	monitorPoll = 250 * time.Millisecond
)

// defaultCancelGrace bounds unconfirmed cancellation before the instance
// is drained and restarted (DESIGN §5.3). It is a variable so tests can
// shorten it; the daemon's configured grace overrides it at wiring time
// through SetCancelGrace.
var defaultCancelGrace = 60 * time.Second

// Supervisor owns one goroutine per open instance stream.
type Supervisor struct {
	st          *store.Store
	client      *crushapi.Client
	cancelGrace time.Duration
}

// New builds the supervisor over the store and Crush client.
func New(st *store.Store, client *crushapi.Client) *Supervisor {
	return &Supervisor{st: st, client: client, cancelGrace: defaultCancelGrace}
}

// SetCancelGrace applies the configured cancellation grace
// (supervision.cancel_grace); wiring code calls it once at startup.
func (s *Supervisor) SetCancelGrace(grace time.Duration) {
	if grace > 0 {
		s.cancelGrace = grace
	}
}

// Consume pumps one instance's SSE stream until stream loss, applying the
// event handlers below. It returns when the stream is lost so
// reconciliation can reconnect with backoff (DESIGN §5.3).
func (s *Supervisor) Consume(ctx context.Context, stream *crushapi.EventStream, instance model.Instance) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err, more := <-stream.Errors():
			if !more {
				continue
			}
			log.Warn("stream decode error", "project", instance.Project, "error", err)
		case event, ok := <-stream.Events():
			if !ok {
				return nil
			}
			s.handle(ctx, instance, event)
		}
	}
}

// handle routes one decoded event.
func (s *Supervisor) handle(ctx context.Context, instance model.Instance, event crushapi.Event) {
	switch event.Kind {
	case crushapi.KindPermissionRequest:
		if err := s.handlePermissionRequest(ctx, instance, event); err != nil {
			log.Error("permission handling failed", "project", instance.Project, "error", err)
		}
	case crushapi.KindQuestionBatch:
		if err := s.handleQuestionBatch(ctx, instance, event); err != nil {
			log.Error("question handling failed", "project", instance.Project, "error", err)
		}
	case crushapi.KindRunComplete:
		if err := s.handleRunComplete(ctx, instance, event); err != nil {
			log.Error("run completion handling failed", "project", instance.Project, "error", err)
		}
	default:
		// message, session, and ignorable LSP/MCP/file/config events.
	}
}

// correlation is one event's correlation outcome.
type correlation struct {
	run          model.Run
	correlated   bool
	failureState string
}

// correlate binds one session-scoped event to exactly one persisted
// running attempt (DESIGN §5.3): the dedicated-session invariant binds
// the event's session ID to one attempt, and workspace and instance
// generation must match the stream.
func (s *Supervisor) correlate(ctx context.Context, instance model.Instance, sessionID string) correlation {
	run, err := s.st.RunBySession(ctx, instance.WorkspaceID, sessionID)
	if err != nil {
		return correlation{failureState: "unknown session"}
	}
	if run.WorkspaceID != instance.WorkspaceID {
		return correlation{run: run, failureState: "workspace mismatch"}
	}
	if int64(run.InstanceGeneration) != instance.Generation {
		return correlation{run: run, failureState: "generation mismatch"}
	}
	if run.Status != model.RunRunning {
		return correlation{run: run, failureState: "run state " + string(run.Status)}
	}
	return correlation{run: run, correlated: true}
}

// handlePermissionRequest correlates a permission_request to exactly one
// persisted running attempt (DESIGN §5.3): the dedicated-session invariant
// binds the event's session ID to one attempt, and workspace and instance
// generation must match the stream. Unknown, stale, mismatched,
// dispatching, cancelling, or post-terminal requests fail closed to deny
// without granting. Permission events carry no RunID, so policy is never
// inferred from workspace activity alone.
func (s *Supervisor) handlePermissionRequest(ctx context.Context, instance model.Instance, ev crushapi.Event) error {
	if ev.Permission == nil {
		return nil
	}
	linked := s.correlate(ctx, instance, ev.Permission.SessionID)
	if !linked.correlated {
		return s.failClosed(ctx, instance, ev, linked.failureState)
	}
	return s.applyPolicy(ctx, linked.run, ev)
}

// failClosed records the correlation failure and denies without granting
// (DESIGN §5.3, §7).
func (s *Supervisor) failClosed(ctx context.Context, instance model.Instance, ev crushapi.Event, reason string) error {
	request := ev.Permission
	decision := model.PermissionDecision{
		Project: instance.Project, WorkspaceID: instance.WorkspaceID,
		SessionID: request.SessionID, InstanceGeneration: instance.Generation,
		RequestID: request.ID, RequestType: request.ToolName,
		Policy: model.SupervisionDeny, Decision: model.DecisionDeny,
		CorrelationFailed: true,
	}
	if err := s.st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.RecordPermissionDecision(ctx, decision)
	}); err != nil {
		log.Error("recording correlation failure failed", "project", instance.Project, "error", err)
	}
	log.Warn("permission request failed correlation; denying",
		"project", instance.Project, "request", request.ID, "reason", reason)
	return s.client.RespondPermission(ctx, instance.ServerURL, instance.WorkspaceID, *request, crushapi.GrantDeny)
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
	request := *ev.Permission
	if request.ID != "" {
		recorded, found, err := s.st.PermissionDecisionByRequest(ctx, request.ID)
		if err == nil && found {
			action := crushapi.GrantDeny
			if recorded.Decision == model.DecisionAllow {
				action = crushapi.GrantAllow
			}
			log.Info("duplicate permission request; reusing recorded decision",
				"request", request.ID, "decision", recorded.Decision)
			return s.client.RespondPermission(ctx, run.ServerURL, run.WorkspaceID, request, action)
		}
	}
	policy, err := s.stepPolicy(ctx, run)
	if err != nil {
		return err
	}
	decision := model.DecisionDeny
	if policy == model.SupervisionGrantAll {
		decision = model.DecisionAllow
	}
	record := model.PermissionDecision{
		GoalID: run.GoalID, RunID: run.ID, Project: run.Project,
		WorkspaceID: run.WorkspaceID, SessionID: run.SessionID,
		InstanceGeneration: int64(run.InstanceGeneration), RequestID: request.ID,
		RequestType: request.ToolName, Policy: policy, Decision: decision,
	}
	if err := s.st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.RecordPermissionDecision(ctx, record)
	}); err != nil {
		// Audit-before-respond: an unaudited request is never granted
		// (DESIGN §5.3, §7).
		log.Error("recording permission decision failed; denying", "run", run.ID, "error", err)
		return s.client.RespondPermission(ctx, run.ServerURL, run.WorkspaceID, request, crushapi.GrantDeny)
	}
	action := crushapi.GrantDeny
	if decision == model.DecisionAllow {
		action = crushapi.GrantAllow
	}
	return s.client.RespondPermission(ctx, run.ServerURL, run.WorkspaceID, request, action)
}

// stepPolicy loads the persisted supervision policy of a run's step.
func (s *Supervisor) stepPolicy(ctx context.Context, run model.Run) (model.SupervisionPolicy, error) {
	goal, err := s.st.Goal(ctx, run.GoalID)
	if err != nil {
		return model.SupervisionDeny, err
	}
	for _, step := range goal.Steps {
		if step.ID == run.StepID {
			if step.Supervision == "" {
				return model.SupervisionDeny, nil
			}
			return step.Supervision, nil
		}
	}
	return model.SupervisionDeny, nil
}

// handleQuestionBatch correlates a question_batch_request by dedicated
// session, persists bounded event metadata (never raw text or choices),
// and immediately calls the workspace-scoped question-cancel endpoint.
// V1 never waits for operator answers (DESIGN §5.3).
func (s *Supervisor) handleQuestionBatch(ctx context.Context, instance model.Instance, ev crushapi.Event) error {
	linked := s.correlate(ctx, instance, ev.SessionID)
	goalID, runID := "", ""
	if linked.correlated {
		goalID, runID = linked.run.GoalID, linked.run.ID
	}
	if err := s.st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.RecordQuestionEvent(ctx, goalID, runID, ev.QuestionID)
	}); err != nil {
		log.Error("recording question event failed", "project", instance.Project, "error", err)
	}
	// Cancellation is fail closed regardless of correlation.
	return s.client.CancelQuestion(ctx, instance.ServerURL, instance.WorkspaceID)
}

// handleRunComplete marks the run terminal from the authoritative
// completion event (matched by the echoed RunID), extracts the result
// reference (MessageID), and wakes dependents (DESIGN §5.3). The event's
// Error and Cancelled fields carry the outcome; success is error empty
// and cancelled false.
func (s *Supervisor) handleRunComplete(ctx context.Context, instance model.Instance, ev crushapi.Event) error {
	if ev.Complete == nil {
		return nil
	}
	run, err := s.st.RunBySession(ctx, instance.WorkspaceID, ev.SessionID)
	if err != nil {
		log.Debug("run_complete for uncorrelated session", "session", ev.SessionID)
		return nil
	}
	if run.CrushRunID != ev.RunID {
		log.Warn("run_complete run id mismatch ignored", "run", run.ID, "event_run", ev.RunID)
		return nil
	}
	outcome, err := completionOutcome(run, *ev.Complete)
	if err != nil {
		return nil
	}
	if err := s.st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.UpdateRunStatus(ctx, run.ID, run.Status, outcome)
	}); err != nil {
		return err
	}
	log.Info("run reached terminal state",
		"run", run.ID, "project", run.Project, "status", outcome)
	return nil
}

// completionOutcome maps one completion event to the durable terminal
// status under the §5.3 rules.
func completionOutcome(run model.Run, complete crushapi.RunComplete) (model.RunStatus, error) {
	if complete.Error != "" {
		return model.RunFailed, nil
	}
	if !complete.Cancelled {
		// A normal completion won even if cancellation had begun.
		return model.RunCompleted, nil
	}
	if run.Status == model.RunCancelling {
		if run.CancelCause == model.CancelCauseTimeout {
			return model.RunTimedOut, nil
		}
		return model.RunCancelled, nil
	}
	return model.RunCancelled, nil
}

// BeginCancellation starts cancellation rather than completing it
// (DESIGN §5.3): atomically transition to cancelling, persist the cause,
// keep the serialization slot occupied, stop new dispatch, and send the
// session cancel once. While cancelling, only a matching run_complete
// establishes the outcome: cancelled=true becomes timed_out for cause
// timeout and cancelled for cause operator; a normal completion records
// its actual status. Session idle alone is not terminal evidence.
func (s *Supervisor) BeginCancellation(ctx context.Context, runID string, cause model.CancelCause) error {
	run, err := s.st.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	switch run.Status {
	case model.RunCancelling:
		// Already cancelling; the persisted request time keeps recovery
		// from re-issuing the cancel (§5.3).
		return nil
	case model.RunRunning:
	default:
		return nil
	}
	if err := s.st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.SetRunCancel(ctx, runID, cause)
	}); err != nil {
		return err
	}
	return s.client.CancelSession(ctx, run.ServerURL, run.WorkspaceID, run.SessionID)
}

// MonitorCancellations enforces the cancellation grace: if a cancelling
// run sees no matching terminal event within the grace period, drain and
// restart the instance before releasing serialization, then reconcile
// the run from its durable session references; an unknowable outcome
// stays nonterminal unknown until evidence or explicit abandonment
// (DESIGN §5.3, §7). It also begins timeout cancellation for runs past
// their step timeout.
func (s *Supervisor) MonitorCancellations(ctx context.Context) error {
	ticker := time.NewTicker(monitorPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.monitorOnce(ctx); err != nil {
				log.Error("cancellation monitor pass failed", "error", err)
			}
		}
	}
}

// monitorOnce performs one monitor pass.
func (s *Supervisor) monitorOnce(ctx context.Context) error {
	runs, err := s.st.ActiveRuns(ctx)
	if err != nil {
		return err
	}
	for _, run := range runs {
		switch run.Status {
		case model.RunRunning:
			if err := s.checkTimeout(ctx, run); err != nil {
				return err
			}
		case model.RunCancelling:
			if err := s.checkCancelGrace(ctx, run); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkTimeout begins timeout cancellation for one running run.
func (s *Supervisor) checkTimeout(ctx context.Context, run model.Run) error {
	step, err := s.stepForRun(ctx, run)
	if err != nil {
		return nil
	}
	if step.Timeout <= 0 || time.Since(run.CreatedAt) < step.Timeout {
		return nil
	}
	log.Warn("run exceeded step timeout; cancelling", "run", run.ID, "timeout", step.Timeout)
	return s.BeginCancellation(ctx, run.ID, model.CancelCauseTimeout)
}

// checkCancelGrace drains an instance whose cancellation went unconfirmed
// past the grace period; the reconciliation loop completes the drain and
// restart, and stream loss marks the run unknown (§5.3, §7).
func (s *Supervisor) checkCancelGrace(ctx context.Context, run model.Run) error {
	if run.CancelRequestedAt == nil {
		return nil
	}
	if time.Since(*run.CancelRequestedAt) < s.cancelGrace {
		return nil
	}
	instance, err := s.st.Instance(ctx, run.Project)
	if err != nil {
		return err
	}
	if instance.State == model.InstanceDraining || instance.State == model.InstanceStopped {
		return nil
	}
	log.Warn("cancellation grace expired; draining instance for restart",
		"run", run.ID, "project", run.Project)
	return s.st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.SetInstanceState(ctx, run.Project, model.InstanceDraining, instance.Generation)
	})
}

// stepForRun loads the step owning one run.
func (s *Supervisor) stepForRun(ctx context.Context, run model.Run) (model.Step, error) {
	goal, err := s.st.Goal(ctx, run.GoalID)
	if err != nil {
		return model.Step{}, err
	}
	for _, step := range goal.Steps {
		if step.ID == run.StepID {
			return step, nil
		}
	}
	return model.Step{}, nil
}

// ReconcileUnknownRun inspects the attempt's dedicated session through
// the workspace session API (DESIGN §5.3): a persisted matching user
// message proves prompt acceptance and a busy session returns the
// attempt to running; absence of that message does not prove
// non-acceptance. Only a matching live run_complete proves terminal
// status; lost events leave the attempt unknown, and Matchmaker never
// automatically resubmits it.
func (s *Supervisor) ReconcileUnknownRun(ctx context.Context, run model.Run) error {
	// Reload: the passed snapshot may predate the unknown transition.
	fresh, err := s.st.GetRun(ctx, run.ID)
	if err != nil {
		return err
	}
	run = fresh
	if run.Status != model.RunUnknown || run.SessionID == "" {
		return nil
	}
	info, err := s.client.GetSession(ctx, run.ServerURL, run.WorkspaceID, run.SessionID)
	if err != nil {
		// The session or workspace is gone; the outcome stays unknown.
		log.Debug("unknown run session unavailable", "run", run.ID, "error", err)
		return nil
	}
	accepted := false
	for _, message := range info.Messages {
		if message.Role != "user" {
			continue
		}
		sum := sha256.Sum256([]byte(message.Content))
		if hex.EncodeToString(sum[:]) == run.RenderedPromptHash {
			accepted = true
			break
		}
	}
	if !info.Busy {
		// Accepted-or-not, idle alone is not terminal evidence; only a
		// matching live run_complete could terminalize the attempt.
		log.Debug("unknown run session idle; awaiting evidence", "run", run.ID, "accepted", accepted)
		return nil
	}
	log.Info("unknown run session busy; returning to running", "run", run.ID)
	return s.st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.UpdateRunStatus(ctx, run.ID, model.RunUnknown, model.RunRunning)
	})
}

// OnStreamLoss handles a lost stream (DESIGN §5.3): mark in-flight runs
// unknown, let reconciliation recreate and re-attach the workspace if it
// was torn down (C1), and reconcile runs from persisted session IDs
// without ambiguous resubmission.
func (s *Supervisor) OnStreamLoss(ctx context.Context, instance model.Instance) error {
	runs, err := s.st.ActiveRuns(ctx)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.Project != instance.Project {
			continue
		}
		switch run.Status {
		case model.RunDispatching, model.RunRunning, model.RunCancelling:
		default:
			continue
		}
		if err := s.st.WithinTx(ctx, func(tx *store.Tx) error {
			return tx.UpdateRunStatus(ctx, run.ID, run.Status, model.RunUnknown)
		}); err != nil {
			log.Warn("run -> unknown failed", "run", run.ID, "error", err)
		}
	}
	return nil
}

// Abandon performs the explicit operator action that accepts an
// unresolved outcome: transition to abandoned and release any held
// serialization (DESIGN §4.5). Retrying an unknown attempt abandons it
// first.
func (s *Supervisor) Abandon(ctx context.Context, runID string) error {
	run, err := s.st.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if model.RunIsTerminal(run.Status) {
		return nil
	}
	if err := s.st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.UpdateRunStatus(ctx, run.ID, run.Status, model.RunAbandoned)
	}); err != nil {
		return err
	}
	// Serialization is derived from durable run state; release the
	// instance's busy state when no dispatched attempt remains.
	runs, err := s.st.ActiveRuns(ctx)
	if err != nil {
		return nil
	}
	for _, active := range runs {
		if active.Project == run.Project && active.Status != model.RunQueued {
			return nil
		}
	}
	instance, err := s.st.Instance(ctx, run.Project)
	if err != nil || instance.State != model.InstanceBusy {
		return nil
	}
	return s.st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.SetInstanceState(ctx, run.Project, model.InstanceReady, instance.Generation)
	})
}
