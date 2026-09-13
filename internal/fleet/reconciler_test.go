package fleet

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/pshickeydev/matchmaker/internal/config"
	"github.com/pshickeydev/matchmaker/internal/crushapi"
	"github.com/pshickeydev/matchmaker/internal/crushtest"
	"github.com/pshickeydev/matchmaker/internal/model"
	"github.com/pshickeydev/matchmaker/internal/store"
)

// stubBinary writes a long-lived stand-in for the crush server binary:
// it ignores argv and sleeps, so the supervised child stays alive while
// the fake server answers health on the project port.
func stubBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "crush-stub")
	script := "#!/bin/sh\nexec sleep 600\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub binary: %v", err)
	}
	return path
}

// testMM builds fast-timing daemon config for integration tests.
func testMM(t *testing.T) *config.Matchmaker {
	return &config.Matchmaker{
		CrushBinary: stubBinary(t),
		Reconcile: config.ReconcileConfig{
			TickInterval:         50 * time.Millisecond,
			SSEReconnectBackoff:  10 * time.Millisecond,
			HealthTimeout:        300 * time.Millisecond,
			DrainTimeout:         500 * time.Millisecond,
			GlobalDrainDeadline:  2 * time.Second,
			MaxRestartsPerWindow: 3,
			CrashLoopWindow:      time.Minute,
			InstanceStartRetry:   config.RetryPolicy{MaxAttempts: 2, Backoff: 10 * time.Millisecond},
		},
		Fleet: config.FleetPins{ApprovedVersion: config.PinnedCrushVersion},
	}
}

// fleetFor builds a one-project fleet pointed at the fake server's port.
func fleetFor(t *testing.T, fake *crushtest.Server) (*config.Fleet, config.Project) {
	t.Helper()
	port, err := strconv.Atoi(strings.TrimPrefix(fake.BaseURL(), "http://127.0.0.1:"))
	if err != nil {
		t.Fatalf("parse fake port: %v", err)
	}
	projectPath := filepath.Join(t.TempDir(), "api")
	if err := os.MkdirAll(projectPath, 0o700); err != nil {
		t.Fatal(err)
	}
	project := config.Project{
		Name: "api",
		Path: projectPath,
		Port: port,
		Tags: []string{"lang:go"},
		CrushOptions: config.CrushOptions{
			DataDir: filepath.Join(projectPath, ".crush"),
		},
	}
	return &config.Fleet{Projects: []config.Project{project}}, project
}

// setup wires a reconciler over a fake server and real store. The
// returned context is the reconcile context: driveOne uses it so stream
// loops die with the test.
func setup(t *testing.T) (*Reconciler, *crushtest.Server, *store.Store, config.Project, context.Context) {
	t.Helper()
	fake := crushtest.New()
	t.Cleanup(fake.Close)
	fleet, project := fleetFor(t, fake)
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	// A private transport keeps pooled keep-alive connections from
	// leaking across tests: the OS recycles ephemeral fake ports, and a
	// stale pooled connection hangs the SSE attach silently.
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	client := crushapi.NewClient(crushapi.NewClientID(), &http.Client{Transport: transport})
	r := New(fleet, testMM(t), st, client, NewSupervisor())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		if child, ok := r.procs.Child(project.Name); ok {
			child.Signal(syscall.SIGKILL)
		}
	})
	return r, fake, st, project, ctx
}

// approve pre-approves the project fingerprint.
func approve(t *testing.T, r *Reconciler, project config.Project) {
	t.Helper()
	fingerprint, err := ComputeFingerprint(project, r.mm)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	fingerprint.ApprovedBy = "operator"
	ctx := context.Background()
	if err := r.st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.SetApprovedFingerprint(ctx, fingerprint)
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}
}

// goalSeq generates unique goal IDs across one test binary.
var goalSeq atomic.Int64

