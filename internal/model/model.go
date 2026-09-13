// Package model defines the core orchestration concepts of DESIGN §4:
// goals, steps, target executions, runs, instances, and notes, together
// with their persisted state machines and permitted transitions.
package model

import "time"

// GoalType distinguishes operator work goals from internal planning goals
// (DESIGN §5.7).
type GoalType string

const (
	GoalTypeWork GoalType = "work"
	GoalTypePlan GoalType = "plan"
)

// GoalStatus is the aggregated status of a goal (DESIGN §5.5).
type GoalStatus string

const (
	// GoalActive is the nonterminal status of a goal whose steps are not
	// all terminal yet.
	GoalActive GoalStatus = "active"
	// GoalSucceeded means every step succeeded.
	GoalSucceeded GoalStatus = "succeeded"
	// GoalFailed means no step succeeded.
	GoalFailed GoalStatus = "failed"
	// GoalPartial covers any mixed outcome, including skipped branches.
	GoalPartial GoalStatus = "partial"
)

// Goal is the top-level unit: a user-supplied objective decomposed into a
// DAG of steps (DESIGN §4.3). Target expansion is frozen at acceptance.
type Goal struct {
	ID              string
	Type            GoalType
	Objective       string
	Status          GoalStatus
	Steps           []Step
	FrozenTargets   map[string][]ResolvedTarget // step ID -> resolved targets
	PlanningRunID   string                      // provenance, empty for operator-authored goals
	FleetSnapshotID string                      // fleet config snapshot validated against
	CreatedAt       time.Time
	FinalizedAt     *time.Time
}

// StepStatus is the rollup status of a step, derived from its target
// executions (DESIGN §5.5). Steps are terminal only when every frozen
// target is terminal and no retry remains eligible.
type StepStatus string

const (
	StepPending StepStatus = "pending"
	StepSkipped StepStatus = "skipped"
	// StepSucceeded requires every target execution to have succeeded.
	StepSucceeded StepStatus = "succeeded"
	// StepFailed requires every target execution to have failed (failed,
	// cancelled, timed-out, skipped, and abandoned attempts count as failed
	// for this rollup while retaining their detailed status).
	StepFailed StepStatus = "failed"
	// StepPartial covers mixed terminal target outcomes.
	StepPartial StepStatus = "partial"
)

// Step is one node in a goal's DAG (DESIGN §4.4).
type Step struct {
	ID                 string
	GoalID             string
	PromptTemplate     string // Go text/template; upstream outputs are referenced, not inlined
	Target             TargetSpec
	Needs              []string
	AcceptPartialNeeds bool
	Supervision        SupervisionPolicy
	Timeout            time.Duration
	Retries            int
}

// SupervisionPolicy is the per-step permission policy (DESIGN §4.4, §5.3).
// The reserved policy "ask" is rejected by v1 goal validation.
type SupervisionPolicy string

const (
	// SupervisionDeny rejects every correlated permission request. Default.
	SupervisionDeny SupervisionPolicy = "deny"
	// SupervisionGrantAll automatically allows each correlated request
	// individually; it is never implemented via permissions/skip or yolo.
	SupervisionGrantAll SupervisionPolicy = "grant_all"
	// SupervisionAsk is reserved for future work (DESIGN §8).
	SupervisionAsk SupervisionPolicy = "ask"
)

// TargetSpec addresses work to fleet projects: explicit names, a tag
// expression, or all (fan-out) (DESIGN §4.4).
type TargetSpec struct {
	// Exactly one of Explicit, TagExpr, or All is set.
	Explicit []string
	TagExpr  string
	All      bool
}

// ResolvedTarget is one resolved (project, step) pair produced by target
// expansion during validation; the expansion is frozen at acceptance.
type ResolvedTarget struct {
	StepID      string
	Project     string
	Instance    string // fleet project name
	ServerURL   string
	Workspace   string   // Crush workspace ID, filled once the workspace exists
	Tags        []string // participant tags frozen with the expansion (§5.4 audience addressing)
	ExecutionID string   // target execution owning this target's attempts, assigned at acceptance
}

// TargetExecution is one durable fan-out execution of a step on one
// resolved instance; it owns an ordered sequence of run attempts
// (DESIGN §4.4, §5.3).
type TargetExecution struct {
	ID       string
	GoalID   string
	StepID   string
	Project  string
	Attempts []Run
}

// RunStatus is the state of one run attempt (DESIGN §4.5). Terminal states
// are immutable; nonterminal states block or gate further dispatch.
type RunStatus string

