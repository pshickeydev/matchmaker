// Package fleet implements fleet reconciliation: observe, compare,
// converge over one crush server per project, plus the supervised child
// process lifecycle (DESIGN §5.1, §10.1).
package fleet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/log"

	"github.com/pshickeydev/matchmaker/internal/config"
	"github.com/pshickeydev/matchmaker/internal/crushapi"
	"github.com/pshickeydev/matchmaker/internal/model"
	"github.com/pshickeydev/matchmaker/internal/store"
	"github.com/pshickeydev/matchmaker/internal/supervise"
)

// Child environment allowlist contents (DESIGN §5.1, §6, THREAT_MODEL
// TB3): the crush server child never inherits the full parent
// environment; lifecycle tunables are set deliberately (verified v0.94.1
// internal/backend/backend.go). The idle timeout is raised from Crush's
// 60s default to 1h (verified against a live v0.94.1 server): Matchmaker
// owns instance lifecycle through its own drain and shutdown_if_idle, and
// a 60s idle exit would orphan sessions and result sources between goal
// steps.
const (
	envDetachGrace  = "CRUSH_SERVER_DETACH_GRACE=10s"
	envIdleTimeout  = "CRUSH_SERVER_IDLE_TIMEOUT=3600s"
	healthPollEvery = 250 * time.Millisecond
	// removePollEvery paces bounded teardown waits.
	removePollEvery = 100 * time.Millisecond
	// drainWriteBackoff spaces the single retry of a drain's final store
	// write, absorbing contention with other shutting-down components.
	drainWriteBackoff = 250 * time.Millisecond
	// Teardown escalation signals after the bounded drain timeouts.
	sigTerm = syscall.SIGTERM
	sigKill = syscall.SIGKILL
)

// hostFlag and dataDirFlag are the typed server flags of the allowlisted
// crush_options surface (DESIGN §4.1).
const (
	hostFlag     = "--host"
	debugFlag    = "--debug"
	dataDirFlag  = "--data-dir"
	serverSubcmd = "server"
)

// Reconciler drives the fleet toward desired state. It performs an
// immediate startup reconciliation, then a periodic loop (default ~30s
// tick); it never handles failures reactively per-failure.
type Reconciler struct {
	fleet  *config.Fleet
	mm     *config.Matchmaker
	st     *store.Store
	client *crushapi.Client
	procs  *Supervisor

	consumer streamConsumer
	recovery *supervise.Supervisor

	mu          sync.Mutex
	streams     map[string]*crushapi.EventStream
	reattaching map[string]bool
	restarts    map[string][]time.Time
	demand      chan string
}

// streamConsumer handles one attached stream until loss; the
// supervision package supplies the real implementation (DESIGN §5.1
// AttachStream). Until supervision lands, the reconciler drains streams
// to hold the workspace claim (C1).
type streamConsumer interface {
	Consume(ctx context.Context, stream *crushapi.EventStream, instance model.Instance) error
}

// drainConsumer reads and discards events while holding the stream open.
type drainConsumer struct{}

// Consume drains until stream loss.
func (drainConsumer) Consume(ctx context.Context, stream *crushapi.EventStream, _ model.Instance) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-stream.Events():
			if !ok {
				return nil
			}
		}
	}
}

// New builds a reconciler from the desired-state fleet config, daemon
// configuration, the durable store, and the Crush client.
func New(fleet *config.Fleet, mm *config.Matchmaker, st *store.Store, client *crushapi.Client, procs *Supervisor) *Reconciler {
	supervisor := supervise.New(st, client)
	return &Reconciler{
		fleet:       fleet,
		mm:          mm,
		st:          st,
		client:      client,
		procs:       procs,
		consumer:    supervisor,
		recovery:    supervisor,
		streams:     make(map[string]*crushapi.EventStream),
		reattaching: make(map[string]bool),
		restarts:    make(map[string][]time.Time),
		demand:      make(chan string, len(fleet.Projects)+1),
	}
}

// Run blocks until ctx is canceled: adoptOnStartup, then tick forever.
func (r *Reconciler) Run(ctx context.Context) error {
	if err := r.adoptOnStartup(ctx); err != nil {
		return fmt.Errorf("startup adoption: %w", err)
	}
	ticker := time.NewTicker(r.mm.Reconcile.TickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := r.reconcileOnce(ctx); err != nil {
				log.Error("reconcile pass failed", "error", err)
			}
		case <-r.demand:
			if err := r.reconcileOnce(ctx); err != nil {
				log.Error("demand reconcile pass failed", "error", err)
			}
		}
	}
}

// adoptOnStartup reloads durable state, probes live servers, verifies
// project config fingerprints before adoption (DESIGN §7), and attaches
// new-process SSE claims to matching workspaces before the periodic timer
// starts, minimizing the detached-stream window and allowing adoption
// during Crush's detach grace (DESIGN §5.1 step 4). An expired workspace
// is recreated and in-flight runs reconcile from durable session IDs;
// ambiguous dispatches are never resubmitted.
func (r *Reconciler) adoptOnStartup(ctx context.Context) error {
	observations, err := r.observe(ctx)
	if err != nil {
		return err
	}
	for _, project := range r.fleet.Projects {
		if _, err := r.st.Instance(ctx, project.Name); err != nil {
			// Never recorded: nothing to adopt; the periodic loop
			// starts it on demand.
			continue
		}
		if err := r.converge(ctx, project, observations[project.Name]); err != nil {
			log.Error("adoption failed", "project", project.Name, "error", err)
			continue
		}
		// Orphan recovery (§7): dispatched runs left nonterminal by the
		// previous daemon lost their stream evidence; they become
		// unknown and reconcile from durable session IDs. Never
		// resubmit.
		r.markRunsUnknown(ctx, project.Name)
		r.reconcileUnknownRuns(ctx, project)
	}
	return nil
}

