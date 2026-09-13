package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Defaults and bounds for every operator-tunable value. They fill unset
// fields at load and anchor the range checks; DESIGN §5.1, §5.2, §5.4,
// §5.7, §7.
const (
	defaultStateDir         = "~/.local/state/matchmaker"
	defaultCrushBinary      = "crush"
	defaultCoordinationAddr = "127.0.0.1:4763"

	defaultTick          = 30 * time.Second
	defaultSSEReconnect  = 1 * time.Second
	defaultHealthTimeout = 5 * time.Second
	defaultDrainTimeout  = 30 * time.Second
	defaultGlobalDrain   = 2 * time.Minute
	defaultRestarts      = 5
	defaultCrashWindow   = 10 * time.Minute
	defaultStartAttempts = 3
	defaultStartBackoff  = 5 * time.Second

	defaultMaxSteps       = 50
	defaultMaxDeps        = 10
	defaultMaxPromptBytes = 64 * 1024
	defaultMaxTargets     = 10
	defaultMaxTotalRuns   = 200
	defaultMinTimeout     = 5 * time.Second
	defaultMaxTimeout     = time.Hour
	defaultMaxRetries     = 3

	defaultMaxNoteBody   = 4 * 1024
	defaultMaxNotesGoal  = 1000
	defaultMaxInjected   = 2 * 1024
	defaultMaxReadPage   = 50
	defaultMaxChunk      = 16 * 1024
	defaultRateWindow    = time.Minute
	defaultRateRequests  = 120
	defaultRetainedNotes = 5000

	defaultMaxObjective   = 8 * 1024
	defaultMaxDraft       = 64 * 1024
	defaultMaxRevisions   = 3
	defaultPlannerBudget  = 30 * time.Minute
	defaultPlannerTimeout = 10 * time.Minute
	defaultPlannerTries   = 3
	defaultPlannerBackoff = 5 * time.Second

	defaultCancelGrace = time.Minute
)