const (
	RunQueued      RunStatus = "queued"
	RunDispatching RunStatus = "dispatching"
	RunRunning     RunStatus = "running"
	RunCancelling  RunStatus = "cancelling"
	// RunUnknown means Matchmaker cannot prove the server-side state after
	// stream loss or restart; it blocks its target execution and workspace
	// from further dispatch until reconciled or abandoned.
	RunUnknown   RunStatus = "unknown"
	RunCompleted RunStatus = "completed"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
	RunTimedOut  RunStatus = "timed_out"
	RunSkipped   RunStatus = "skipped"
	// RunAbandoned records an explicit operator action accepting an
	// unresolved outcome, or automatic abandonment after startup-retry
	// exhaustion, releasing any held serialization.
	RunAbandoned RunStatus = "abandoned"
)

// Run is one attempt of a target execution on one instance, correlated to
// the Crush RunID from run_complete SSE events (DESIGN §4.5).
type Run struct {
	ID                 string
	GoalID             string
	StepID             string
	TargetExecutionID  string
	Attempt            int // monotonically increasing, 1-based
	Project            string
	ServerURL          string
	WorkspaceID        string
	InstanceGeneration int
	CrushRunID         string // caller-supplied RunID persisted before submission
	SessionID          string // dedicated session persisted before prompt submission
	RenderedPromptHash string
	Status             RunStatus
	CancelCause        CancelCause // set when entering cancelling
	NotesHighWater     int64       // last note_id injected into the prompt
	CancelRequestedAt  *time.Time  // prevents recovery from re-issuing cancel
	CreatedAt          time.Time
	FinalizedAt        *time.Time
}

// CancelCause records why cancellation began (DESIGN §5.3).
type CancelCause string

const (
	CancelCauseTimeout  CancelCause = "timeout"
	CancelCauseOperator CancelCause = "operator"
)

// Decision is one permission decision outcome (DESIGN §5.3).
type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionDeny  Decision = "deny"
)

// DrainMode selects between graceful and forced drain (DESIGN §5.1):
// forced drains cancel active runs; graceful drains await terminal states.
type DrainMode string

const (
	DrainGraceful DrainMode = "graceful"
	DrainForce    DrainMode = "force"
)

// InstanceState is the persisted state of a managed Crush server instance
// (DESIGN §4.2).
type InstanceState string

const (
	InstanceStopped          InstanceState = "stopped"
	InstanceStarting         InstanceState = "starting"
	InstanceReady            InstanceState = "ready"
	InstanceBusy             InstanceState = "busy"
	InstanceApprovalRequired InstanceState = "approval_required"
	InstanceVersionMismatch  InstanceState = "version_mismatch"
	InstanceDraining         InstanceState = "draining"
	InstanceFailed           InstanceState = "failed"
)

// Instance is a Crush server plus its managed workspace for one project.
// Generation increments on every successful server start or adoption and
// is persisted with runs and SSE streams.
type Instance struct {
	Project      string
	State        InstanceState
	Generation   int64
	Port         int
	ServerURL    string
	WorkspaceID  string
	Fingerprint  *Fingerprint
	FailedReason string
}

// Audience addresses a note to one instance, a tag, or all participants in
// the goal (DESIGN §4.6).
type Audience struct {
	Project string
	Tag     string
	All     bool
}

// Note is a short, addressed message passed between agents working the
// same goal (DESIGN §4.6, §5.4). Sender attribution and goal scoping are
// cooperative, not security boundaries.
type Note struct {
	NoteID    int64 // store-generated, monotonically increasing
	GoalID    string
	From      string // caller-supplied claimed sender project name
	To        Audience
	Body      string
	CreatedAt time.Time
}

// Fingerprint is an operator-approved digest set over project and global
// Crush configuration that can affect the server or workspace
// (DESIGN §5.1): file paths plus content digests, including additions
// and removals.
type Fingerprint struct {
	Project    string
	Entries    []FingerprintEntry
	ApprovedBy string
	ApprovedAt time.Time
}

// FingerprintEntry is one contributing config file with its digest.
type FingerprintEntry struct {
	Path   string
	Digest string
}

// PermissionDecision is the audit record for one permission_request
// decision; it is recorded transactionally before Matchmaker responds and
// never retains raw tool parameters (DESIGN §5.3, §5.5).
type PermissionDecision struct {
	ID                 string
	GoalID             string
	RunID              string
	Project            string
	WorkspaceID        string
	SessionID          string
	InstanceGeneration int64
	RequestID          string // when supplied by Crush
	RequestType        string
	Policy             SupervisionPolicy
	Decision           Decision
	CorrelationFailed  bool
	DecidedAt          time.Time
}