// reconcileOnce runs one observe/compare/converge pass over the fleet.
func (r *Reconciler) reconcileOnce(ctx context.Context) error {
	observations, err := r.observe(ctx)
	if err != nil {
		return err
	}
	for _, project := range r.fleet.Projects {
		if err := r.converge(ctx, project, observations[project.Name]); err != nil {
			log.Error("converge failed", "project", project.Name, "error", err)
		}
	}
	return nil
}

// observed captures one fleet entry's live state (§5.1 step 1): health,
// server version, workspace presence, SSE stream status.
type observed struct {
	healthy     bool
	version     crushapi.VersionInfo
	workspaceID string
	childExited bool
}

// observe polls /v1/health and /v1/version for every fleet entry.
func (r *Reconciler) observe(ctx context.Context) (map[string]observed, error) {
	observations := make(map[string]observed, len(r.fleet.Projects))
	for _, project := range r.fleet.Projects {
		obs := observed{}
		baseURL := serverURL(project)
		if err := r.client.Health(ctx, baseURL); err == nil {
			obs.healthy = true
			if version, err := r.client.Version(ctx, baseURL); err == nil {
				obs.version = version
			}
		}
		if instance, err := r.st.Instance(ctx, project.Name); err == nil {
			obs.workspaceID = instance.WorkspaceID
		}
		if child, ok := r.procs.Child(project.Name); ok {
			obs.childExited = child.Exited()
		}
		observations[project.Name] = obs
	}
	return observations, nil
}

// converge acts on one fleet entry to move observed state toward desired
// state (§5.1 step 3). Ordering inside converge: verify instance ->
// fingerprint check -> workspace create/adopt -> stream attach ->
// drain/restart/quarantine as needed.
func (r *Reconciler) converge(ctx context.Context, project config.Project, obs observed) error {
	instance, err := r.persistedInstance(ctx, project)
	if err != nil {
		return err
	}

	// A supervised child that died takes its instance down with it; the
	// exit is recorded for crash-loop quarantine input (§5.1).
	if obs.childExited && instance.State != model.InstanceFailed &&
		instance.State != model.InstanceStopped && instance.State != model.InstanceDraining {
		r.recordRestart(project.Name)
		if err := r.quarantineIfNeeded(ctx, project.Name); err != nil {
			return err
		}
		if err := r.closeStream(project.Name); err != nil {
			log.Warn("stream close on child exit failed", "project", project.Name, "error", err)
		}
		r.markRunsUnknown(ctx, project.Name)
		return r.fail(ctx, project.Name, "crush server process exited")
	}

	switch instance.State {
	case model.InstanceStopped:
		if r.hasDemand(ctx, project.Name) {
			return r.startInstance(ctx, project)
		}
		return nil
	case model.InstanceStarting:
		return r.convergeStarting(ctx, project, obs)
	case model.InstanceReady, model.InstanceBusy:
		return r.convergeLive(ctx, project, instance, obs)
	case model.InstanceApprovalRequired:
		return r.convergeApproval(ctx, project, instance)
	case model.InstanceVersionMismatch:
		return r.convergeVersionMismatch(ctx, project, instance, obs)
	case model.InstanceDraining:
		return r.drainInstance(ctx, project, model.DrainGraceful)
	case model.InstanceFailed:
		// Only explicit operator reset re-enters stopped (§4.2).
		return nil
	}
	return nil
}

// convergeStarting completes or fails an in-flight instance start.
func (r *Reconciler) convergeStarting(ctx context.Context, project config.Project, obs observed) error {
	if !obs.healthy {
		// Health is awaited inside startInstance; a starting state seen
		// here without health means the start path failed elsewhere.
		return nil
	}
	if err := r.adoptLive(ctx, project, obs); err != nil {
		return err
	}
	// adoptLive may have placed the instance in approval_required or
	// version_mismatch; only a still-starting instance becomes ready.
	instance, err := r.persistedInstance(ctx, project)
	if err != nil {
		return err
	}
	if instance.State != model.InstanceStarting {
		return nil
	}
	return r.st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.SetInstanceState(ctx, project.Name, model.InstanceReady, instance.Generation)
	})
}

// convergeLive verifies a live instance and keeps its claim attached.
func (r *Reconciler) convergeLive(ctx context.Context, project config.Project, instance model.Instance, obs observed) error {
	if !obs.healthy {
		// The server went away without a supervised exit (adopted server
		// or external kill): in-flight runs become unknown (§7).
		r.closeStream(project.Name)
		r.markRunsUnknown(ctx, project.Name)
		return r.fail(ctx, project.Name, "server stopped responding")
	}
	if err := r.verifyInstance(ctx, project, obs); err != nil {
		return err
	}
	if err := r.checkFingerprint(ctx, project); err != nil {
		return err
	}
	// The workspace may have expired under the stream (C1); adoptLive
	// recreates it and re-attaches.
	if instance.WorkspaceID == "" {
		return r.adoptLive(ctx, project, obs)
	}
	if _, err := r.client.GetWorkspace(ctx, serverURL(project), instance.WorkspaceID); err != nil {
		log.Warn("workspace expired under the stream; marking in-flight runs unknown and recreating",
			"project", project.Name, "workspace", instance.WorkspaceID)
		r.closeStream(project.Name)
		r.markRunsUnknown(ctx, project.Name)
		return r.adoptLive(ctx, project, obs)
	}
	r.AttachStream(ctx, project)
	return nil
}

