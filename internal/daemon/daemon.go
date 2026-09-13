// Package daemon wires and runs the single Matchmaker daemon: it owns the
// SQLite store, reconciliation, Crush children, SSE claims, and the
// coordination MCP listener exclusively (DESIGN §3).
package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/charmbracelet/log"

	"github.com/pshickeydev/matchmaker/internal/aggregate"
	"github.com/pshickeydev/matchmaker/internal/config"
	"github.com/pshickeydev/matchmaker/internal/coordination"
	"github.com/pshickeydev/matchmaker/internal/crushapi"
	"github.com/pshickeydev/matchmaker/internal/daemonlock"
	"github.com/pshickeydev/matchmaker/internal/dispatch"
	"github.com/pshickeydev/matchmaker/internal/fleet"
	"github.com/pshickeydev/matchmaker/internal/goalvalidate"
	"github.com/pshickeydev/matchmaker/internal/model"
	"github.com/pshickeydev/matchmaker/internal/notes"
	"github.com/pshickeydev/matchmaker/internal/onboarding"
	"github.com/pshickeydev/matchmaker/internal/rpc"
	"github.com/pshickeydev/matchmaker/internal/store"
	"github.com/pshickeydev/matchmaker/internal/supervise"
)

const (
	// storeName is the SQLite file inside the state directory.
	storeName = "store.db"
	// aggregateEvery paces the periodic aggregation pass; runs wake
	// dependents through dispatch's own loop.
	aggregateEvery = 1 * time.Second
	// planMilestone is referenced by not-implemented plan responses.
	planMilestone = "the post-MVP planner milestone (M10, plan §3)"
)

// Daemon is the long-running process.
type Daemon struct {
	lock         *daemonlock.Lock
	st           *store.Store
	fleetCfg     *config.Fleet
	mm           *config.Matchmaker
	client       *crushapi.Client
	procs        *fleet.Supervisor
	reconciler   *fleet.Reconciler
	dispatcher   *dispatch.Dispatcher
	supervisor   *supervise.Supervisor
	notesSvc     *notes.Service
	coordination *coordination.Server
	aggregator   *aggregate.Aggregator
	rpcServer    *rpc.Server

	runCtx    context.Context
	runCancel context.CancelFunc
}

// New builds the daemon: acquire the OS-level exclusive lock on the state
// directory, open and integrity-check the store, resolve the crush
// binary once, build the client with a fresh process-lifetime UUID
// identity, and construct the reconciler, dispatcher, supervisor,
// coordination server, aggregator, and local RPC server.
func New(stateDir string, fleetCfg *config.Fleet, mmCfg *config.Matchmaker) (*Daemon, error) {
	lock, err := daemonlock.Acquire(stateDir)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(context.Background(), filepath.Join(stateDir, storeName))
	if err != nil {
		lock.Release()
		return nil, fmt.Errorf("open store: %w", err)
	}
	crushBinary, err := resolveCrushBinary(mmCfg.CrushBinary)
	if err != nil {
		st.Close()
		lock.Release()
		return nil, err
	}
	mmCfg.CrushBinary = crushBinary
	client := crushapi.NewClient(crushapi.NewClientID(), nil)
	procs := fleet.NewSupervisor()
	d := &Daemon{
		lock:     lock,
		st:       st,
		fleetCfg: fleetCfg,
		mm:       mmCfg,
		client:   client,
		procs:    procs,
	}
	d.reconciler = fleet.New(fleetCfg, mmCfg, st, client, procs)
	d.dispatcher = dispatch.New(st, client, notes.NewSelector(st), mmCfg)
	d.supervisor = supervise.New(st, client)
	d.notesSvc = notes.New(st, mmCfg.NoteLimits)
	d.coordination = coordination.New(d.notesSvc, st, client, mmCfg.NoteLimits)
	d.aggregator = aggregate.New(st, sessionSource{client: client})
	d.rpcServer = rpc.NewServer(stateDir, d)
	onboarding.Register(st, fleetCfg, mmCfg)
	return d, nil
}

