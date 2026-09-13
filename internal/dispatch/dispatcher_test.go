package dispatch

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pshickeydev/matchmaker/internal/config"
	"github.com/pshickeydev/matchmaker/internal/crushapi"
	"github.com/pshickeydev/matchmaker/internal/crushtest"
	"github.com/pshickeydev/matchmaker/internal/model"
	"github.com/pshickeydev/matchmaker/internal/notes"
	"github.com/pshickeydev/matchmaker/internal/store"
)

// fixture wires a dispatcher over a fake Crush server and real store with
// two ready instances: api and web.
type fixture struct {
	t       *testing.T
	ctx     context.Context
	st      *store.Store
	fake    *crushtest.Server
	client  *crushapi.Client
	d       *Dispatcher
	service *notes.Service
	apiURL  string
	webURL  string

	currentGoal string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	fake := crushtest.New()
	t.Cleanup(fake.Close)
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	client := crushapi.NewClient(crushapi.NewClientID(), &http.Client{Transport: transport})
	mm := &config.Matchmaker{
		NoteLimits: config.NoteLimits{
			MaxNoteBodyBytes:     1024,
			MaxNotesPerGoal:      100,
			MaxInjectedNoteBytes: 2048,
			MaxReadPageSize:      10,
			RateWindow:           time.Minute,
			MaxRequestsPerWindow: 1000,
		},
	}
	d := New(st, client, notes.NewSelector(st), mm)
	f := &fixture{
		t: t, ctx: context.Background(), st: st, fake: fake, client: client, d: d,
		service: notes.New(st, mm.NoteLimits),
		apiURL:  fake.BaseURL(), webURL: fake.BaseURL(),
	}
	f.makeReady("api")
	f.makeReady("web")
	return f
}

// makeReady seeds a ready instance with a workspace on the fake.
func (f *fixture) makeReady(project string) {
	f.t.Helper()
	path := filepath.Join(f.t.TempDir(), project)
	workspace, err := f.client.CreateWorkspace(f.ctx, f.fake.BaseURL(), path, path+"/.crush", nil)
	if err != nil {
		f.t.Fatalf("workspace %s: %v", project, err)
	}
	err = f.st.WithinTx(f.ctx, func(tx *store.Tx) error {
		if err := tx.SetInstanceEndpoint(f.ctx, project, f.fake.BaseURL(), 0, workspace.ID); err != nil {
			return err
		}
		if err := tx.SetInstanceState(f.ctx, project, model.InstanceStarting, 1); err != nil {
			return err
		}
		return tx.SetInstanceState(f.ctx, project, model.InstanceReady, 1)
	})
	if err != nil {
		f.t.Fatalf("instance %s: %v", project, err)
	}
}

// submitGoal persists a goal from step/target pairs and returns the
// reloaded goal with frozen execution IDs.
func (f *fixture) submitGoal(steps []model.Step, targets map[string][]string) model.Goal {
	f.t.Helper()
	goalID := "goal-" + time.Now().Format("150405.000000000")
	frozen := make(map[string][]model.ResolvedTarget, len(targets))
	for stepID, projects := range targets {
		for _, project := range projects {
			frozen[stepID] = append(frozen[stepID], model.ResolvedTarget{
				StepID: stepID, Project: project, Instance: project,
				ServerURL: f.fake.BaseURL(), Tags: []string{"team:platform"},
			})
		}
	}
	for i := range steps {
		steps[i].GoalID = goalID
		if steps[i].Supervision == "" {
			steps[i].Supervision = model.SupervisionDeny
		}
		if steps[i].Timeout == 0 {
			steps[i].Timeout = time.Minute
		}
	}
	goal := model.Goal{ID: goalID, Type: model.GoalTypeWork, Status: model.GoalActive,
		Steps: steps, FrozenTargets: frozen}
	if err := f.st.WithinTx(f.ctx, func(tx *store.Tx) error {
		return tx.CreateGoal(f.ctx, goal)
	}); err != nil {
		f.t.Fatalf("CreateGoal: %v", err)
	}
	loaded, err := f.st.Goal(f.ctx, goalID)
	if err != nil {
		f.t.Fatal(err)
	}
	f.currentGoal = goalID
	return loaded
}