// convergeApproval resumes reconciliation once the operator approved the
// changed fingerprint (§5.1): busy while a nonterminal run still owns
// serialization, otherwise stopped for the demand path.
func (r *Reconciler) convergeApproval(ctx context.Context, project config.Project, instance model.Instance) error {
	fingerprint, found, err := r.st.ApprovedFingerprint(ctx, project.Name)
	if err != nil || !found {
		return err
	}
	current, err := ComputeFingerprint(project, r.mm)
	if err != nil {
		return err
	}
	if !sameFingerprint(fingerprint, current) {
		return nil // a different change is pending approval
	}
	target := model.InstanceStopped
	if r.hasActiveRun(ctx, project.Name) {
		target = model.InstanceBusy
	}
	return r.st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.SetInstanceState(ctx, project.Name, target, instance.Generation)
	})
}

// convergeVersionMismatch resumes only after an approved binary change
// or a recorded compatibility override (§5.1, §9.3); the restart loop is
// never entered with the same binary.
func (r *Reconciler) convergeVersionMismatch(ctx context.Context, project config.Project, instance model.Instance, obs observed) error {
	if !obs.healthy {
		return nil
	}
	override, err := r.st.CompatibilityOverride(ctx, obs.version.Version, obs.version.BuildID)
	if err != nil || !override {
		return err
	}
	return r.st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.SetInstanceState(ctx, project.Name, model.InstanceStopped, instance.Generation)
	})
}

// startInstance spawns a missing server: argv is constructed from typed
// crush_options plus Matchmaker's authoritative
// --host tcp://127.0.0.1:{port}. The crush binary is resolved once at
// daemon startup, never per spawn; free-form server arguments are
// rejected. The project path is supplied only to POST /v1/workspaces,
// never as a server flag.
func (r *Reconciler) startInstance(ctx context.Context, project config.Project) error {
	baseURL := serverURL(project)
	instance, err := r.persistedInstance(ctx, project)
	if err != nil {
		return err
	}
	generation := instance.Generation + 1
	if err := r.st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.SetInstanceState(ctx, project.Name, model.InstanceStarting, generation)
	}); err != nil {
		return err
	}
	argv, env, err := buildChildArgv(project, r.mm)
	if err != nil {
		return err
	}
	if err := r.awaitStart(ctx, project, argv, env); err != nil {
		return err
	}
	obs := observed{healthy: true}
	if version, err := r.client.Version(ctx, baseURL); err == nil {
		obs.version = version
	}
	if err := r.adoptLive(ctx, project, obs); err != nil {
		return err
	}
	// adoptLive may have placed the instance in approval_required or
	// version_mismatch; only a still-starting instance becomes ready.
	instance, err = r.persistedInstance(ctx, project)
	if err != nil {
		return err
	}
	if instance.State != model.InstanceStarting {
		return nil
	}
	return r.st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.SetInstanceState(ctx, project.Name, model.InstanceReady, generation)
	})
}

// awaitStart spawns and health-checks the server under the configured
// instance-start retry policy: each failed start records a restart for
// crash-loop quarantine; exhausted retries fail the instance and abandon
// its queued attempts (DESIGN §5.1, §7, §4.5).
func (r *Reconciler) awaitStart(ctx context.Context, project config.Project, argv, env []string) error {
	policy := r.mm.Reconcile.InstanceStartRetry
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		startErr := func() error {
			child, err := r.procs.Spawn(project.Name, argv, env)
			if err != nil {
				return err
			}
			healthErr := awaitHealth(ctx, r.client, serverURL(project), r.mm.Reconcile.HealthTimeout)
			if healthErr != nil && child.Exited() {
				// A child that dies during startup usually failed to parse its
				// crushrc under the child environment; surface the exit status.
				return fmt.Errorf("%w (server child exited early: %v)", healthErr, child.ExitErr())
			}
			return healthErr
		}()
		if startErr == nil {
			return nil
		}
		r.recordRestart(project.Name)
		log.Warn("instance start attempt failed", "project", project.Name,
			"attempt", attempt, "error", startErr)
		if err := r.quarantineIfNeeded(ctx, project.Name); err != nil {
			return err
		}
		if attempt >= policy.MaxAttempts {
			r.abandonQueued(ctx, project.Name)
			if err := r.fail(ctx, project.Name, "instance start retries exhausted"); err != nil {
				return err
			}
			return errors.New("instance start failed")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(policy.Backoff):
		}
	}
}