// resolveCrushBinary resolves the crush server binary once at daemon
// startup, never per spawn (DESIGN §5.1).
func resolveCrushBinary(configured string) (string, error) {
	if filepath.IsAbs(configured) {
		if _, err := os.Stat(configured); err != nil {
			return "", fmt.Errorf("crush binary %s: %w", configured, err)
		}
		return configured, nil
	}
	resolved, err := exec.LookPath(configured)
	if err != nil {
		return "", fmt.Errorf("resolve crush binary %q on PATH: %w", configured, err)
	}
	return resolved, nil
}

// Run starts all components and blocks until ctx is canceled: startup
// adoption and reconciliation, the periodic reconcile loop, dispatch,
// supervision streams, cancellation monitors, the coordination MCP
// server, and the local RPC listener. A locked or corrupt store crashes
// the daemon loudly (DESIGN §7).
func (d *Daemon) Run(ctx context.Context) error {
	d.runCtx, d.runCancel = context.WithCancel(ctx)
	defer d.runCancel()

	if err := d.rpcServer.Listen(); err != nil {
		return err
	}
	components := []struct {
		name string
		run  func(context.Context) error
	}{
		{"reconciler", d.reconciler.Run},
		{"dispatcher", d.dispatcher.Run},
		{"cancellation-monitor", d.supervisor.MonitorCancellations},
	}
	errs := make(chan error, len(components)+2)
	for _, component := range components {
		component := component
		go func() { errs <- component.run(d.runCtx) }()
	}
	go func() { errs <- d.coordination.Serve(d.runCtx, d.mm.CoordinationAddr) }()
	go func() { errs <- d.aggregateLoop(d.runCtx) }()

	select {
	case <-ctx.Done():
		return d.Shutdown(context.Background(), model.DrainGraceful)
	case err := <-errs:
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Error("daemon component failed", "error", err)
		}
		return d.Shutdown(context.Background(), model.DrainGraceful)
	}
}

// aggregateLoop periodically rolls up active goals (DESIGN §5.5).
func (d *Daemon) aggregateLoop(ctx context.Context) error {
	ticker := time.NewTicker(aggregateEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			goals, err := d.st.ListGoals(ctx, model.GoalActive)
			if err != nil {
				continue
			}
			for _, goal := range goals {
				if err := d.aggregator.Wake(ctx, goal.ID); err != nil {
					log.Error("aggregate wake failed", "goal", goal.ID, "error", err)
				}
			}
		}
	}
}

// Handle implements rpc.Handler for CLI/TUI clients: goal submission
// (atomic §5.2 validation then persistence), plan requests (§5.7), status,
// operator approvals (fingerprint and compatibility override), run
// abandonment, instance reset, report export, onboarding, and shutdown.
// All methods enforce idempotency keys.
func (d *Daemon) Handle(ctx context.Context, req rpc.Request) (rpc.Response, error) {
	if err := requireIdempotencyKey(req); err != nil {
		return rpc.Response{}, err
	}
	if response, found, err := d.idempotent(ctx, req); found || err != nil {
		return response, err
	}
	response, err := d.dispatch(ctx, req)
	if err != nil {
		return rpc.Response{}, err
	}
	if mutating(req.Method) {
		encoded, encodeErr := json.Marshal(response)
		if encodeErr == nil {
			if err := d.st.WithinTx(ctx, func(tx *store.Tx) error {
				return tx.RecordIdempotency(ctx, req.IdempotencyKey, encoded)
			}); err != nil {
				log.Warn("idempotency record failed", "method", req.Method, "error", err)
			}
		}
	}
	return response, nil
}

// idempotent replays a recorded response for a retried mutating request.
func (d *Daemon) idempotent(ctx context.Context, req rpc.Request) (rpc.Response, bool, error) {
	if !mutating(req.Method) {
		return rpc.Response{}, false, nil
	}
	recorded, found, err := d.st.IdempotencyLookup(ctx, req.IdempotencyKey)
	if err != nil || !found {
		return rpc.Response{}, false, err
	}
	var response rpc.Response
	if err := json.Unmarshal(recorded, &response); err != nil {
		return rpc.Response{}, false, nil
	}
	return response, true, nil
}