// seedGoal persists a one-step goal targeting the project; its initial
// queued attempt is the demand signal.
func seedGoal(t *testing.T, st *store.Store, project config.Project) model.Run {
	t.Helper()
	ctx := context.Background()
	goalID := fmt.Sprintf("g%d", goalSeq.Add(1))
	goal := model.Goal{
		ID: goalID, Type: model.GoalTypeWork, Status: model.GoalActive,
		Steps: []model.Step{{ID: "a", GoalID: goalID, PromptTemplate: "work",
			Supervision: model.SupervisionDeny, Timeout: time.Minute}},
		FrozenTargets: map[string][]model.ResolvedTarget{
			"a": {{StepID: "a", Project: project.Name, Instance: project.Name,
				ServerURL: serverURL(project), Tags: []string{"lang:go"}}},
		},
	}
	if err := st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.CreateGoal(ctx, goal)
	}); err != nil {
		t.Fatalf("seed goal: %v", err)
	}
	runs, err := st.ActiveRuns(ctx)
	if err != nil || len(runs) != 1 {
		t.Fatalf("seeded runs = %+v err=%v", runs, err)
	}
	return runs[0]
}

// driveOne runs one reconcile pass under the test's reconcile context.
func driveOne(t *testing.T, ctx context.Context, r *Reconciler) {
	t.Helper()
	if err := r.reconcileOnce(ctx); err != nil {
		t.Fatalf("reconcileOnce: %v", err)
	}
}

// driveUntil reconcile-passes until cond holds or the deadline expires.
func driveUntil(t *testing.T, ctx context.Context, r *Reconciler, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		driveOne(t, ctx, r)
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// waitFor polls a condition with a test deadline.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func instanceState(t *testing.T, r *Reconciler, project string) model.InstanceState {
	t.Helper()
	state, err := r.InstanceState(context.Background(), project)
	if err != nil {
		t.Fatalf("instance state: %v", err)
	}
	return state
}