// abandonQueued moves every queued attempt targeting an unstartable
// instance to abandoned; no serialization was acquired (DESIGN §4.5, §7).
func (r *Reconciler) abandonQueued(ctx context.Context, project string) {
	runs, err := r.st.ActiveRuns(ctx)
	if err != nil {
		log.Error("abandon scan failed", "project", project, "error", err)
		return
	}
	for _, run := range runs {
		if run.Project != project || run.Status != model.RunQueued {
			continue
		}
		err := r.st.WithinTx(ctx, func(tx *store.Tx) error {
			return tx.UpdateRunStatus(ctx, run.ID, model.RunQueued, model.RunAbandoned)
		})
		if err != nil {
			log.Warn("queued abandon failed", "run", run.ID, "error", err)
			continue
		}
		log.Warn("queued attempt abandoned after startup retry exhaustion",
			"project", project, "run", run.ID)
	}
}

// adoptLive verifies a responding server, checks the fingerprint, and
// creates or adopts its workspace before attaching the stream. It is the
// shared path of startup adoption and instance start.
func (r *Reconciler) adoptLive(ctx context.Context, project config.Project, obs observed) error {
	if err := r.verifyInstance(ctx, project, obs); err != nil {
		return err
	}
	if err := r.checkFingerprint(ctx, project); err != nil {
		return err
	}
	instance, err := r.persistedInstance(ctx, project)
	if err != nil {
		return err
	}
	baseURL := serverURL(project)
	workspace, err := r.ensureWorkspace(ctx, project, instance)
	if err != nil {
		return r.fail(ctx, project.Name, "workspace create/adopt failed: "+err.Error())
	}
	if err := r.st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.SetInstanceEndpoint(ctx, project.Name, baseURL, project.Port, workspace.ID)
	}); err != nil {
		return err
	}
	r.AttachStream(ctx, project)
	r.reconcileUnknownRuns(ctx, project)
	return nil
}

// reconcileUnknownRuns resolves unknown attempts against their durable
// session references after adoption or restart (DESIGN §5.3 recovery;
// never resubmit).
func (r *Reconciler) reconcileUnknownRuns(ctx context.Context, project config.Project) {
	runs, err := r.st.ActiveRuns(ctx)
	if err != nil {
		return
	}
	for _, run := range runs {
		if run.Project != project.Name || run.Status != model.RunUnknown {
			continue
		}
		if err := r.recovery.ReconcileUnknownRun(ctx, run); err != nil {
			log.Warn("unknown run reconciliation failed", "run", run.ID, "error", err)
		}
	}
}

// ensureWorkspace adopts the persisted workspace when it still serves
// this project, or creates a fresh one (C1 expiry or first start).
func (r *Reconciler) ensureWorkspace(ctx context.Context, project config.Project, instance model.Instance) (crushapi.Workspace, error) {
	baseURL := serverURL(project)
	if instance.WorkspaceID != "" {
		if workspace, err := r.client.GetWorkspace(ctx, baseURL, instance.WorkspaceID); err == nil {
			if workspace.Path == project.Path && workspace.DataDir == project.CrushOptions.DataDir {
				return workspace, nil
			}
			return crushapi.Workspace{}, fmt.Errorf("workspace %s mismatched (path %q data_dir %q)",
				instance.WorkspaceID, workspace.Path, workspace.DataDir)
		}
		// The workspace expired (C1); recreate below.
	}
	return r.client.CreateWorkspace(ctx, baseURL, project.Path, project.CrushOptions.DataDir, nil)
}

// verifyInstance validates after startup or adoption: the configured
// loopback endpoint is the one responding; /v1/version and build_id match
// the approved compatibility policy; the canonical workspace path and
// effective typed options match the fleet entry. Version/build mismatch
// -> version_mismatch; bind, path, or option mismatch -> failed. Neither
// state is driven, adopted, or dispatched to (DESIGN §5.1).
func (r *Reconciler) verifyInstance(ctx context.Context, project config.Project, obs observed) error {
	if !obs.healthy {
		return nil
	}
	version, err := r.client.Version(ctx, serverURL(project))
	if err != nil {
		return err
	}
	pins := r.mm.Fleet
	versionOK := version.Version == pins.ApprovedVersion &&
		(pins.ApprovedBuildID == "" || version.BuildID == pins.ApprovedBuildID)
	if !versionOK {
		override, err := r.st.CompatibilityOverride(ctx, version.Version, version.BuildID)
		if err != nil {
			return err
		}
		if !override {
			instance, err := r.persistedInstance(ctx, project)
			if err != nil {
				return err
			}
			return r.st.WithinTx(ctx, func(tx *store.Tx) error {
				return tx.SetInstanceState(ctx, project.Name, model.InstanceVersionMismatch, instance.Generation)
			})
		}
	}
	return nil
}

// ComputeFingerprint digests all project and global Crush configuration
// that can affect the server or workspace: applicable crushrc/.crushrc,
// legacy JSON still loaded by the pinned version, the selected environment
// allowlist, resolved binary identity, and the effective data directory.
// Additions, removals, and changes all affect it. Matchmaker never
// creates, modifies, or relies on JSON configuration.
func ComputeFingerprint(project config.Project, mm *config.Matchmaker) (model.Fingerprint, error) {
	candidates, err := fingerprintPaths(project, mm)
	if err != nil {
		return model.Fingerprint{}, err
	}
	fingerprint := model.Fingerprint{Project: project.Name}
	for _, path := range candidates {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.IsDir() {
			continue
		}
		digest, err := fileDigest(path)
		if err != nil {
			return model.Fingerprint{}, err
		}
		fingerprint.Entries = append(fingerprint.Entries, model.FingerprintEntry{Path: path, Digest: digest})
	}
	// Binary identity and the environment allowlist are recorded as
	// entries too, so binary swaps and allowlist changes affect the
	// fingerprint.
	fingerprint.Entries = append(fingerprint.Entries,
		model.FingerprintEntry{Path: "binary:" + mm.CrushBinary, Digest: binaryIdentity(mm.CrushBinary)},
		model.FingerprintEntry{Path: "env:allowlist", Digest: envAllowlistDigest(mm)},
		model.FingerprintEntry{Path: "data_dir:" + project.CrushOptions.DataDir, Digest: "-"},
	)
	sort.Slice(fingerprint.Entries, func(i, j int) bool {
		return fingerprint.Entries[i].Path < fingerprint.Entries[j].Path
	})
	return fingerprint, nil
}

