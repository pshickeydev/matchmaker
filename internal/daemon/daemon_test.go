package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/pshickeydev/matchmaker/internal/config"
	"github.com/pshickeydev/matchmaker/internal/crushtest"
	"github.com/pshickeydev/matchmaker/internal/goalvalidate"
	"github.com/pshickeydev/matchmaker/internal/model"
	"github.com/pshickeydev/matchmaker/internal/rpc"
)

// e2e wires a full daemon over two fake Crush servers.
type e2e struct {
	t           *testing.T
	ctx         context.Context
	cancel      context.CancelFunc
	apiFake     *crushtest.Server
	webFake     *crushtest.Server
	stateDir    string
	d           *Daemon
	fleetCfg    *config.Fleet
	mm          *config.Matchmaker
	projectPath map[string]string
}

var stubSeq atomic.Int64

// stubBinary writes a long-lived stand-in for the crush server binary.
func stubBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("crush-stub-%d", stubSeq.Add(1)), "crush-stub")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec sleep 600\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// newE2E starts a daemon over two fake Crush servers.
func newE2E(t *testing.T) *e2e {
	t.Helper()
	apiFake := crushtest.New()
	webFake := crushtest.New()
	t.Cleanup(apiFake.Close)
	t.Cleanup(webFake.Close)
	root := t.TempDir()
	apiPath := filepath.Join(root, "api")
	webPath := filepath.Join(root, "web")
	for _, dir := range []string{apiPath, webPath} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	stub := stubBinary(t)
	// Orphaned stub children outlive crash-style teardown in tests; sweep
	// by the stub script name at cleanup.
	t.Cleanup(func() { exec.Command("pkill", "-f", "crush-stub").Run() })
	fleetCfg := &config.Fleet{Projects: []config.Project{
		{Name: "api", Path: apiPath, Port: portOf(t, apiFake), Tags: []string{"lang:go"},
			CrushOptions: config.CrushOptions{DataDir: filepath.Join(apiPath, ".crush")}},
		{Name: "web", Path: webPath, Port: portOf(t, webFake), Tags: []string{"lang:ts"},
			CrushOptions: config.CrushOptions{DataDir: filepath.Join(webPath, ".crush")}},
	}}
	mm := &config.Matchmaker{
		StateDir:         filepath.Join(root, "state"),
		CrushBinary:      stub,
		CoordinationAddr: "127.0.0.1:0",
		Reconcile: config.ReconcileConfig{
			TickInterval:         50 * time.Millisecond,
			SSEReconnectBackoff:  10 * time.Millisecond,
			HealthTimeout:        300 * time.Millisecond,
			DrainTimeout:         300 * time.Millisecond,
			GlobalDrainDeadline:  2 * time.Second,
			MaxRestartsPerWindow: 5,
			CrashLoopWindow:      time.Minute,
			InstanceStartRetry:   config.RetryPolicy{MaxAttempts: 2, Backoff: 10 * time.Millisecond},
		},
		GoalLimits: config.GoalLimits{
			MaxSteps: 10, MaxDepsPerStep: 5, MaxPromptBytes: 4096,
			MaxTargetsPerStep: 5, MaxTotalRunsPerGoal: 20,
			MinTimeout: time.Second, MaxTimeout: time.Hour, MaxRetries: 3,
		},
		NoteLimits: config.NoteLimits{
			MaxNoteBodyBytes:     2048,
			MaxNotesPerGoal:      100,
			MaxInjectedNoteBytes: 2048,
			MaxReadPageSize:      10,
			MaxResultChunkBytes:  1024,
			RateWindow:           time.Minute,
			MaxRequestsPerWindow: 1000,
			MaxRetainedNotes:     500,
		},
		Supervision: config.SupervisionConfig{CancelGrace: 250 * time.Millisecond},
		Fleet:       config.FleetPins{ApprovedVersion: config.PinnedCrushVersion},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	e := &e2e{
		t: t, ctx: ctx, cancel: cancel, apiFake: apiFake, webFake: webFake,
		stateDir: mm.StateDir, fleetCfg: fleetCfg, mm: mm,
		projectPath: map[string]string{"api": apiPath, "web": webPath},
	}
	e.startDaemon()
	return e
}

// startDaemon builds and runs the daemon, waiting for coordination bind.
func (e *e2e) startDaemon() {
	e.t.Helper()
	d, err := New(e.stateDir, e.fleetCfg, e.mm)
	if err != nil {
		e.t.Fatalf("daemon.New: %v", err)
	}
	e.d = d
	go d.Run(e.ctx)
	e.waitUntil("coordination bound", func() bool {
		return d.coordination.Addr() != nil
	})
}

// portOf extracts a fake server's port.
func portOf(t *testing.T, fake *crushtest.Server) int {
	t.Helper()
	port, err := strconv.Atoi(strings.TrimPrefix(fake.BaseURL(), "http://127.0.0.1:"))
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// waitUntil polls a condition with a deadline.
func (e *e2e) waitUntil(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	e.t.Fatalf("timed out waiting for %s", what)
}

// handle performs one RPC against the daemon under test.
func (e *e2e) handle(req rpc.Request) rpc.Response {
	e.t.Helper()
	response, err := e.d.Handle(e.ctx, req)
	if err != nil {
		e.t.Fatalf("handle %s: %v", req.Method, err)
	}
	return response
}

// onboard installs the coordination registration for a project.
func (e *e2e) onboard(project string) rpc.Response {
	e.t.Helper()
	return e.handle(rpc.Request{
		IdempotencyKey: "onboard-" + project,
		Method:         rpc.MethodOnboard,
		Project:        project,
		Approval:       "granted",
	})
}

// workspaceOf reads a project's adopted workspace ID.
func (e *e2e) workspaceOf(project string) string {
	e.t.Helper()
	instance, err := e.d.st.Instance(context.Background(), project)
	if err != nil || instance.WorkspaceID == "" {
		e.t.Fatalf("workspace of %s: %+v err=%v", project, instance, err)
	}
	return instance.WorkspaceID
}

// goalOf loads one goal by ID.
func (e *e2e) goalOf(goalID string) model.Goal {
	e.t.Helper()
	goal, err := e.d.st.Goal(e.ctx, goalID)
	if err != nil {
		e.t.Fatal(err)
	}
	return goal
}

// runIDByCrushRun resolves a step's durable run ID from its Crush RunID.
func (e *e2e) runIDByCrushRun(goalID, crushRunID string) string {
	e.t.Helper()
	goal, err := e.d.st.Goal(e.ctx, goalID)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, targets := range goal.FrozenTargets {
		for _, target := range targets {
			exec, err := e.d.st.TargetExecution(e.ctx, target.ExecutionID)
			if err != nil {
				continue
			}
			for _, run := range exec.Attempts {
				if run.CrushRunID == crushRunID {
					return run.ID
				}
			}
		}
	}
	e.t.Fatalf("no run with Crush RunID %s", crushRunID)
	return ""
}

// shippedDraft builds the typed two-step draft: build on api, review on
// web needing it.
func shippedDraft(objective string) goalvalidate.GoalDraft {
	return goalvalidate.GoalDraft{
		Objective: objective,
		Steps: []model.Step{
			{ID: "build", PromptTemplate: "implement the change",
				Supervision: model.SupervisionDeny, Timeout: 5 * time.Minute,
				Target: model.TargetSpec{Explicit: []string{"api"}}},
			{ID: "review",
				PromptTemplate: "adapt to the upstream change; fetch full results for {{range .Upstream}}{{.RunHandle}} {{end}}",
				Needs:          []string{"build"},
				Supervision:    model.SupervisionDeny, Timeout: 5 * time.Minute,
				Target: model.TargetSpec{Explicit: []string{"web"}}},
		},
	}
}

// noteSend invokes the live MCP note_send tool over the coordination
// HTTP transport, as a fleet agent would (plan Decision 2).
func (e *e2e) noteSend(goalID, from, to, body string) error {
	e.t.Helper()
	url := "http://" + e.d.coordination.Addr().String() + "/mcp"
	payload := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "note_send",
			"arguments": map[string]any{
				"goal": goalID, "from": from, "to": to, "body": body,
			},
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("note_send status %d", resp.StatusCode)
	}
	return nil
}

func TestEndToEndGoalWithNotePassing(t *testing.T) {
	e := newE2E(t)

	// Submit the goal: its queued attempts are the demand that starts
	// the stopped instances.
	objective := "ship the cutover"
	draft := shippedDraft(objective)
	response := e.handle(rpc.Request{
		IdempotencyKey: "submit-e2e-1",
		Method:         rpc.MethodSubmitGoal,
		Submit:         &rpc.SubmitRequest{Draft: draft},
	})
	if len(response.Validation) > 0 {
		t.Fatalf("goal rejected: %+v", response.Validation)
	}
	goalID := response.GoalID

	// Onboard both projects: registration plus atomic fingerprint commit
	// unlocks the demanded instance starts.
	for _, project := range []string{"api", "web"} {
		if onboardResponse := e.onboard(project); !onboardResponse.Onboarded {
			t.Fatalf("onboard %s: %s", project, onboardResponse.Message)
		}
	}
	e.waitUntil("api ready", func() bool {
		state, _ := e.d.reconciler.InstanceState(e.ctx, "api")
		return state == model.InstanceReady || state == model.InstanceBusy
	})
	e.waitUntil("web ready", func() bool {
		state, _ := e.d.reconciler.InstanceState(e.ctx, "web")
		return state == model.InstanceReady || state == model.InstanceBusy
	})

	// Step 1 dispatches on api.
	e.waitUntil("build submitted", func() bool {
		return len(e.apiFake.Submits()) == 1
	})
	buildSubmit := e.apiFake.Submits()[0]

	// A permission request on the dedicated session is denied by policy.
	if err := e.apiFake.EmitPermission(e.workspaceOf("api"), map[string]any{
		"id": "perm-e2e", "session_id": buildSubmit.SessionID,
		"tool_name": "bash", "params": map[string]string{"command": "ls"},
	}); err != nil {
		t.Fatal(err)
	}
	e.waitUntil("permission denied", func() bool {
		for _, grant := range e.apiFake.Grants() {
			if grant.RequestID == "perm-e2e" && grant.Action == "deny" {
				return true
			}
		}
		return false
	})

	// The api agent passes a note mid-goal through the live MCP tool: the
	// blocking decision informs the other area (plan Decision 2).
	if err := e.noteSend(goalID, "api", "all", "the api shape changed to v2"); err != nil {
		t.Fatalf("note_send: %v", err)
	}

	// The build completes.
	e.apiFake.AddMessage(buildSubmit.SessionID, "assistant", "api cutover complete: shape v2 with new fields")
	if err := e.apiFake.CompleteRun(e.workspaceOf("api"), buildSubmit.SessionID, buildSubmit.RunID, "api done", "", false); err != nil {
		t.Fatal(err)
	}

	// Step 2 dispatches on web; its prompt carries the mid-goal note and
	// the result_read guidance: the demo moment.
	e.waitUntil("review submitted", func() bool {
		return len(e.webFake.Submits()) == 1
	})
	reviewSubmit := e.webFake.Submits()[0]
	for _, want := range []string{
		"notes from other agents",
		"the api shape changed to v2",
		"claimed sender: api",
		"result_read",
		"Goal ID: " + goalID,
		"Objective: " + objective,
	} {
		if !strings.Contains(reviewSubmit.Prompt, want) {
			t.Errorf("review prompt missing %q:\n%s", want, reviewSubmit.Prompt)
		}
	}

	// The review completes and the goal aggregates to succeeded.
	e.webFake.AddMessage(reviewSubmit.SessionID, "assistant", "web adapted to v2")
	if err := e.webFake.CompleteRun(e.workspaceOf("web"), reviewSubmit.SessionID, reviewSubmit.RunID, "web done", "", false); err != nil {
		t.Fatal(err)
	}
	e.waitUntil("goal succeeded", func() bool {
		return e.goalOf(goalID).Status == model.GoalSucceeded
	})

	// goal status carries per-target run detail: run IDs and statuses are
	// what operators need to abandon stuck attempts.
	status := e.handle(rpc.Request{Method: rpc.MethodGoalStatus, GoalID: goalID})
	if len(status.Runs) != 2 {
		t.Fatalf("status runs = %d, want one per target", len(status.Runs))
	}
	for _, run := range status.Runs {
		if run.Status != model.RunCompleted {
			t.Errorf("run %s = %s", run.ID, run.Status)
		}
	}

	// The exported report resolves the lazily fetched session content.
	exportPath := filepath.Join(e.stateDir, "report.json")
	exportResponse := e.handle(rpc.Request{
		Method: rpc.MethodExportReport, GoalID: goalID, FilePath: exportPath,
	})
	if !strings.Contains(exportResponse.Message, "exported") {
		t.Fatalf("export: %+v", exportResponse)
	}
	content, err := os.ReadFile(exportPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"api cutover complete", "web adapted to v2", `"status": "succeeded"`} {
		if !strings.Contains(string(content), want) {
			t.Errorf("report missing %q", want)
		}
	}
	info, err := os.Stat(exportPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("report perms = %v", info.Mode().Perm())
	}

	// An idempotent submit replay does not duplicate the goal.
	replay := e.handle(rpc.Request{
		IdempotencyKey: "submit-e2e-1",
		Method:         rpc.MethodSubmitGoal,
		Submit:         &rpc.SubmitRequest{Draft: draft},
	})
	if replay.GoalID != goalID {
		t.Errorf("idempotent replay returned %q, want %q", replay.GoalID, goalID)
	}
	goals, _ := e.d.st.ListGoals(e.ctx, model.GoalSucceeded)
	if len(goals) != 1 {
		t.Errorf("goal duplicated on replay: %d succeeded goals", len(goals))
	}
}

func TestDaemonCrashRestartRecovery(t *testing.T) {
	e := newE2E(t)
	response := e.handle(rpc.Request{
		IdempotencyKey: "submit-restart-1",
		Method:         rpc.MethodSubmitGoal,
		Submit:         &rpc.SubmitRequest{Draft: shippedDraft("survive the restart")},
	})
	goalID := response.GoalID
	for _, project := range []string{"api", "web"} {
		if onboardResponse := e.onboard(project); !onboardResponse.Onboarded {
			t.Fatalf("onboard %s: %s", project, onboardResponse.Message)
		}
	}
	e.waitUntil("api ready", func() bool {
		state, _ := e.d.reconciler.InstanceState(e.ctx, "api")
		return state == model.InstanceReady || state == model.InstanceBusy
	})
	e.waitUntil("build submitted", func() bool {
		return len(e.apiFake.Submits()) == 1
	})
	buildSubmit := e.apiFake.Submits()[0]
	// Capture the durable run id for post-restart polling.
	buildRunID := e.runIDByCrushRun(goalID, buildSubmit.RunID)

	// The daemon crashes: components stop, the stream drops, the lock and
	// store close without draining, so workspaces survive for adoption.
	// The in-flight run stays recorded as running: the crashed daemon
	// cannot prove anything, and adoption reconciles it.
	e.d.runCancel()
	e.d.rpcServer.Close()
	for _, project := range []string{"api", "web"} {
		if child, ok := e.d.procs.Child(project); ok {
			child.Signal(syscall.SIGKILL)
		}
	}
	e.d.st.Close()
	if err := e.d.lock.Release(); err != nil {
		t.Fatal(err)
	}
	e.cancel()

	// A new daemon adopts the live fleet and re-attaches the stream.
	restartCtx, restartCancel := context.WithCancel(context.Background())
	t.Cleanup(restartCancel)
	e.ctx = restartCtx
	e.d = nil
	e.startDaemon()
	e.waitUntil("workspace re-adopted", func() bool {
		instance, err := e.d.st.Instance(e.ctx, "api")
		return err == nil && instance.WorkspaceID != ""
	})
	// Adoption marked the orphaned run unknown (its stream evidence was
	// lost with the crash) and reconciled it from its durable session.
	e.waitUntil("orphaned run recovered to unknown", func() bool {
		run, err := e.d.st.GetRun(e.ctx, buildRunID)
		return err == nil && (run.Status == model.RunUnknown || run.Status == model.RunRunning)
	})

	// The completing event on the re-attached stream terminalizes the
	// recovered run; it is never resubmitted. Wait for the attach first
	// so the event reaches a live subscriber.
	adoptedWorkspace := e.workspaceOf("api")
	e.waitUntil("re-attached stream", func() bool {
		return e.apiFake.SubscriberCount(adoptedWorkspace) > 0
	})
	e.apiFake.AddMessage(buildSubmit.SessionID, "assistant", "api cutover done after restart")
	if err := e.apiFake.CompleteRun(adoptedWorkspace, buildSubmit.SessionID, buildSubmit.RunID, "done after restart", "", false); err != nil {
		t.Fatal(err)
	}
	e.waitUntil("recovered run completed", func() bool {
		run, err := e.d.st.GetRun(e.ctx, buildRunID)
		return err == nil && run.Status == model.RunCompleted
	})
	if submits := e.apiFake.Submits(); len(submits) != 1 {
		t.Errorf("recovered run was resubmitted: %d submits", len(submits))
	}

	// The dependent step dispatches and the goal finishes.
	e.waitUntil("review submitted after recovery", func() bool {
		return len(e.webFake.Submits()) == 1
	})
	reviewSubmit := e.webFake.Submits()[0]
	e.webFake.AddMessage(reviewSubmit.SessionID, "assistant", "web review done after restart")
	if err := e.webFake.CompleteRun(e.workspaceOf("web"), reviewSubmit.SessionID, reviewSubmit.RunID, "review done", "", false); err != nil {
		t.Fatal(err)
	}
	e.waitUntil("goal succeeded after restart", func() bool {
		return e.goalOf(goalID).Status == model.GoalSucceeded
	})

	// The final report exports after recovery (plan M9 acceptance).
	exportPath := filepath.Join(e.stateDir, "report-after-restart.json")
	exportResponse := e.handle(rpc.Request{
		Method: rpc.MethodExportReport, GoalID: goalID, FilePath: exportPath,
	})
	if !strings.Contains(exportResponse.Message, "exported") {
		t.Fatalf("export: %+v", exportResponse)
	}
	content, err := os.ReadFile(exportPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"api cutover done after restart", "web review done after restart", `"status": "succeeded"`} {
		if !strings.Contains(string(content), want) {
			t.Errorf("report missing %q", want)
		}
	}
}

func TestMutatingMethodRequiresIdempotencyKey(t *testing.T) {
	e := newE2E(t)
	_, err := e.d.Handle(e.ctx, rpc.Request{
		Method: rpc.MethodSubmitGoal,
		Submit: &rpc.SubmitRequest{Draft: shippedDraft("needs a key")},
	})
	if err == nil || !strings.Contains(err.Error(), "idempotency key") {
		t.Fatalf("keyless mutating request = %v, want idempotency-key rejection", err)
	}
}
