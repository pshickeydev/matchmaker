package config

import "time"

// Matchmaker holds all operator-tunable daemon configuration. Defaults and
// bounds come from DESIGN §5.1, §5.2, §5.4, §5.7, and the failure-mode
// table of §7.
type Matchmaker struct {
	StateDir         string // SQLite store, lock, and local Unix RPC socket live here
	CrushBinary      string // resolved once at daemon startup, never per spawn (§5.1)
	CoordinationAddr string // loopback bind for the coordination MCP server (§5.4)

	Reconcile   ReconcileConfig
	GoalLimits  GoalLimits
	NoteLimits  NoteLimits
	Planner     PlannerConfig
	Supervision SupervisionConfig
	Fleet       FleetPins
}

// ReconcileConfig controls the fleet reconciliation loop (DESIGN §5.1, §7).
// Crush-side lifecycle tunables are set per child through the spawn
// environment allowlist, never inherited: CRUSH_SERVER_DETACH_GRACE
// (default 10s) and CRUSH_SERVER_IDLE_TIMEOUT (default 60s), verified
// v0.94.1 internal/backend/backend.go.
type ReconcileConfig struct {
	TickInterval         time.Duration // default ~30s
	SSEReconnectBackoff  time.Duration
	HealthTimeout        time.Duration
	DrainTimeout         time.Duration // bounded per-instance teardown deadline
	GlobalDrainDeadline  time.Duration // bounded parallel shutdown deadline (§5.1 step 3)
	MaxRestartsPerWindow int           // crash-loop quarantine threshold
	CrashLoopWindow      time.Duration
	InstanceStartRetry   RetryPolicy // queued attempts abandon after exhaustion (§4.5)
}

// RetryPolicy is a bounded, backoff-driven retry configuration.
type RetryPolicy struct {
	MaxAttempts int
	Backoff     time.Duration
}

// GoalLimits are the configurable submission limits of DESIGN §5.2, checked
// during validation before anything is persisted.
type GoalLimits struct {
	MaxSteps            int
	MaxDepsPerStep      int
	MaxPromptBytes      int
	MaxTargetsPerStep   int
	MaxTotalRunsPerGoal int
	MinTimeout          time.Duration
	MaxTimeout          time.Duration
	MaxRetries          int
}

// NoteLimits are the coordination limits of DESIGN §5.4, checked before
// writes or response allocation.
type NoteLimits struct {
	MaxNoteBodyBytes     int
	MaxNotesPerGoal      int
	MaxInjectedNoteBytes int // injected note bytes per prompt
	MaxReadPageSize      int // notes and bytes per note_read page; callers may only lower server maxima
	MaxResultChunkBytes  int // result_read chunk size; server-configured, never caller-raised
	RateWindow           time.Duration
	MaxRequestsPerWindow int
	MaxRetainedNotes     int
}

// PlannerConfig bounds planning runs (DESIGN §5.7).
type PlannerConfig struct {
	MaxObjectiveBytes int
	MaxDraftBytes     int
	MaxRevisions      int
	MaxTotalBudget    time.Duration
	Timeout           time.Duration
	RetryPolicy       RetryPolicy
}

// SupervisionConfig controls cancellation and recovery behavior
// (DESIGN §5.3, §7).
type SupervisionConfig struct {
	CancelGrace time.Duration // unconfirmed cancellation triggers drain + restart
}

// PinnedCrushVersion is the project's pinned Crush version (DESIGN §9.3).
// Bumping it requires re-running the THREAT_MODEL Appendix A review.
const PinnedCrushVersion = "v0.94.1"

// FleetPins record the approved Crush version pin (DESIGN §9.3). The
// default pin is PinnedCrushVersion; the approved build_id is
// deployment-specific — release builds carry the commit, while unflagged
// builds derive it from the executable mtime and change on every recompile.
type FleetPins struct {
	ApprovedVersion string
	ApprovedBuildID string
}

// LoadMatchmaker reads Matchmaker's own configuration, applying defaults
// and validating ranges.
func LoadMatchmaker(path string) (*Matchmaker, error) {
	panic(notImplemented)
}