// fingerprintPaths lists every config file that can affect the server or
// workspace: project-local crushrc/.crushrc/legacy JSON, and the global
// equivalents (DESIGN §5.1, C7; verified v0.94.1 config loading order).
func fingerprintPaths(project config.Project, mm *config.Matchmaker) ([]string, error) {
	paths := []string{
		filepath.Join(project.Path, "crushrc"),
		filepath.Join(project.Path, ".crushrc"),
		filepath.Join(project.Path, "crush.json"),
	}
	global, err := globalConfigDir()
	if err != nil {
		return nil, err
	}
	if global != "" {
		paths = append(paths,
			filepath.Join(global, "crushrc"),
			filepath.Join(global, ".crushrc"),
			filepath.Join(global, "crush.json"),
		)
	}
	return paths, nil
}

// globalConfigDir resolves Crush's global config directory.
func globalConfigDir() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "crush"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "crush"), nil
}

// fileDigest is the hex SHA-256 of one file's content.
func fileDigest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// binaryIdentity records the resolved binary's size and mtime; full
// content hashing is covered by the version/build pin (§9.3).
func binaryIdentity(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "missing"
	}
	return fmt.Sprintf("size=%d mtime=%d", info.Size(), info.ModTime().UnixNano())
}

// envAllowlistDigest pins the child environment allowlist.
func envAllowlistDigest(mm *config.Matchmaker) string {
	entries := append([]string{envDetachGrace, envIdleTimeout}, baseEnvKeys()...)
	// Declared pass_env names join the digest: adding or removing a name
	// requires renewed operator approval, while credential rotation (a
	// value change) does not.
	for _, name := range mm.PassEnv {
		entries = append(entries, "pass_env:"+name)
	}
	sort.Strings(entries)
	sum := sha256.Sum256([]byte(strings.Join(entries, "\n") + "\n"))
	return hex.EncodeToString(sum[:])
}

// checkFingerprint compares the current fingerprint with the approved one;
// a difference warns with the changed paths, places the instance in
// approval_required, and refuses unattended workspace lifecycle and new
// dispatch. Existing runs are not interrupted (DESIGN §5.1).
func (r *Reconciler) checkFingerprint(ctx context.Context, project config.Project) error {
	current, err := ComputeFingerprint(project, r.mm)
	if err != nil {
		return err
	}
	approved, found, err := r.st.ApprovedFingerprint(ctx, project.Name)
	if err != nil {
		return err
	}
	if found && sameFingerprint(approved, current) {
		return nil
	}
	changed := changedPaths(approved, current)
	if !found {
		changed = entryPaths(current)
	}
	log.Warn("project crush config awaits operator approval",
		"project", project.Name, "changed", loggingField(changed))
	instance, err := r.persistedInstance(ctx, project)
	if err != nil {
		return err
	}
	return r.st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.SetInstanceState(ctx, project.Name, model.InstanceApprovalRequired, instance.Generation)
	})
}

// approveFingerprint records an explicit operator approval and resumes
// reconciliation. Approval attests only that the operator reviewed the
// config; it does not make that config safe.
func (r *Reconciler) approveFingerprint(ctx context.Context, project string) error {
	entry, ok := r.fleetProject(project)
	if !ok {
		return fmt.Errorf("project %q not in fleet", project)
	}
	current, err := ComputeFingerprint(entry, r.mm)
	if err != nil {
		return err
	}
	current.ApprovedBy = "operator"
	return r.st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.SetApprovedFingerprint(ctx, current)
	})
}