// mutating reports whether a method deduplicates through idempotency
// keys (DESIGN §3).
func mutating(method rpc.Method) bool {
	switch method {
	case rpc.MethodSubmitGoal, rpc.MethodApproveFingerprint, rpc.MethodApproveVersion,
		rpc.MethodAbandonRun, rpc.MethodResetInstance, rpc.MethodOnboard:
		return true
	}
	return false
}

// requireIdempotencyKey rejects mutating requests without an idempotency
// key: all mutating methods share one dedupe namespace, and an empty key
// would replay the first recorded response to every later keyless call
// (DESIGN §3).
func requireIdempotencyKey(req rpc.Request) error {
	if !mutating(req.Method) {
		return nil
	}
	if req.IdempotencyKey == "" {
		return fmt.Errorf("%s requires an idempotency key", req.Method)
	}
	return nil
}

// dispatch routes one request to its handler.
func (d *Daemon) dispatch(ctx context.Context, req rpc.Request) (rpc.Response, error) {
	switch req.Method {
	case rpc.MethodSubmitGoal:
		return d.handleSubmit(ctx, req)
	case rpc.MethodPlanGoal:
		return rpc.Response{}, fmt.Errorf("plan_goal is not implemented in the MVP; planning runs land with %s", planMilestone)
	case rpc.MethodGoalStatus:
		goal, err := d.st.Goal(ctx, req.GoalID)
		if err != nil {
			return rpc.Response{}, err
		}
		runs, err := d.goalRuns(ctx, goal)
		if err != nil {
			return rpc.Response{}, err
		}
		return rpc.Response{Goal: &goal, Runs: runs}, nil
	case rpc.MethodListGoals:
		goals, err := d.st.ListGoals(ctx, model.GoalActive)
		if err != nil {
			return rpc.Response{}, err
		}
		finalized, err := d.st.ListGoals(ctx, model.GoalSucceeded)
		if err != nil {
			return rpc.Response{}, err
		}
		return rpc.Response{Goals: append(goals, finalized...)}, nil
	case rpc.MethodApproveFingerprint:
		return d.handleApproveFingerprint(ctx, req)
	case rpc.MethodApproveVersion:
		if req.Version == "" {
			return rpc.Response{}, errors.New("approve_version requires the observed version")
		}
		if err := d.st.WithinTx(ctx, func(tx *store.Tx) error {
			return tx.RecordCompatibilityOverride(ctx, req.Version, req.BuildID, "operator")
		}); err != nil {
			return rpc.Response{}, err
		}
		return rpc.Response{Message: "compatibility override recorded"}, nil
	case rpc.MethodAbandonRun:
		if err := d.supervisor.Abandon(ctx, req.RunID); err != nil {
			return rpc.Response{}, err
		}
		return rpc.Response{Message: "run abandoned"}, nil
	case rpc.MethodResetInstance:
		return d.handleResetInstance(ctx, req)
	case rpc.MethodFleetStatus:
		return d.handleFleetStatus(ctx)
	case rpc.MethodExportReport:
		return d.handleExportReport(ctx, req)
	case rpc.MethodOnboard:
		return d.handleOnboard(ctx, req)
	case rpc.MethodShutdown:
		go d.runCancel()
		return rpc.Response{Message: "shutdown initiated"}, nil
	}
	return rpc.Response{}, fmt.Errorf("unknown method %q", req.Method)
}