func TestSupervisorSpawnAndSignal(t *testing.T) {
	supervisor := NewSupervisor()
	child, err := supervisor.Spawn("api", []string{"/bin/sleep", "60"}, []string{"HOME=/tmp"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if child.Pid() <= 0 {
		t.Fatalf("pid = %d", child.Pid())
	}
	if child.Exited() {
		t.Fatal("child reported exited immediately")
	}
	if err := child.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	select {
	case <-child.Done():
		if !child.Exited() {
			t.Fatal("done signaled without exit")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("child did not exit after SIGTERM")
	}
	if err := child.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestBuildChildArgv(t *testing.T) {
	t.Setenv("LEAKED_SECRET", "do-not-inherit")
	debug := true
	project := config.Project{
		Name: "api", Port: 41001,
		CrushOptions: config.CrushOptions{Debug: &debug, DataDir: "/data/api"},
	}
	mm := testMM(t)
	argv, env, err := buildChildArgv(project, mm)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{mm.CrushBinary, "server", "--host", "tcp://127.0.0.1:41001", "--debug", "--data-dir", "/data/api"}
	if strings.Join(argv, " ") != strings.Join(want, " ") {
		t.Errorf("argv = %v, want %v", argv, want)
	}
	joined := strings.Join(env, "\n")
	for _, required := range []string{"HOME=", "PATH=", envDetachGrace, envIdleTimeout} {
		if !strings.Contains(joined, required) {
			t.Errorf("env missing %s", required)
		}
	}
	if strings.Contains(joined, "LEAKED_SECRET") {
		t.Error("child environment inherited an unallowlisted variable")
	}
}

func TestChildEnvPassThrough(t *testing.T) {
	t.Setenv("PROVIDER_API_KEY", "credential-value")
	t.Setenv("LEAKED_SECRET", "do-not-inherit")
	project := config.Project{Name: "api", Port: 41001,
		CrushOptions: config.CrushOptions{DataDir: "/data/api"}}
	mm := testMM(t)
	mm.PassEnv = []string{"PROVIDER_API_KEY", "UNSET_DECLARED_VAR"}
	_, env, err := buildChildArgv(project, mm)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "PROVIDER_API_KEY=credential-value") {
		t.Errorf("declared variable not passed through: %s", joined)
	}
	if strings.Contains(joined, "UNSET_DECLARED_VAR") {
		t.Errorf("unset declared variable invented a value: %s", joined)
	}
	if strings.Contains(joined, "LEAKED_SECRET") {
		t.Error("undeclared variable passed through to child")
	}
	// The declared names are part of the deployment fingerprint: adding a
	// name requires renewed operator approval, while credential rotation
	// (a value change) does not.
	withName, err := ComputeFingerprint(project, mm)
	if err != nil {
		t.Fatal(err)
	}
	mm.PassEnv = nil
	withoutName, err := ComputeFingerprint(project, mm)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(entryDigests(withName), ",") == strings.Join(entryDigests(withoutName), ",") {
		t.Error("pass_env name not part of the fingerprint")
	}
}

func TestStartOnDemand(t *testing.T) {
	r, fake, st, project, ctx := setup(t)
	approve(t, r, project)
	seedGoal(t, st, project)

	driveOne(t, ctx, r)
	waitFor(t, "instance ready", func() bool {
		return instanceState(t, r, project.Name) == model.InstanceReady
	})
	instance, err := st.Instance(context.Background(), project.Name)
	if err != nil {
		t.Fatal(err)
	}
	if instance.WorkspaceID == "" || instance.Generation != 1 {
		t.Errorf("instance = %+v", instance)
	}
	if workspace, ok := fake.Workspace(instance.WorkspaceID); !ok || workspace.Path != project.Path {
		t.Errorf("workspace not created for project path: %+v", workspace)
	}
	if workspace, ok := fake.WorkspaceByPath(project.Path); !ok || workspace.DataDir != project.CrushOptions.DataDir {
		t.Errorf("workspace data_dir not explicit: %+v", workspace)
	}
	waitFor(t, "stream attached", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		_, attached := r.streams[project.Name]
		return attached
	})
}

func TestFirstContactRequiresFingerprintApproval(t *testing.T) {
	r, fake, st, project, ctx := setup(t)
	seedGoal(t, st, project)

	driveOne(t, ctx, r)
	waitFor(t, "approval_required", func() bool {
		return instanceState(t, r, project.Name) == model.InstanceApprovalRequired
	})
	if !model.InstanceRejectsDispatch(model.InstanceApprovalRequired) {
		t.Fatal("model no longer rejects dispatch from approval_required")
	}
	if fake.WorkspaceDeletes() != nil {
		t.Error("lifecycle actions taken before approval")
	}
	// Operator approval resumes reconciliation.
	if err := r.approveFingerprint(context.Background(), project.Name); err != nil {
		t.Fatalf("approveFingerprint: %v", err)
	}
	driveUntil(t, ctx, r, "instance ready after approval", func() bool {
		return instanceState(t, r, project.Name) == model.InstanceReady
	})
}

func TestFingerprintChangeBlocksDispatch(t *testing.T) {
	r, _, _, project, ctx := setup(t)
	approve(t, r, project)
	driveOne(t, ctx, r)
	// No demand, no instance record: converged cleanly. Now approve is in
	// place; simulate a project config change and re-converge with a
	// seeded live instance.
	if err := r.st.WithinTx(ctx, func(tx *store.Tx) error {
		if err := tx.SetInstanceEndpoint(ctx, project.Name, serverURL(project), project.Port, "ws-old"); err != nil {
			return err
		}
		if err := tx.SetInstanceState(ctx, project.Name, model.InstanceStarting, 1); err != nil {
			return err
		}
		return tx.SetInstanceState(ctx, project.Name, model.InstanceReady, 1)
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project.Path, ".crushrc"), []byte("# changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	driveOne(t, ctx, r)
	state := instanceState(t, r, project.Name)
	if state != model.InstanceApprovalRequired {
		t.Fatalf("state = %s, want approval_required after fingerprint change", state)
	}
	if !model.InstanceRejectsDispatch(state) {
		t.Fatal("changed fingerprint state does not reject dispatch")
	}
}

func TestVersionMismatchAndOverride(t *testing.T) {
	r, fake, _, project, ctx := setup(t)
	approve(t, r, project)
	// Seed a live adopted instance.
	if err := r.st.WithinTx(ctx, func(tx *store.Tx) error {
		if err := tx.SetInstanceEndpoint(ctx, project.Name, serverURL(project), project.Port, ""); err != nil {
			return err
		}
		return tx.SetInstanceState(ctx, project.Name, model.InstanceStarting, 1)
	}); err != nil {
		t.Fatal(err)
	}
	driveOne(t, ctx, r)
	waitFor(t, "instance live", func() bool {
		state := instanceState(t, r, project.Name)
		return state == model.InstanceReady || state == model.InstanceBusy
	})

	fake.SetVersion("v0.99.0", "rogue-build")
	driveOne(t, ctx, r)
	waitFor(t, "version_mismatch", func() bool {
		return instanceState(t, r, project.Name) == model.InstanceVersionMismatch
	})
	if !model.InstanceRejectsDispatch(model.InstanceVersionMismatch) {
		t.Fatal("version_mismatch does not reject dispatch")
	}
	// The explicit compatibility override records the observed pair and
	// resumes (§9.3).
	if err := r.st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.RecordCompatibilityOverride(ctx, "v0.99.0", "rogue-build", "operator")
	}); err != nil {
		t.Fatal(err)
	}
	driveOne(t, ctx, r)
	waitFor(t, "override resumes to stopped", func() bool {
		return instanceState(t, r, project.Name) == model.InstanceStopped
	})
}

func TestWorkspaceExpiryIsRecreated(t *testing.T) {
	r, fake, st, project, ctx := setup(t)
	approve(t, r, project)
	seedGoal(t, st, project)
	driveOne(t, ctx, r)
	waitFor(t, "instance ready", func() bool {
		return instanceState(t, r, project.Name) == model.InstanceReady
	})
	first, err := st.Instance(context.Background(), project.Name)
	if err != nil {
		t.Fatal(err)
	}
	// C1 teardown: the workspace expired under the stream.
	fake.RemoveWorkspace(first.WorkspaceID)
	driveOne(t, ctx, r)
	waitFor(t, "workspace recreated", func() bool {
		instance, err := st.Instance(context.Background(), project.Name)
		return err == nil && instance.WorkspaceID != "" && instance.WorkspaceID != first.WorkspaceID
	})
	state := instanceState(t, r, project.Name)
	if state != model.InstanceReady && state != model.InstanceBusy {
		t.Errorf("state after recreation = %s", state)
	}
}

func TestStreamLossMarksRunsUnknown(t *testing.T) {
	r, fake, st, project, ctx := setup(t)
	approve(t, r, project)
	seedGoal(t, st, project)
	driveOne(t, ctx, r)
	waitFor(t, "instance ready", func() bool {
		return instanceState(t, r, project.Name) == model.InstanceReady
	})
	// The stream attaches asynchronously; wait for it so the drop hits a
	// registered subscriber instead of racing the attach.
	waitFor(t, "stream attached", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		_, attached := r.streams[project.Name]
		return attached
	})
	runs, _ := st.ActiveRuns(ctx)
	run := runs[0]
	if err := st.WithinTx(ctx, func(tx *store.Tx) error {
		if err := tx.SetRunIdentifiers(ctx, run.ID, "sess-1", "crush-1", "hash"); err != nil {
			return err
		}
		return tx.UpdateRunStatus(ctx, run.ID, model.RunDispatching, model.RunRunning)
	}); err != nil {
		t.Fatal(err)
	}
	instance, _ := st.Instance(ctx, project.Name)
	fake.DropStream(instance.WorkspaceID)
	waitFor(t, "run unknown after stream loss", func() bool {
		updated, err := st.GetRun(ctx, run.ID)
		return err == nil && updated.Status == model.RunUnknown
	})
}

func TestCrashLoopQuarantineAndAbandon(t *testing.T) {
	// No fake server: starts time out and exhaust the retry policy.
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	projectPath := filepath.Join(dir, "api")
	os.MkdirAll(projectPath, 0o700)
	project := config.Project{
		Name: "api", Path: projectPath, Port: port,
		CrushOptions: config.CrushOptions{DataDir: filepath.Join(projectPath, ".crush")},
	}
	fleet := &config.Fleet{Projects: []config.Project{project}}
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	client := crushapi.NewClient(crushapi.NewClientID(), &http.Client{Transport: transport})
	r := New(fleet, testMM(t), st, client, NewSupervisor())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		if child, ok := r.procs.Child(project.Name); ok {
			child.Signal(syscall.SIGKILL)
		}
	})
	approve(t, r, project)
	run := seedGoal(t, st, project)

	driveOne(t, ctx, r)
	waitFor(t, "start retries exhausted", func() bool {
		return instanceState(t, r, project.Name) == model.InstanceFailed
	})
	updated, err := st.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != model.RunAbandoned {
		t.Errorf("queued attempt = %s, want abandoned after startup retry exhaustion", updated.Status)
	}

	// Operator reset re-arms the start path; repeated failures cross the
	// crash-loop threshold and quarantine the instance (§5.1).
	for i := 0; i < 3; i++ {
		if err := st.WithinTx(ctx, func(tx *store.Tx) error {
			return tx.SetInstanceState(ctx, project.Name, model.InstanceStopped, 1)
		}); err != nil {
			t.Fatal(err)
		}
		seedGoal(t, st, project)
		driveOne(t, ctx, r)
		waitFor(t, "failed after reset", func() bool {
			return instanceState(t, r, project.Name) == model.InstanceFailed
		})
	}
	// Quarantined: another demand does not restart it.
	if err := st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.SetInstanceState(ctx, project.Name, model.InstanceStopped, 1)
	}); err != nil {
		t.Fatal(err)
	}
	seedGoal(t, st, project)
	driveOne(t, ctx, r)
	time.Sleep(100 * time.Millisecond)
	state := instanceState(t, r, project.Name)
	if state == model.InstanceStarting || state == model.InstanceReady {
		t.Errorf("quarantined instance restarted: state = %s", state)
	}
}