// drainInstance runs the stop/restart drain sequence (DESIGN §5.1):
// transition to draining, reject new dispatch, cancel active runs only
// when the mode is forced, otherwise await terminal states, close the
// workspace SSE stream, release the workspace hold with DELETE
// /v1/workspaces, await removal, call shutdown_if_idle, and escalate to
// process signals only after bounded teardown timeouts. The
// process-wide client identity is never retired to stop one instance.
func (r *Reconciler) drainInstance(ctx context.Context, project config.Project, mode model.DrainMode) error {
	instance, err := r.persistedInstance(ctx, project)
	if err != nil {
		return err
	}
	if err := r.st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.SetInstanceState(ctx, project.Name, model.InstanceDraining, instance.Generation)
	}); err != nil {
		return err
	}
	if mode == model.DrainForce {
		if err := r.cancelActiveRuns(ctx, project.Name); err != nil {
			return err
		}
	}
	if err := r.awaitRunsTerminal(ctx, project.Name, r.mm.Reconcile.DrainTimeout); err != nil {
		log.Warn("drain deadline exceeded with active runs", "project", project.Name)
	}
	r.closeStream(project.Name)
	baseURL := serverURL(project)
	if instance.WorkspaceID != "" {
		if err := r.client.DeleteWorkspace(ctx, baseURL, instance.WorkspaceID); err != nil {
			log.Warn("workspace release failed", "project", project.Name, "error", err)
		}
		if err := awaitRemoval(ctx, r.client, baseURL, instance.WorkspaceID, r.mm.Reconcile.DrainTimeout); err != nil {
			log.Warn("workspace removal not observed", "project", project.Name, "error", err)
		}
	}
	if err := r.client.Control(ctx, baseURL, crushapi.ControlShutdownIfIdle); err != nil {
		log.Warn("shutdown_if_idle refused", "project", project.Name, "error", err)
		// Escalate to supervised process signals only after the bounded
		// teardown timeout (DESIGN §5.1).
		if child, ok := r.procs.Child(project.Name); ok {
			if err := child.Signal(sigTerm); err != nil {
				log.Warn("SIGTERM escalation failed", "project", project.Name, "error", err)
			}
			select {
			case <-child.Done():
			case <-time.After(r.mm.Reconcile.DrainTimeout):
				if err := child.Signal(sigKill); err != nil {
					log.Warn("SIGKILL escalation failed", "project", project.Name, "error", err)
				}
			}
		}
	}
	if child, ok := r.procs.Child(project.Name); ok {
		child.Stop()
	}
	// The stopped transition can race other shutting-down components'
	// final store writes; one retry absorbs the contention.
	err = r.st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.SetInstanceState(ctx, project.Name, model.InstanceStopped, instance.Generation)
	})
	if err != nil {
		select {
		case <-ctx.Done():
			return err
		case <-time.After(drainWriteBackoff):
		}
		return r.st.WithinTx(ctx, func(tx *store.Tx) error {
			return tx.SetInstanceState(ctx, project.Name, model.InstanceStopped, instance.Generation)
		})
	}
	return nil
}

// cancelActiveRuns begins operator-cause cancellation for every active
// run on the instance (forced drain; DESIGN §5.1, §5.3).
func (r *Reconciler) cancelActiveRuns(ctx context.Context, project string) error {
	runs, err := r.st.ActiveRuns(ctx)
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.Project != project || run.Status != model.RunRunning {
			continue
		}
		if err := r.st.WithinTx(ctx, func(tx *store.Tx) error {
			return tx.SetRunCancel(ctx, run.ID, model.CancelCauseOperator)
		}); err != nil {
			log.Warn("forced cancel failed", "run", run.ID, "error", err)
			continue
		}
		if err := r.client.CancelSession(ctx, run.ServerURL, run.WorkspaceID, run.SessionID); err != nil {
			log.Warn("cancel request failed", "run", run.ID, "error", err)
		}
	}
	return nil
}

// awaitRunsTerminal waits until no non-queued active run remains or the
// deadline expires.
func (r *Reconciler) awaitRunsTerminal(ctx context.Context, project string, deadline time.Duration) error {
	timeout := time.After(deadline)
	for {
		runs, err := r.st.ActiveRuns(ctx)
		if err != nil {
			return err
		}
		active := false
		for _, run := range runs {
			if run.Project == project && run.Status != model.RunQueued {
				active = true
				break
			}
		}
		if !active {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return errors.New("drain deadline exceeded")
		case <-time.After(removePollEvery):
		}
	}
}

// quarantineIfNeeded counts restarts per window and moves crash-looping
// instances into failed (DESIGN §5.1).
func (r *Reconciler) quarantineIfNeeded(ctx context.Context, project string) error {
	r.mu.Lock()
	windowStart := time.Now().Add(-r.mm.Reconcile.CrashLoopWindow)
	recent := make([]time.Time, 0, len(r.restarts[project]))
	for _, at := range r.restarts[project] {
		if at.After(windowStart) {
			recent = append(recent, at)
		}
	}
	r.restarts[project] = recent
	count := len(recent)
	r.mu.Unlock()
	if count < r.mm.Reconcile.MaxRestartsPerWindow {
		return nil
	}
	return r.fail(ctx, project, fmt.Sprintf("crash loop: %d restarts in %s", count, r.mm.Reconcile.CrashLoopWindow))
}

// recordRestart tracks one restart time for crash-loop quarantine.
func (r *Reconciler) recordRestart(project string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.restarts[project] = append(r.restarts[project], time.Now())
}

// DemandStart requests that the reconciliation loop start a stopped
// instance because a queued run targets it (DESIGN §5.3 dispatch).
func (r *Reconciler) DemandStart(project string) {
	select {
	case r.demand <- project:
	default:
	}
}

// Shutdown performs the orchestrator shutdown: drain all instances in
// parallel under a bounded global deadline, release streams and workspace
// holds, then retire the process-wide client claim with
// DELETE /v1/clients/{client_id} as final cleanup (DESIGN §5.1). Forced
// mode cancels active runs; graceful mode awaits their terminal states.
func (r *Reconciler) Shutdown(ctx context.Context, mode model.DrainMode) error {
	var wg sync.WaitGroup
	for _, project := range r.fleet.Projects {
		wg.Add(1)
		go func(project config.Project) {
			defer wg.Done()
			if err := r.drainInstance(ctx, project, mode); err != nil {
				log.Error("shutdown drain failed", "project", project.Name, "error", err)
			}
		}(project)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(r.mm.Reconcile.GlobalDrainDeadline):
		return errors.New("global drain deadline exceeded")
	case <-ctx.Done():
		return ctx.Err()
	}
	for _, project := range r.fleet.Projects {
		if err := r.client.DeleteClient(ctx, serverURL(project)); err != nil {
			// The drain above already stopped every server; a refused
			// connection at retirement is the expected end state.
			if !connectionRefused(err) {
				log.Warn("client retirement failed", "project", project.Name, "error", err)
			}
		}
	}
	return nil
}