// handleSubmit validates atomically against one fleet snapshot and
// persists in a single transaction (DESIGN §5.2). The idempotency record
// commits in the same transaction as the goal it deduplicates.
func (d *Daemon) handleSubmit(ctx context.Context, req rpc.Request) (rpc.Response, error) {
	if req.Submit == nil {
		return rpc.Response{}, errors.New("submit_goal requires a draft")
	}
	errs, frozen := goalvalidate.ValidateDraft(req.Submit.Draft, d.fleetCfg.Snapshot(), d.mm.GoalLimits)
	if len(errs) > 0 {
		return rpc.Response{Validation: errs}, nil
	}
	goal := model.Goal{
		ID:            newGoalID(),
		Type:          model.GoalTypeWork,
		Objective:     req.Submit.Draft.Objective,
		Status:        model.GoalActive,
		Steps:         req.Submit.Draft.Steps,
		FrozenTargets: frozen,
	}
	response := rpc.Response{GoalID: goal.ID, Message: "goal accepted"}
	err := d.st.WithinTx(ctx, func(tx *store.Tx) error {
		if err := tx.CreateGoal(ctx, goal); err != nil {
			return err
		}
		encoded, encodeErr := json.Marshal(response)
		if encodeErr != nil {
			return encodeErr
		}
		return tx.RecordIdempotency(ctx, req.IdempotencyKey, encoded)
	})
	if err != nil {
		return rpc.Response{}, err
	}
	// Demand-start every targeted instance immediately (plan M5).
	for _, targets := range frozen {
		for _, target := range targets {
			d.reconciler.DemandStart(target.Project)
		}
	}
	return response, nil
}

// goalRuns lists every attempt of every target execution of a goal's
// steps, newest last (goal_status detail; run IDs enable explicit
// abandonment of stuck attempts).
func (d *Daemon) goalRuns(ctx context.Context, goal model.Goal) ([]model.Run, error) {
	var runs []model.Run
	for _, step := range goal.Steps {
		for _, target := range goal.FrozenTargets[step.ID] {
			exec, err := d.st.TargetExecution(ctx, target.ExecutionID)
			if err != nil {
				continue
			}
			runs = append(runs, exec.Attempts...)
		}
	}
	return runs, nil
}

// newGoalID generates one store-recognizable goal identifier.
func newGoalID() string {
	var bytes [16]byte
	rand.Read(bytes[:])
	return "g_" + hex.EncodeToString(bytes[:])
}

// handleApproveFingerprint records the operator approval of the current
// project config (DESIGN §5.1); onboarding's own writes commit their
// fingerprint together with the install.
func (d *Daemon) handleApproveFingerprint(ctx context.Context, req rpc.Request) (rpc.Response, error) {
	project, err := d.fleetCfg.Project(req.Project)
	if err != nil {
		return rpc.Response{}, err
	}
	fingerprint, err := fleet.ComputeFingerprint(project, d.mm)
	if err != nil {
		return rpc.Response{}, err
	}
	fingerprint.ApprovedBy = "operator"
	if err := d.st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.SetApprovedFingerprint(ctx, fingerprint)
	}); err != nil {
		return rpc.Response{}, err
	}
	return rpc.Response{Message: "fingerprint approved"}, nil
}

// handleResetInstance moves a failed instance back to stopped for
// explicit operator reset (§4.2).
func (d *Daemon) handleResetInstance(ctx context.Context, req rpc.Request) (rpc.Response, error) {
	instance, err := d.st.Instance(ctx, req.Project)
	if err != nil {
		return rpc.Response{}, err
	}
	if instance.State != model.InstanceFailed {
		return rpc.Response{}, fmt.Errorf("instance %s is %s; only failed instances reset", req.Project, instance.State)
	}
	if err := d.st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.SetInstanceState(ctx, req.Project, model.InstanceStopped, instance.Generation)
	}); err != nil {
		return rpc.Response{}, err
	}
	return rpc.Response{Message: "instance reset to stopped"}, nil
}

// handleFleetStatus reports every fleet entry's persisted instance.
func (d *Daemon) handleFleetStatus(ctx context.Context) (rpc.Response, error) {
	instances := make([]model.Instance, 0, len(d.fleetCfg.Projects))
	for _, project := range d.fleetCfg.Projects {
		instance, err := d.st.Instance(ctx, project.Name)
		if err != nil {
			instance = model.Instance{Project: project.Name, State: model.InstanceStopped}
		}
		instances = append(instances, instance)
	}
	return rpc.Response{Instances: instances}, nil
}