// queuedRun returns the queued attempt of one step on one project.
func (f *fixture) queuedRun(stepID, project string) model.Run {
	f.t.Helper()
	runs, err := f.st.ActiveRuns(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, run := range runs {
		if run.StepID == stepID && run.Project == project && run.Status == model.RunQueued {
			return run
		}
	}
	f.t.Fatalf("no queued run for step %s project %s", stepID, project)
	return model.Run{}
}

// runOf returns the newest attempt of one step on one project.
func (f *fixture) runOf(stepID, project string) model.Run {
	f.t.Helper()
	goal, err := f.st.Goal(f.ctx, f.currentGoal)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, target := range goal.FrozenTargets[stepID] {
		if target.Project != project {
			continue
		}
		exec, err := f.st.TargetExecution(f.ctx, target.ExecutionID)
		if err != nil {
			continue
		}
		if len(exec.Attempts) == 0 {
			continue
		}
		return exec.Attempts[len(exec.Attempts)-1]
	}
	f.t.Fatalf("no run for step %s project %s", stepID, project)
	return model.Run{}
}

// completeAs drives one step's newest attempt to a terminal state
// through the store, as supervision would after the matching events.
func (f *fixture) completeAs(stepID, project string, terminal model.RunStatus) {
	f.t.Helper()
	run := f.runOf(stepID, project)
	err := f.st.WithinTx(f.ctx, func(tx *store.Tx) error {
		switch run.Status {
		case model.RunQueued:
			if err := tx.SetRunIdentifiers(f.ctx, run.ID, "sess-"+stepID, "crush-"+stepID, "hash"); err != nil {
				return err
			}
			if err := tx.UpdateRunStatus(f.ctx, run.ID, model.RunDispatching, model.RunRunning); err != nil {
				return err
			}
		case model.RunRunning:
		default:
			return nil
		}
		return tx.UpdateRunStatus(f.ctx, run.ID, model.RunRunning, terminal)
	})
	if err != nil {
		f.t.Fatalf("completeAs %s/%s: %v", stepID, project, err)
	}
}

// driveOnce runs one dispatch pass.
func (f *fixture) driveOnce() {
	f.t.Helper()
	if err := f.d.drive(f.ctx); err != nil {
		f.t.Fatalf("drive: %v", err)
	}
}

func (f *fixture) runStatus(stepID, project string) model.RunStatus {
	f.t.Helper()
	return f.runOf(stepID, project).Status
}

func step(id, prompt string, needs ...string) model.Step {
	return model.Step{ID: id, PromptTemplate: prompt, Needs: needs}
}

func TestDispatchRunHappyPath(t *testing.T) {
	f := newFixture(t)
	goal := f.submitGoal(
		[]model.Step{step("a", "build {{.Objective}}")},
		map[string][]string{"a": {"api"}},
	)
	_ = goal
	f.driveOnce()

	if status := f.runStatus("a", "api"); status != model.RunRunning {
		t.Fatalf("run status = %s, want running", status)
	}
	submits := f.fake.Submits()
	if len(submits) != 1 {
		t.Fatalf("submits = %d, want 1", len(submits))
	}
	if !strings.Contains(submits[0].Prompt, "build ") {
		t.Errorf("prompt = %q", submits[0].Prompt)
	}
	if !strings.HasPrefix(submits[0].RunID, runIDPrefix+"_") {
		t.Errorf("crush run id = %q", submits[0].RunID)
	}
	run := f.runOf("a", "api")
	if run.SessionID == "" || run.RenderedPromptHash == "" || run.WorkspaceID == "" {
		t.Errorf("run identifiers incomplete: %+v", run)
	}
	// The dedicated session exists on the fake.
	if _, ok := f.fake.Session(run.SessionID); !ok {
		t.Errorf("dedicated session %s missing on server", run.SessionID)
	}
}

func TestNeedsGatingUnlocksDownstream(t *testing.T) {
	f := newFixture(t)
	f.submitGoal(
		[]model.Step{
			step("a", "build"),
			step("b", "review {{range .Upstream}}{{.Project}}{{end}}", "a"),
		},
		map[string][]string{"a": {"api"}, "b": {"web"}},
	)
	f.driveOnce()
	if status := f.runStatus("b", "web"); status != model.RunQueued {
		t.Fatalf("downstream dispatched before needs satisfied: %s", status)
	}
	if len(f.fake.Submits()) != 1 {
		t.Fatalf("submits = %d, want only upstream", len(f.fake.Submits()))
	}

	f.completeAs("a", "api", model.RunCompleted)
	f.driveOnce()
	if status := f.runStatus("b", "web"); status != model.RunRunning {
		t.Fatalf("downstream not dispatched after upstream completed: %s", status)
	}
	submits := f.fake.Submits()
	last := submits[len(submits)-1]
	if !strings.Contains(last.Prompt, "api") {
		t.Errorf("downstream prompt lacks upstream reference: %q", last.Prompt)
	}
	if !strings.Contains(last.Prompt, "result_read") {
		t.Errorf("downstream prompt lacks result_read guidance: %q", last.Prompt)
	}
}

func TestNeedsSkipPropagation(t *testing.T) {
	f := newFixture(t)
	f.submitGoal(
		[]model.Step{
			step("a", "build"),
			step("b", "review", "a"),
		},
		map[string][]string{"a": {"api"}, "b": {"web"}},
	)
	f.completeAs("a", "api", model.RunFailed)
	f.driveOnce()
	if status := f.runStatus("b", "web"); status != model.RunSkipped {
		t.Fatalf("unsatisfiable dependent = %s, want skipped", status)
	}
	if len(f.fake.Submits()) != 0 {
		t.Errorf("skipped step dispatched: %+v", f.fake.Submits())
	}
}

func TestAcceptPartialNeeds(t *testing.T) {
	f := newFixture(t)
	f.submitGoal(
		[]model.Step{
			step("a", "build"),
			{ID: "b", PromptTemplate: "review", Needs: []string{"a"}, AcceptPartialNeeds: true},
		},
		map[string][]string{"a": {"api", "web"}, "b": {"api"}},
	)
	f.completeAs("a", "api", model.RunCompleted)
	f.completeAs("a", "web", model.RunFailed)
	f.driveOnce()
	if status := f.runStatus("b", "api"); status != model.RunRunning {
		t.Fatalf("partial-satisfied dependent = %s, want running", status)
	}

	// Without accept_partial_needs the same upstream mix skips.
	f2 := newFixture(t)
	f2.submitGoal(
		[]model.Step{
			step("a", "build"),
			step("b", "review", "a"),
		},
		map[string][]string{"a": {"api", "web"}, "b": {"api"}},
	)
	f2.completeAs("a", "api", model.RunCompleted)
	f2.completeAs("a", "web", model.RunFailed)
	f2.driveOnce()
	if status := f2.runStatus("b", "api"); status != model.RunSkipped {
		t.Fatalf("strict dependent = %s, want skipped on partial upstream", status)
	}
}

func TestPerInstanceSerialization(t *testing.T) {
	f := newFixture(t)
	f.submitGoal(
		[]model.Step{
			step("a", "first"),
			step("c", "second"),
		},
		map[string][]string{"a": {"api"}, "c": {"api"}},
	)
	f.driveOnce()
	submits := f.fake.Submits()
	if len(submits) != 1 {
		t.Fatalf("serialization violated: %d submits", len(submits))
	}
	other := f.runStatus("c", "api")
	if other == model.RunRunning || other == model.RunDispatching {
		t.Fatalf("second run dispatched concurrently: %s", other)
	}
	// The queued run dispatches once the holder goes terminal.
	f.completeAs("a", "api", model.RunCompleted)
	f.driveOnce()
	if len(f.fake.Submits()) != 2 {
		t.Fatalf("queued run not dispatched after release: %d", len(f.fake.Submits()))
	}
}

func TestRetrySchedulingWithBackoff(t *testing.T) {
	origBackoff := retryBackoff
	retryBackoff = time.Millisecond
	t.Cleanup(func() { retryBackoff = origBackoff })

	f := newFixture(t)
	f.submitGoal(
		[]model.Step{{ID: "a", PromptTemplate: "build", Retries: 1}},
		map[string][]string{"a": {"api"}},
	)
	f.driveOnce()
	f.completeAs("a", "api", model.RunFailed)
	f.driveOnce()
	// The retry's backoff is a millisecond; one more pass dispatches it.
	time.Sleep(5 * time.Millisecond)
	f.driveOnce()

	// A retry attempt was appended and, with a short backoff, dispatched.
	goal, err := f.st.Goal(f.ctx, f.currentGoal)
	if err != nil {
		t.Fatal(err)
	}
	var attempt2 model.Run
	for _, target := range goal.FrozenTargets["a"] {
		exec, err := f.st.TargetExecution(f.ctx, target.ExecutionID)
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range exec.Attempts {
			if run.Attempt == 2 {
				attempt2 = run
			}
		}
	}
	if attempt2.ID == "" {
		t.Fatal("retry attempt not appended after failure")
	}
	if attempt2.Status != model.RunRunning {
		t.Errorf("retry attempt status = %s, want running", attempt2.Status)
	}

	// Exhausted budget: no third attempt.
	f.completeAs("a", "api", model.RunFailed)
	f.driveOnce()
	goal, err = f.st.Goal(f.ctx, f.currentGoal)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range goal.FrozenTargets["a"] {
		exec, err := f.st.TargetExecution(f.ctx, target.ExecutionID)
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range exec.Attempts {
			if run.Attempt == 3 {
				t.Fatal("retry appended beyond step budget")
			}
		}
	}
}

func TestNoteInjectionAndCursorAdvance(t *testing.T) {
	f := newFixture(t)
	goal := f.submitGoal(
		[]model.Step{step("a", "build")},
		map[string][]string{"a": {"api"}},
	)
	if _, err := f.service.Send(f.ctx, goal.ID, "api", model.Audience{Project: "api"}, "the api shape changed"); err != nil {
		t.Fatalf("send note: %v", err)
	}
	f.driveOnce()

	submits := f.fake.Submits()
	if len(submits) != 1 {
		t.Fatalf("submits = %d", len(submits))
	}
	if !strings.Contains(submits[0].Prompt, "notes from other agents") {
		t.Errorf("prompt lacks untrusted notes section: %q", submits[0].Prompt)
	}
	if !strings.Contains(submits[0].Prompt, "the api shape changed") {
		t.Errorf("prompt lacks injected note body: %q", submits[0].Prompt)
	}
	if !strings.Contains(submits[0].Prompt, "claimed sender: api") {
		t.Errorf("prompt lacks claimed sender attribution: %q", submits[0].Prompt)
	}
	// The audience cursor advanced in the submission transaction.
	cursor, err := f.st.NoteCursor(f.ctx, goal.ID, "api")
	if err != nil || cursor == 0 {
		t.Errorf("note cursor = %d err=%v, want advanced", cursor, err)
	}
	run := f.runOf("a", "api")
	if run.NotesHighWater != cursor {
		t.Errorf("run high water = %d, cursor = %d", run.NotesHighWater, cursor)
	}
	// A second dispatch of the same audience injects nothing new.
	f.driveOnce()
	if len(f.fake.Submits()) != 1 {
		t.Errorf("unexpected second submit: %+v", f.fake.Submits())
	}
}

func TestDispatchStoppedInstanceLeavesRunQueued(t *testing.T) {
	f := newFixture(t)
	// A fresh instance record that was never started.
	err := f.st.WithinTx(f.ctx, func(tx *store.Tx) error {
		return tx.SetInstanceState(f.ctx, "gone", model.InstanceStopped, 0)
	})
	if err != nil {
		t.Fatal(err)
	}
	f.submitGoal(
		[]model.Step{step("a", "build")},
		map[string][]string{"a": {"gone"}},
	)
	f.driveOnce()
	if len(f.fake.Submits()) != 0 {
		t.Errorf("dispatched to stopped instance: %+v", f.fake.Submits())
	}
	// The queued attempt itself is the durable, store-mediated demand the
	// reconcile tick consumes (plan M5): it stays queued, ready to
	// dispatch once the instance start converges.
	queued := f.queuedRun("a", "gone")
	if queued.Status != model.RunQueued {
		t.Errorf("stopped-instance run = %s, want queued", queued.Status)
	}
}

func TestDispatchStartingInstanceWaitsForReady(t *testing.T) {
	f := newFixture(t)
	// A third instance mid-start: endpoint recorded, state starting.
	path := filepath.Join(f.t.TempDir(), "svc")
	workspace, err := f.client.CreateWorkspace(f.ctx, f.fake.BaseURL(), path, path+"/.crush", nil)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	err = f.st.WithinTx(f.ctx, func(tx *store.Tx) error {
		if err := tx.SetInstanceEndpoint(f.ctx, "svc", f.fake.BaseURL(), 0, workspace.ID); err != nil {
			return err
		}
		return tx.SetInstanceState(f.ctx, "svc", model.InstanceStarting, 1)
	})
	if err != nil {
		t.Fatal(err)
	}
	f.submitGoal(
		[]model.Step{step("s", "work")},
		map[string][]string{"s": {"svc"}},
	)
	f.driveOnce()
	if len(f.fake.Submits()) != 0 {
		t.Errorf("submitted to a starting instance: %+v", f.fake.Submits())
	}
	queued := f.queuedRun("s", "svc")
	if queued.Status != model.RunQueued {
		t.Errorf("starting-instance run = %s, want queued", queued.Status)
	}
}

func TestRenderPromptShape(t *testing.T) {
	step := model.Step{ID: "b", PromptTemplate: "objective {{.Objective}}; upstream {{range .Upstream}}{{.StepID}}:{{.Status}} {{end}}"}
	upstream := []UpstreamResult{{StepID: "a", Project: "api", Status: model.RunCompleted, Excerpt: "ex", RunHandle: "r1"}}
	notes := []model.Note{{NoteID: 1, From: "web", Body: "heads up"}}
	prompt, hash, err := RenderPrompt(step, upstream, notes)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"objective", "a:completed", "result_read", "notes from other agents", "heads up", "claimed sender: web"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q: %q", want, prompt)
		}
	}
	if len(hash) != 32 {
		t.Errorf("hash length = %d", len(hash))
	}
	again, hash2, _ := RenderPrompt(step, upstream, notes)
	if again != prompt || string(hash) != string(hash2) {
		t.Error("render not deterministic")
	}
	// No upstream: no result_read guidance.
	prompt, _, err = RenderPrompt(model.Step{ID: "x", PromptTemplate: "solo"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "result_read") {
		t.Errorf("solo prompt carries upstream guidance: %q", prompt)
	}
}