// connectionRefused reports whether err is a refused TCP connection.
func connectionRefused(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && errors.Is(opErr.Err, syscall.ECONNREFUSED)
}

// AttachStream creates or re-attaches the workspace SSE stream with
// backoff, re-creating the workspace if it was torn down (C1). The stream
// handler is supplied by the supervision package.
func (r *Reconciler) AttachStream(ctx context.Context, project config.Project) error {
	r.mu.Lock()
	_, attached := r.streams[project.Name]
	reattaching := r.reattaching[project.Name]
	if attached || reattaching {
		r.mu.Unlock()
		return nil
	}
	r.reattaching[project.Name] = true
	r.mu.Unlock()
	go r.streamLoop(ctx, project)
	return nil
}

// streamLoop keeps one workspace's SSE claim attached until the context
// ends: attach, consume until stream loss, mark in-flight runs unknown,
// and re-attach with backoff (DESIGN §5.3).
func (r *Reconciler) streamLoop(ctx context.Context, project config.Project) {
	defer func() {
		r.mu.Lock()
		delete(r.reattaching, project.Name)
		r.mu.Unlock()
	}()
	backoff := r.mm.Reconcile.SSEReconnectBackoff
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return
		}
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff * time.Duration(min(attempt, 10))):
			}
		}
		instance, err := r.persistedInstance(ctx, project)
		if err != nil || instance.WorkspaceID == "" {
			continue
		}
		stream, err := r.client.StreamEvents(ctx, serverURL(project), instance.WorkspaceID)
		if err != nil {
			log.Warn("SSE attach failed; retrying with backoff",
				"project", project.Name, "error", err)
			continue
		}
		r.mu.Lock()
		r.streams[project.Name] = stream
		r.mu.Unlock()
		instance.Generation = max(instance.Generation, 1)
		err = r.consumer.Consume(ctx, stream, instance)
		r.mu.Lock()
		delete(r.streams, project.Name)
		r.mu.Unlock()
		stream.Close()
		if ctx.Err() != nil {
			return
		}
		// Stream loss: in-flight runs become unknown and reconcile from
		// persisted session IDs (DESIGN §5.3, §7); never resubmit.
		r.markRunsUnknown(ctx, project.Name)
	}
}

// closeStream detaches one project's stream without state changes.
func (r *Reconciler) closeStream(project string) error {
	r.mu.Lock()
	stream, attached := r.streams[project]
	delete(r.streams, project)
	r.mu.Unlock()
	if !attached {
		return nil
	}
	return stream.Close()
}

// markRunsUnknown moves every dispatched in-flight run of a project into
// unknown after crash or stream loss (DESIGN §4.5, §7).
func (r *Reconciler) markRunsUnknown(ctx context.Context, project string) {
	runs, err := r.st.ActiveRuns(ctx)
	if err != nil {
		log.Error("marking runs unknown failed", "project", project, "error", err)
		return
	}
	for _, run := range runs {
		if run.Project != project {
			continue
		}
		switch run.Status {
		case model.RunDispatching, model.RunRunning, model.RunCancelling:
		default:
			continue
		}
		err := r.st.WithinTx(ctx, func(tx *store.Tx) error {
			return tx.UpdateRunStatus(ctx, run.ID, run.Status, model.RunUnknown)
		})
		if err != nil {
			log.Warn("run -> unknown failed", "run", run.ID, "error", err)
		}
	}
}

// hasDemand reports whether any nonterminal run targets the project:
// queued runs demand a start, and dispatched runs (including unknown
// ones awaiting reconciliation) demand the instance stay up. This is the
// store-mediated demand signal of dispatch (plan M5).
func (r *Reconciler) hasDemand(ctx context.Context, project string) bool {
	runs, err := r.st.ActiveRuns(ctx)
	if err != nil {
		return false
	}
	for _, run := range runs {
		if run.Project == project && model.RunIsNonterminal(run.Status) {
			return true
		}
	}
	return false
}

// hasActiveRun reports whether any dispatched nonterminal run holds the
// instance's serialization slot.
func (r *Reconciler) hasActiveRun(ctx context.Context, project string) bool {
	runs, err := r.st.ActiveRuns(ctx)
	if err != nil {
		return false
	}
	for _, run := range runs {
		if run.Project == project && run.Status != model.RunQueued {
			return true
		}
	}
	return false
}

// fail records a terminal failure reason and state.
func (r *Reconciler) fail(ctx context.Context, project, reason string) error {
	instance, err := r.persistedInstance(ctx, config.Project{Name: project})
	if err != nil {
		return err
	}
	if err := r.st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.SetInstanceState(ctx, project, model.InstanceFailed, instance.Generation)
	}); err != nil {
		return err
	}
	log.Error("instance failed", "project", project, "reason", reason)
	return nil
}