// handleExportReport streams the per-goal report to the operator-selected
// file with operator-only permissions (DESIGN §5.5).
func (d *Daemon) handleExportReport(ctx context.Context, req rpc.Request) (rpc.Response, error) {
	if req.GoalID == "" || req.FilePath == "" {
		return rpc.Response{}, errors.New("export_report requires a goal and a file path")
	}
	if err := os.MkdirAll(filepath.Dir(req.FilePath), 0o700); err != nil {
		return rpc.Response{}, err
	}
	file, err := os.OpenFile(req.FilePath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return rpc.Response{}, err
	}
	defer file.Close()
	if err := d.aggregator.Export(ctx, req.GoalID, file); err != nil {
		return rpc.Response{}, err
	}
	return rpc.Response{Message: "report exported to " + req.FilePath}, nil
}

// handleOnboard previews or installs the Matchmaker-owned crushrc block
// (DESIGN §5.4; plan Decision 2 / M8). The daemon orchestrates because
// only it owns the store: the approved write and the resulting
// fingerprint commit together and never self-alert.
func (d *Daemon) handleOnboard(ctx context.Context, req rpc.Request) (rpc.Response, error) {
	project, err := d.fleetCfg.Project(req.Project)
	if err != nil {
		return rpc.Response{}, err
	}
	if project.NoteOptOut {
		return rpc.Response{}, fmt.Errorf("project %s opted out of coordination registration; flip note_opt_out to register it", req.Project)
	}
	coordinationURL := "http://" + d.mm.CoordinationAddr
	if d.coordination.Addr() != nil {
		coordinationURL = "http://" + d.coordination.Addr().String()
	}
	fragment := onboarding.GenerateFragment(coordinationURL)
	if req.Approval != string(onboarding.ApprovalGranted) {
		return rpc.Response{Fragment: fragment,
			Message: "preview; pass approval=granted to install"}, nil
	}
	result, err := onboarding.Install(project.Path, fragment, onboarding.ApprovalGranted)
	if err != nil {
		// Fail closed: the fragment is returned for manual installation.
		return rpc.Response{Fragment: fragment, Message: "onboarding failed closed: " + err.Error()}, nil
	}
	installed := "appended to existing config"
	if result.WroteNewFile {
		installed = "wrote new .crushrc"
	}
	if err := onboarding.VerifyParse(d.mm.CrushBinary, project.Path); err != nil {
		return rpc.Response{Fragment: fragment, Message: "config failed parse verification: " + err.Error()}, nil
	}
	if err := onboarding.CommitWithFingerprint(project.Name); err != nil {
		return rpc.Response{Fragment: fragment, Message: "fingerprint commit failed: " + err.Error()}, nil
	}
	return rpc.Response{Onboarded: true, Fragment: fragment, Message: "coordination registered (" + installed + ")"}, nil
}

// Shutdown performs the drain: stop the RPC listener, drain all instances
// in parallel under the global deadline, close streams and release
// workspace holds, then retire the process-wide client claim as final
// cleanup (DESIGN §5.1). Forced mode cancels active runs; graceful mode
// awaits their terminal states.
func (d *Daemon) Shutdown(ctx context.Context, mode model.DrainMode) error {
	if d.runCancel != nil {
		d.runCancel()
	}
	d.rpcServer.Close()
	if err := d.reconciler.Shutdown(ctx, mode); err != nil {
		log.Error("fleet drain failed", "error", err)
	}
	if err := d.st.Close(); err != nil {
		log.Error("store close failed", "error", err)
	}
	return d.lock.Release()
}

// sessionSource lazily resolves Crush session content for reports and
// exports (DESIGN §5.5) through the allowlist-scoped client.
type sessionSource struct {
	client *crushapi.Client
}

// FinalOutput returns a run's final assistant output.
func (s sessionSource) FinalOutput(ctx context.Context, run model.Run) ([]byte, bool, error) {
	info, err := s.client.GetSession(ctx, run.ServerURL, run.WorkspaceID, run.SessionID)
	if err != nil {
		return nil, false, nil
	}
	for i := len(info.Messages) - 1; i >= 0; i-- {
		message := info.Messages[i]
		if message.Role == "assistant" && message.Content != "" {
			return []byte(message.Content), true, nil
		}
	}
	return nil, false, nil
}