// envNamePattern is the syntax of one passable environment variable name.
var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Matchmaker holds all operator-tunable daemon configuration. Defaults and
// bounds come from DESIGN §5.1, §5.2, §5.4, §5.7, and the failure-mode
// table of §7.
type Matchmaker struct {
	StateDir         string   // SQLite store, lock, and local Unix RPC socket live here
	CrushBinary      string   // resolved once at daemon startup, never per spawn (§5.1)
	CoordinationAddr string   // loopback bind for the coordination MCP server (§5.4)
	PassEnv          []string // env names copied into each crush server child (§5.1)

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
// (10s) and CRUSH_SERVER_IDLE_TIMEOUT (1h, raised from Crush's 60s
// default because Matchmaker owns instance lifecycle; a 60s idle exit
// would orphan sessions between goal steps), verified v0.94.1
// internal/backend/backend.go and against a live server.
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

// rawMatchmaker mirrors matchmaker.toml for strict TOML decoding.
// Durations decode as strings parsed with time.ParseDuration.
type rawMatchmaker struct {
	StateDir         string         `toml:"state_dir"`
	CrushBinary      string         `toml:"crush_binary"`
	CoordinationAddr string         `toml:"coordination_addr"`
	PassEnv          []string       `toml:"pass_env"`
	Reconcile        rawReconcile   `toml:"reconcile"`
	GoalLimits       rawGoalLimits  `toml:"goal_limits"`
	NoteLimits       rawNoteLimits  `toml:"note_limits"`
	Planner          rawPlanner     `toml:"planner"`
	Supervision      rawSupervision `toml:"supervision"`
	Fleet            rawFleetPins   `toml:"fleet_pins"`
}

type rawReconcile struct {
	TickInterval         string   `toml:"tick_interval"`
	SSEReconnectBackoff  string   `toml:"sse_reconnect_backoff"`
	HealthTimeout        string   `toml:"health_timeout"`
	DrainTimeout         string   `toml:"drain_timeout"`
	GlobalDrainDeadline  string   `toml:"global_drain_deadline"`
	MaxRestartsPerWindow int      `toml:"max_restarts_per_window"`
	CrashLoopWindow      string   `toml:"crash_loop_window"`
	InstanceStartRetry   rawRetry `toml:"instance_start_retry"`
}

type rawRetry struct {
	MaxAttempts int    `toml:"max_attempts"`
	Backoff     string `toml:"backoff"`
}

type rawGoalLimits struct {
	MaxSteps            int    `toml:"max_steps"`
	MaxDepsPerStep      int    `toml:"max_deps_per_step"`
	MaxPromptBytes      int    `toml:"max_prompt_bytes"`
	MaxTargetsPerStep   int    `toml:"max_targets_per_step"`
	MaxTotalRunsPerGoal int    `toml:"max_total_runs_per_goal"`
	MinTimeout          string `toml:"min_timeout"`
	MaxTimeout          string `toml:"max_timeout"`
	MaxRetries          int    `toml:"max_retries"`
}

type rawNoteLimits struct {
	MaxNoteBodyBytes     int    `toml:"max_note_body_bytes"`
	MaxNotesPerGoal      int    `toml:"max_notes_per_goal"`
	MaxInjectedNoteBytes int    `toml:"max_injected_note_bytes"`
	MaxReadPageSize      int    `toml:"max_read_page_size"`
	MaxResultChunkBytes  int    `toml:"max_result_chunk_bytes"`
	RateWindow           string `toml:"rate_window"`
	MaxRequestsPerWindow int    `toml:"max_requests_per_window"`
	MaxRetainedNotes     int    `toml:"max_retained_notes"`
}

type rawPlanner struct {
	MaxObjectiveBytes int      `toml:"max_objective_bytes"`
	MaxDraftBytes     int      `toml:"max_draft_bytes"`
	MaxRevisions      int      `toml:"max_revisions"`
	MaxTotalBudget    string   `toml:"max_total_budget"`
	Timeout           string   `toml:"timeout"`
	RetryPolicy       rawRetry `toml:"retry_policy"`
}

type rawSupervision struct {
	CancelGrace string `toml:"cancel_grace"`
}

type rawFleetPins struct {
	ApprovedVersion string `toml:"approved_version"`
	ApprovedBuildID string `toml:"approved_build_id"`
}

// LoadMatchmaker reads Matchmaker's own configuration, applying defaults
// and validating ranges.
func LoadMatchmaker(path string) (*Matchmaker, error) {
	var raw rawMatchmaker
	meta, err := toml.DecodeFile(path, &raw)
	if err != nil {
		return nil, fmt.Errorf("decode matchmaker config %s: %w", path, err)
	}
	if err := rejectUndecoded(meta.Undecoded(), path); err != nil {
		return nil, err
	}
	mm, errs := buildMatchmaker(raw)
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("matchmaker config %s: %w", path, err)
	}
	return mm, nil
}

