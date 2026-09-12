// Package fleet implements fleet reconciliation: observe, compare,
// converge over one crush server per project, plus the supervised child
// process lifecycle (DESIGN §5.1, §10.1).
package fleet

import (
	"context"

	"github.com/pshickeydev/matchmaker/internal/config"
	"github.com/pshickeydev/matchmaker/internal/crushapi"
	"github.com/pshickeydev/matchmaker/internal/model"
	"github.com/pshickeydev/matchmaker/internal/store"
)

// Reconciler drives the fleet toward desired state. It performs an
// immediate startup reconciliation, then a periodic loop (default ~30s
// tick); it never handles failures reactively per-failure.
type Reconciler struct{}

// New builds a reconciler from the desired-state fleet config, daemon
// configuration, the durable store, and the Crush client.
func New(fleet *config.Fleet, mm *config.Matchmaker, st *store.Store, client *crushapi.Client, procs *Supervisor) *Reconciler {
	panic(notImplemented)
}

// Run blocks until ctx is canceled: adoptOnStartup, then tick forever.
func (r *Reconciler) Run(ctx context.Context) error { panic(notImplemented) }

// adoptOnStartup reloads durable state, probes live servers, verifies
// project config fingerprints before adoption (DESIGN §7), and attaches
// new-process SSE claims to matching workspaces before the periodic timer
// starts, minimizing the detached-stream window and allowing adoption
// during Crush's detach grace (DESIGN §5.1 step 4). An expired workspace
// is recreated and in-flight runs reconcile from durable session IDs;
// ambiguous dispatches are never resubmitted.
func (r *Reconciler) adoptOnStartup(ctx context.Context) error { panic(notImplemented) }

// reconcileOnce runs one observe/compare/converge pass over the fleet.
func (r *Reconciler) reconcileOnce(ctx context.Context) error { panic(notImplemented) }

// observed captures one fleet entry's live state (§5.1 step 1): health,
// server version, workspace presence, SSE stream status.
type observed struct{}

// observe polls /v1/health and /v1/version for every fleet entry.
func (r *Reconciler) observe(ctx context.Context) (map[string]observed, error) {
	panic(notImplemented)
}

// converge acts on one fleet entry to move observed state toward desired
// state (§5.1 step 3). Ordering inside converge: verify instance ->
// fingerprint check -> workspace create/adopt -> stream attach ->
// drain/restart/quarantine as needed.
func (r *Reconciler) converge(ctx context.Context, project config.Project, obs observed) error {
	panic(notImplemented)
}

// startInstance spawns a missing server: argv is constructed from typed
// crush_options plus Matchmaker's authoritative
// --host tcp://127.0.0.1:{port}. The crush binary is resolved once at
// daemon startup, never per spawn; free-form server arguments are
// rejected. The project path is supplied only to POST /v1/workspaces,
// never as a server flag.
func (r *Reconciler) startInstance(ctx context.Context, project config.Project) error {
	panic(notImplemented)
}

// verifyInstance validates after startup or adoption: the configured
// loopback endpoint is the one responding; /v1/version and build_id match
// the approved compatibility policy; the canonical workspace path and
// effective typed options match the fleet entry. Version/build mismatch
// -> version_mismatch; bind, path, or option mismatch -> failed. Neither
// state is driven, adopted, or dispatched to (DESIGN §5.1).
func (r *Reconciler) verifyInstance(ctx context.Context, project config.Project) error {
	panic(notImplemented)
}

// ComputeFingerprint digests all project and global Crush configuration
// that can affect the server or workspace: applicable crushrc/.crushrc,
// legacy JSON still loaded by the pinned version, the selected environment
// allowlist, resolved binary identity, and the effective data directory.
// Additions, removals, and changes all affect it. Matchmaker never
// creates, modifies, or relies on JSON configuration.
func ComputeFingerprint(project config.Project, mm *config.Matchmaker) (model.Fingerprint, error) {
	panic(notImplemented)
}

// checkFingerprint compares the current fingerprint with the approved one;
// a difference warns with the changed paths, places the instance in
// approval_required, and refuses unattended workspace lifecycle and new
// dispatch. Existing runs are not interrupted (DESIGN §5.1).
func (r *Reconciler) checkFingerprint(ctx context.Context, project config.Project) error {
	panic(notImplemented)
}

// approveFingerprint records an explicit operator approval and resumes
// reconciliation. Approval attests only that the operator reviewed the
// config; it does not make the config safe.
func (r *Reconciler) approveFingerprint(ctx context.Context, project string) error {
	panic(notImplemented)
}

// drainInstance runs the stop/restart drain sequence (DESIGN §5.1):
// transition to draining, reject new dispatch, cancel active runs only
// when the mode is forced, otherwise await terminal states, close the
// workspace SSE stream, release the workspace hold with DELETE
// /v1/workspaces, await removal, call shutdown_if_idle, and escalate to
// process signals only after bounded teardown timeouts. The
// process-wide client identity is never retired to stop one instance.
func (r *Reconciler) drainInstance(ctx context.Context, project config.Project, mode model.DrainMode) error {
	panic(notImplemented)
}

// quarantineIfNeeded counts restarts per window and moves crash-looping
// instances into failed (DESIGN §5.1).
func (r *Reconciler) quarantineIfNeeded(ctx context.Context, project string) error {
	panic(notImplemented)
}

// DemandStart requests that the reconciliation loop start a stopped
// instance because a queued run targets it (DESIGN §5.3 dispatch).
func (r *Reconciler) DemandStart(project string) { panic(notImplemented) }

// Shutdown performs the orchestrator shutdown: drain all instances in
// parallel under a bounded global deadline, release streams and workspace
// holds, then retire the process-wide client claim with
// DELETE /v1/clients/{client_id} as final cleanup (DESIGN §5.1). Forced
// mode cancates active runs; graceful mode awaits their terminal states.
func (r *Reconciler) Shutdown(ctx context.Context, mode model.DrainMode) error { panic(notImplemented) }

// AttachStream creates or re-attaches the workspace SSE stream with
// backoff, re-creating the workspace if it was torn down (C1). The stream
// handler is supplied by the supervision package.
func (r *Reconciler) AttachStream(ctx context.Context, project config.Project) error {
	panic(notImplemented)
}

// InstanceState exposes the persisted instance state to dispatch gating.
func (r *Reconciler) InstanceState(ctx context.Context, project string) (model.InstanceState, error) {
	panic(notImplemented)
}