func TestDrainReleasesWorkspaceAndStops(t *testing.T) {
	r, fake, st, project, ctx := setup(t)
	approve(t, r, project)
	seedGoal(t, st, project)
	driveOne(t, ctx, r)
	waitFor(t, "instance ready", func() bool {
		return instanceState(t, r, project.Name) == model.InstanceReady
	})
	instance, _ := st.Instance(context.Background(), project.Name)

	err := r.drainInstance(context.Background(), project, model.DrainGraceful)
	if err != nil {
		t.Fatalf("drainInstance: %v", err)
	}
	if deletes := fake.WorkspaceDeletes(); len(deletes) == 0 || deletes[0] != instance.WorkspaceID {
		t.Errorf("workspace hold not released: %v", deletes)
	}
	if controls := fake.Controls(); len(controls) == 0 || controls[len(controls)-1] != "shutdown_if_idle" {
		t.Errorf("shutdown_if_idle not issued: %v", controls)
	}
	if state := instanceState(t, r, project.Name); state != model.InstanceStopped {
		t.Errorf("state after drain = %s, want stopped", state)
	}
}

func TestDemandStartPokesLoop(t *testing.T) {
	r, _, st, project, ctx := setup(t)
	approve(t, r, project)
	seedGoal(t, st, project)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	waitFor(t, "instance started by loop", func() bool {
		return instanceState(t, r, project.Name) == model.InstanceReady
	})
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
}