// buildMatchmaker applies defaults to raw values and validates ranges,
// collecting every problem before failing.
func buildMatchmaker(raw rawMatchmaker) (*Matchmaker, []error) {
	var errs []error
	duration := func(name, value string, fallback time.Duration) time.Duration {
		if strings.TrimSpace(value) == "" {
			return fallback
		}
		parsed, err := time.ParseDuration(value)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: invalid duration %q", name, value))
			return fallback
		}
		return parsed
	}
	positive := func(errs *[]error, name string, value, fallback int) int {
		if value == 0 {
			return fallback
		}
		if value < 0 {
			*errs = append(*errs, fmt.Errorf("%s: %d is negative", name, value))
			return fallback
		}
		return value
	}

	mm := &Matchmaker{
		StateDir:         strOr(raw.StateDir, defaultStateDir),
		CrushBinary:      strOr(raw.CrushBinary, defaultCrushBinary),
		CoordinationAddr: strOr(raw.CoordinationAddr, defaultCoordinationAddr),
		PassEnv:          slices.Clone(raw.PassEnv),
		Reconcile: ReconcileConfig{
			TickInterval:         duration("reconcile.tick_interval", raw.Reconcile.TickInterval, defaultTick),
			SSEReconnectBackoff:  duration("reconcile.sse_reconnect_backoff", raw.Reconcile.SSEReconnectBackoff, defaultSSEReconnect),
			HealthTimeout:        duration("reconcile.health_timeout", raw.Reconcile.HealthTimeout, defaultHealthTimeout),
			DrainTimeout:         duration("reconcile.drain_timeout", raw.Reconcile.DrainTimeout, defaultDrainTimeout),
			GlobalDrainDeadline:  duration("reconcile.global_drain_deadline", raw.Reconcile.GlobalDrainDeadline, defaultGlobalDrain),
			MaxRestartsPerWindow: positive(&errs, "reconcile.max_restarts_per_window", raw.Reconcile.MaxRestartsPerWindow, defaultRestarts),
			CrashLoopWindow:      duration("reconcile.crash_loop_window", raw.Reconcile.CrashLoopWindow, defaultCrashWindow),
			InstanceStartRetry: RetryPolicy{
				MaxAttempts: positive(&errs, "reconcile.instance_start_retry.max_attempts", raw.Reconcile.InstanceStartRetry.MaxAttempts, defaultStartAttempts),
				Backoff:     duration("reconcile.instance_start_retry.backoff", raw.Reconcile.InstanceStartRetry.Backoff, defaultStartBackoff),
			},
		},
		GoalLimits: GoalLimits{
			MaxSteps:            positive(&errs, "goal_limits.max_steps", raw.GoalLimits.MaxSteps, defaultMaxSteps),
			MaxDepsPerStep:      positive(&errs, "goal_limits.max_deps_per_step", raw.GoalLimits.MaxDepsPerStep, defaultMaxDeps),
			MaxPromptBytes:      positive(&errs, "goal_limits.max_prompt_bytes", raw.GoalLimits.MaxPromptBytes, defaultMaxPromptBytes),
			MaxTargetsPerStep:   positive(&errs, "goal_limits.max_targets_per_step", raw.GoalLimits.MaxTargetsPerStep, defaultMaxTargets),
			MaxTotalRunsPerGoal: positive(&errs, "goal_limits.max_total_runs_per_goal", raw.GoalLimits.MaxTotalRunsPerGoal, defaultMaxTotalRuns),
			MinTimeout:          duration("goal_limits.min_timeout", raw.GoalLimits.MinTimeout, defaultMinTimeout),
			MaxTimeout:          duration("goal_limits.max_timeout", raw.GoalLimits.MaxTimeout, defaultMaxTimeout),
			MaxRetries:          positive(&errs, "goal_limits.max_retries", raw.GoalLimits.MaxRetries, defaultMaxRetries),
		},
		NoteLimits: NoteLimits{
			MaxNoteBodyBytes:     positive(&errs, "note_limits.max_note_body_bytes", raw.NoteLimits.MaxNoteBodyBytes, defaultMaxNoteBody),
			MaxNotesPerGoal:      positive(&errs, "note_limits.max_notes_per_goal", raw.NoteLimits.MaxNotesPerGoal, defaultMaxNotesGoal),
			MaxInjectedNoteBytes: positive(&errs, "note_limits.max_injected_note_bytes", raw.NoteLimits.MaxInjectedNoteBytes, defaultMaxInjected),
			MaxReadPageSize:      positive(&errs, "note_limits.max_read_page_size", raw.NoteLimits.MaxReadPageSize, defaultMaxReadPage),
			MaxResultChunkBytes:  positive(&errs, "note_limits.max_result_chunk_bytes", raw.NoteLimits.MaxResultChunkBytes, defaultMaxChunk),
			RateWindow:           duration("note_limits.rate_window", raw.NoteLimits.RateWindow, defaultRateWindow),
			MaxRequestsPerWindow: positive(&errs, "note_limits.max_requests_per_window", raw.NoteLimits.MaxRequestsPerWindow, defaultRateRequests),
			MaxRetainedNotes:     positive(&errs, "note_limits.max_retained_notes", raw.NoteLimits.MaxRetainedNotes, defaultRetainedNotes),
		},
		Planner: PlannerConfig{
			MaxObjectiveBytes: positive(&errs, "planner.max_objective_bytes", raw.Planner.MaxObjectiveBytes, defaultMaxObjective),
			MaxDraftBytes:     positive(&errs, "planner.max_draft_bytes", raw.Planner.MaxDraftBytes, defaultMaxDraft),
			MaxRevisions:      positive(&errs, "planner.max_revisions", raw.Planner.MaxRevisions, defaultMaxRevisions),
			MaxTotalBudget:    duration("planner.max_total_budget", raw.Planner.MaxTotalBudget, defaultPlannerBudget),
			Timeout:           duration("planner.timeout", raw.Planner.Timeout, defaultPlannerTimeout),
			RetryPolicy: RetryPolicy{
				MaxAttempts: positive(&errs, "planner.retry_policy.max_attempts", raw.Planner.RetryPolicy.MaxAttempts, defaultPlannerTries),
				Backoff:     duration("planner.retry_policy.backoff", raw.Planner.RetryPolicy.Backoff, defaultPlannerBackoff),
			},
		},
		Supervision: SupervisionConfig{
			CancelGrace: duration("supervision.cancel_grace", raw.Supervision.CancelGrace, defaultCancelGrace),
		},
		Fleet: FleetPins{
			ApprovedVersion: strOr(raw.Fleet.ApprovedVersion, PinnedCrushVersion),
			ApprovedBuildID: raw.Fleet.ApprovedBuildID,
		},
	}

	stateDir, err := expandHome(mm.StateDir)
	if err != nil {
		errs = append(errs, fmt.Errorf("state_dir: %w", err))
	} else {
		mm.StateDir = stateDir
		if !filepath.IsAbs(stateDir) {
			errs = append(errs, fmt.Errorf("state_dir %q is not absolute", stateDir))
		}
	}
	errs = append(errs, checkPositiveDurations(mm)...)
	for _, name := range mm.PassEnv {
		if !envNamePattern.MatchString(name) {
			errs = append(errs, fmt.Errorf("pass_env: invalid variable name %q", name))
		}
	}
	if mm.GoalLimits.MinTimeout > mm.GoalLimits.MaxTimeout {
		errs = append(errs, fmt.Errorf("goal_limits: min_timeout %s exceeds max_timeout %s", mm.GoalLimits.MinTimeout, mm.GoalLimits.MaxTimeout))
	}
	return mm, errs
}