// persistedInstance loads the store record or a stopped default.
func (r *Reconciler) persistedInstance(ctx context.Context, project config.Project) (model.Instance, error) {
	instance, err := r.st.Instance(ctx, project.Name)
	if err == nil {
		return instance, nil
	}
	return model.Instance{Project: project.Name, State: model.InstanceStopped, Generation: 0}, nil
}

// fleetProject finds the fleet entry by name.
func (r *Reconciler) fleetProject(name string) (config.Project, bool) {
	for _, project := range r.fleet.Projects {
		if project.Name == name {
			return project, true
		}
	}
	return config.Project{}, false
}

// InstanceState exposes the persisted instance state to dispatch gating.
func (r *Reconciler) InstanceState(ctx context.Context, project string) (model.InstanceState, error) {
	instance, err := r.persistedInstance(ctx, config.Project{Name: project})
	if err != nil {
		return model.InstanceStopped, err
	}
	return instance.State, nil
}

// serverURL derives a project's loopback base URL.
func serverURL(project config.Project) string {
	return fmt.Sprintf("http://127.0.0.1:%d", project.Port)
}

// buildChildArgv constructs the spawn argv from typed crush_options plus
// Matchmaker's authoritative host flag, and the environment allowlist.
func buildChildArgv(project config.Project, mm *config.Matchmaker) ([]string, []string, error) {
	argv := []string{mm.CrushBinary, serverSubcmd, hostFlag, "tcp://127.0.0.1:" + fmt.Sprint(project.Port)}
	if project.CrushOptions.Debug != nil && *project.CrushOptions.Debug {
		argv = append(argv, debugFlag)
	}
	if project.CrushOptions.DataDir != "" {
		argv = append(argv, dataDirFlag, project.CrushOptions.DataDir)
	}
	env := append(baseEnvKeys(), envDetachGrace, envIdleTimeout)
	env = append(env, passThroughEnv(mm)...)
	return argv, env, nil
}

// passThroughEnv copies each operator-declared pass_env variable from the
// daemon environment into the child (DESIGN §5.1): the child never inherits
// the full parent environment, but global crushrc files legitimately
// reference credentials like provider API keys. Declared-but-unset
// variables are omitted; the crushrc's own guards surface their absence.
func passThroughEnv(mm *config.Matchmaker) []string {
	var passed []string
	for _, name := range mm.PassEnv {
		if value, ok := os.LookupEnv(name); ok {
			passed = append(passed, name+"="+value)
		}
	}
	return passed
}

// baseEnvKeys lists the inherited-from-parent allowlist keys.
func baseEnvKeys() []string {
	home, homeErr := os.UserHomeDir()
	if homeErr != nil {
		home = "/root"
	}
	return []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
}

// awaitHealth polls readiness until the deadline.
func awaitHealth(ctx context.Context, client *crushapi.Client, baseURL string, timeout time.Duration) error {
	deadline := time.After(timeout)
	for {
		if err := client.Health(ctx, baseURL); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return errors.New("health check timed out")
		case <-time.After(healthPollEvery):
		}
	}
}

// awaitRemoval polls until the workspace is gone (C1 teardown).
func awaitRemoval(ctx context.Context, client *crushapi.Client, baseURL, workspaceID string, timeout time.Duration) error {
	deadline := time.After(timeout)
	for {
		if _, err := client.GetWorkspace(ctx, baseURL, workspaceID); err != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return errors.New("workspace removal timed out")
		case <-time.After(removePollEvery):
		}
	}
}

// sameFingerprint compares two fingerprints by entries.
func sameFingerprint(a, b model.Fingerprint) bool {
	return slices.Equal(entryPaths(a), entryPaths(b)) &&
		slices.Equal(entryDigests(a), entryDigests(b))
}

// entryPaths lists a fingerprint's entry paths in stored order.
func entryPaths(f model.Fingerprint) []string {
	paths := make([]string, 0, len(f.Entries))
	for _, entry := range f.Entries {
		paths = append(paths, entry.Path)
	}
	return paths
}

// entryDigests lists a fingerprint's entry digests in stored order.
func entryDigests(f model.Fingerprint) []string {
	digests := make([]string, 0, len(f.Entries))
	for _, entry := range f.Entries {
		digests = append(digests, entry.Digest)
	}
	return digests
}

// changedPaths reports the entries that differ between approved and
// current fingerprints.
func changedPaths(approved, current model.Fingerprint) []string {
	approvedMap := make(map[string]string, len(approved.Entries))
	for _, entry := range approved.Entries {
		approvedMap[entry.Path] = entry.Digest
	}
	currentMap := make(map[string]string, len(current.Entries))
	for _, entry := range current.Entries {
		currentMap[entry.Path] = entry.Digest
	}
	var changed []string
	for path, digest := range currentMap {
		if approvedDigest, ok := approvedMap[path]; !ok || approvedDigest != digest {
			changed = append(changed, path)
		}
	}
	for path := range approvedMap {
		if _, ok := currentMap[path]; !ok {
			changed = append(changed, path+" (removed)")
		}
	}
	sort.Strings(changed)
	return changed
}

// loggingField bounds a diagnostic list for structured logging.
func loggingField(values []string) string {
	const maxLen = 256
	joined := fmt.Sprintf("%v", values)
	if len(joined) > maxLen {
		return joined[:maxLen]
	}
	return joined
}