// strOr returns value when set, otherwise fallback.
func strOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// checkPositiveDurations validates that every tuned duration is positive.
func checkPositiveDurations(mm *Matchmaker) []error {
	var errs []error
	fields := []struct {
		name  string
		value time.Duration
	}{
		{"reconcile.tick_interval", mm.Reconcile.TickInterval},
		{"reconcile.sse_reconnect_backoff", mm.Reconcile.SSEReconnectBackoff},
		{"reconcile.health_timeout", mm.Reconcile.HealthTimeout},
		{"reconcile.drain_timeout", mm.Reconcile.DrainTimeout},
		{"reconcile.global_drain_deadline", mm.Reconcile.GlobalDrainDeadline},
		{"reconcile.crash_loop_window", mm.Reconcile.CrashLoopWindow},
		{"reconcile.instance_start_retry.backoff", mm.Reconcile.InstanceStartRetry.Backoff},
		{"goal_limits.min_timeout", mm.GoalLimits.MinTimeout},
		{"goal_limits.max_timeout", mm.GoalLimits.MaxTimeout},
		{"note_limits.rate_window", mm.NoteLimits.RateWindow},
		{"planner.max_total_budget", mm.Planner.MaxTotalBudget},
		{"planner.timeout", mm.Planner.Timeout},
		{"planner.retry_policy.backoff", mm.Planner.RetryPolicy.Backoff},
		{"supervision.cancel_grace", mm.Supervision.CancelGrace},
	}
	for _, field := range fields {
		if field.value <= 0 {
			errs = append(errs, fmt.Errorf("%s: %s is not positive", field.name, field.value))
		}
	}
	return errs
}
