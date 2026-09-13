package coordination

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/pshickeydev/matchmaker/internal/config"
	"github.com/pshickeydev/matchmaker/internal/crushapi"
	"github.com/pshickeydev/matchmaker/internal/crushtest"
	"github.com/pshickeydev/matchmaker/internal/model"
	"github.com/pshickeydev/matchmaker/internal/notes"
	"github.com/pshickeydev/matchmaker/internal/store"
)

// fixture wires the coordination server over a real store, note service,
// and fake Crush server.
type fixture struct {
	t            *testing.T
	ctx          context.Context
	st           *store.Store
	fake         *crushtest.Server
	srv          *Server
	client       *client.Client
	session      string
	completedRun model.Run
}

func testLimits() config.NoteLimits {
	return config.NoteLimits{
		MaxNoteBodyBytes:     64,
		MaxNotesPerGoal:      10,
		MaxInjectedNoteBytes: 128,
		MaxReadPageSize:      2,
		MaxResultChunkBytes:  16,
		RateWindow:           time.Minute,
		MaxRequestsPerWindow: 100,
	}
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
	crushClient := crushapi.NewClient(crushapi.NewClientID(), &http.Client{Transport: transport})
	ctx := context.Background()

	// A work goal with api (lang:go, team:platform) and web participants,
	// a plan goal, and a completed run on a live session.
	projectPath := filepath.Join(t.TempDir(), "api")
	workspace, err := crushClient.CreateWorkspace(ctx, fake.BaseURL(), projectPath, projectPath+"/.crush", nil)
	if err != nil {
		t.Fatal(err)
	}
	session, err := crushClient.CreateSession(ctx, fake.BaseURL(), workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	targets := func(project string, tags []string) []model.ResolvedTarget {
		return []model.ResolvedTarget{{Project: project, Instance: project, ServerURL: fake.BaseURL(), Tags: tags}}
	}
	work := model.Goal{
		ID: "work", Type: model.GoalTypeWork, Status: model.GoalActive,
		Steps: []model.Step{{ID: "a", GoalID: "work", PromptTemplate: "p",
			Supervision: model.SupervisionDeny, Timeout: time.Minute}},
		FrozenTargets: map[string][]model.ResolvedTarget{
			"a": append(targets("api", []string{"lang:go", "team:platform"}),
				model.ResolvedTarget{Project: "web", Instance: "web", ServerURL: fake.BaseURL(), Tags: []string{"lang:ts", "team:platform"}}),
		},
	}
	if err := st.WithinTx(ctx, func(tx *store.Tx) error { return tx.CreateGoal(ctx, work) }); err != nil {
		t.Fatal(err)
	}
	plan := work
	plan.ID = "plan"
	plan.Type = model.GoalTypePlan
	plan.Steps = []model.Step{{ID: "a", GoalID: "plan", PromptTemplate: "p", Supervision: model.SupervisionDeny, Timeout: time.Minute}}
	plan.FrozenTargets = map[string][]model.ResolvedTarget{"a": targets("api", []string{"lang:go"})}
	if err := st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.CreatePlanningGoal(ctx, plan, "plan the thing")
	}); err != nil {
		t.Fatal(err)
	}

	// One completed run on the live session for result_read.
	completed := work
	completed.ID = "done"
	completed.Steps = []model.Step{{ID: "a", GoalID: "done", PromptTemplate: "p", Supervision: model.SupervisionDeny, Timeout: time.Minute}}
	completed.FrozenTargets = map[string][]model.ResolvedTarget{"a": targets("api", []string{"lang:go"})}
	if err := st.WithinTx(ctx, func(tx *store.Tx) error { return tx.CreateGoal(ctx, completed) }); err != nil {
		t.Fatal(err)
	}
	if err := st.WithinTx(ctx, func(tx *store.Tx) error {
		if err := tx.SetInstanceEndpoint(ctx, "api", fake.BaseURL(), 0, workspace.ID); err != nil {
			return err
		}
		if err := tx.SetInstanceState(ctx, "api", model.InstanceStarting, 1); err != nil {
			return err
		}
		return tx.SetInstanceState(ctx, "api", model.InstanceReady, 1)
	}); err != nil {
		t.Fatal(err)
	}
	runs, _ := st.ActiveRuns(ctx)
	var run model.Run
	for _, candidate := range runs {
		if candidate.GoalID == "done" {
			run = candidate
		}
	}
	if run.ID == "" {
		t.Fatal("done-goal run missing before completion")
	}
	if err := st.WithinTx(ctx, func(tx *store.Tx) error {
		if err := tx.SetRunIdentifiers(ctx, run.ID, session.ID, "crush-done", "hash-done"); err != nil {
			return err
		}
		if err := tx.UpdateRunStatus(ctx, run.ID, model.RunDispatching, model.RunRunning); err != nil {
			return err
		}
		return tx.UpdateRunStatus(ctx, run.ID, model.RunRunning, model.RunCompleted)
	}); err != nil {
		t.Fatal(err)
	}
	run, _ = st.GetRun(ctx, run.ID)
	fake.AddMessage(session.ID, "user", "do the work")
	fake.AddMessage(session.ID, "assistant", "the quick brown fox jumps over the lazy dog repeatedly")

	svc := notes.New(st, testLimits())
	srv := New(svc, st, crushClient, testLimits())
	return &fixture{
		t: t, ctx: ctx, st: st, fake: fake, srv: srv,
		client: nil, session: session.ID, completedRun: run,
	}
}

// mcpClient returns an initialized in-process MCP client over the tools.
func (f *fixture) mcpClient(t *testing.T) *client.Client {
	t.Helper()
	if f.client != nil {
		return f.client
	}
	c, err := client.NewInProcessClient(f.srv.mcp)
	if err != nil {
		t.Fatalf("in-process client: %v", err)
	}
	request := mcp.InitializeRequest{}
	request.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	request.Params.ClientInfo = mcp.Implementation{Name: "test", Version: "1.0"}
	if _, err := c.Initialize(f.ctx, request); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	f.client = c
	t.Cleanup(func() { c.Close() })
	return c
}

// callTool invokes one tool and returns its text or error text.
func (f *fixture) callTool(t *testing.T, name string, args map[string]any) string {
	t.Helper()
	request := mcp.CallToolRequest{}
	request.Params.Name = name
	request.Params.Arguments = args
	result, err := f.mcpClient(t).CallTool(f.ctx, request)
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	if result.IsError {
		if len(result.Content) == 0 {
			return "error"
		}
		if text, ok := result.Content[0].(mcp.TextContent); ok {
			return text.Text
		}
		return "error"
	}
	if text, ok := result.Content[0].(mcp.TextContent); ok {
		return text.Text
	}
	return ""
}

func TestNoteSendHappyPath(t *testing.T) {
	f := newFixture(t)
	out := f.callTool(t, "note_send", map[string]any{
		"goal": "work", "from": "api", "to": "web", "body": "heads up",
	})
	if !strings.Contains(out, "accepted note_id=") {
		t.Fatalf("send result = %q", out)
	}
}

func TestNoteSendRejections(t *testing.T) {
	f := newFixture(t)
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{name: "unknown goal", args: map[string]any{"goal": "ghost", "from": "api", "to": "all", "body": "x"}, want: "unknown_goal"},
		{name: "plan goal", args: map[string]any{"goal": "plan", "from": "api", "to": "all", "body": "x"}, want: "plan_goal_rejected"},
		{name: "sender not participant", args: map[string]any{"goal": "work", "from": "ghost", "to": "all", "body": "x"}, want: "not_participant"},
		{name: "destination not participant", args: map[string]any{"goal": "work", "from": "api", "to": "ghost", "body": "x"}, want: "not_participant"},
		{name: "body too large", args: map[string]any{"goal": "work", "from": "api", "to": "web", "body": strings.Repeat("x", 65)}, want: "note_too_large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if out := f.callTool(t, "note_send", tt.args); !strings.Contains(out, tt.want) {
				t.Errorf("send = %q, want %q", out, tt.want)
			}
		})
	}
}

func TestNoteSendAudienceForms(t *testing.T) {
	f := newFixture(t)
	for _, to := range []string{"web", "team:platform", "all"} {
		if out := f.callTool(t, "note_send", map[string]any{
			"goal": "work", "from": "api", "to": to, "body": "note",
		}); !strings.Contains(out, "accepted") {
			t.Errorf("send to %q = %q", to, out)
		}
	}
}

func TestNoteReadPagesWithCursor(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 3; i++ {
		f.callTool(t, "note_send", map[string]any{
			"goal": "work", "from": "api", "to": "all", "body": fmt.Sprintf("note %d", i),
		})
	}
	out := f.callTool(t, "note_read", map[string]any{"goal": "work", "for": "web", "since": 0})
	if !strings.Contains(out, "note 0") || !strings.Contains(out, "note 1") {
		t.Fatalf("page 1 = %q", out)
	}
	if strings.Contains(out, "note 2") {
		t.Fatalf("page 1 overshot page size: %q", out)
	}
	// Extract the next cursor and read the rest.
	next := lastCursor(out)
	if next == "" {
		t.Fatal("page 1 lacked next_cursor")
	}
	out = f.callTool(t, "note_read", map[string]any{"goal": "work", "for": "web", "since": next})
	if !strings.Contains(out, "note 2") {
		t.Fatalf("page 2 = %q", out)
	}
	if out := f.callTool(t, "note_read", map[string]any{"goal": "work", "for": "ghost", "since": 0}); !strings.Contains(out, "not_participant") {
		t.Errorf("read for non-participant = %q", out)
	}
}

// lastCursor extracts the trailing next cursor from one note_read page.
func lastCursor(page string) string {
	for _, line := range strings.Split(page, "\n") {
		if value, ok := strings.CutPrefix(line, "next_cursor="); ok {
			return value
		}
	}
	return ""
}

// completeSecondRun drives the work goal's queued attempt to completed on
// a second session carrying output identical to the done run, so a cursor
// issued for one run can be replayed against the other.
func (f *fixture) completeSecondRun(t *testing.T) model.Run {
	t.Helper()
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	crushClient := crushapi.NewClient(crushapi.NewClientID(), &http.Client{Transport: transport})
	session, err := crushClient.CreateSession(f.ctx, f.fake.BaseURL(), f.completedRun.WorkspaceID)
	if err != nil {
		t.Fatalf("second session: %v", err)
	}
	runs, err := f.st.ActiveRuns(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var run model.Run
	for _, candidate := range runs {
		if candidate.GoalID == "work" && candidate.Project == "api" {
			run = candidate
		}
	}
	if run.ID == "" {
		t.Fatal("work-goal api run missing")
	}
	err = f.st.WithinTx(f.ctx, func(tx *store.Tx) error {
		if err := tx.SetRunIdentifiers(f.ctx, run.ID, session.ID, "crush-2", "hash-2"); err != nil {
			return err
		}
		if err := tx.UpdateRunStatus(f.ctx, run.ID, model.RunDispatching, model.RunRunning); err != nil {
			return err
		}
		return tx.UpdateRunStatus(f.ctx, run.ID, model.RunRunning, model.RunCompleted)
	})
	if err != nil {
		t.Fatalf("second run completion: %v", err)
	}
	f.fake.AddMessage(session.ID, "assistant", "the quick brown fox jumps over the lazy dog repeatedly")
	completed, err := f.st.GetRun(f.ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return completed
}

func TestResultReadChunks(t *testing.T) {
	f := newFixture(t)
	completedRun := f.completedRun
	first := f.callTool(t, "result_read", map[string]any{"goal": "done", "run": completedRun.ID})
	if len(first) == 0 || strings.Contains(first, "next_cursor") == false {
		t.Fatalf("first chunk = %q, want bounded chunk with next cursor", first)
	}
	if len(strings.TrimSuffix(strings.Split(first, "\nnext_cursor=")[0], "\n")) != testLimits().MaxResultChunkBytes {
		t.Fatalf("first chunk not at the server-configured byte limit: %q", first)
	}
	cursor := lastCursor(first)
	second := f.callTool(t, "result_read", map[string]any{"goal": "done", "run": completedRun.ID, "cursor": cursor})
	if !strings.Contains(second, "the lazy dog") && !strings.Contains(second, "fox") {
		t.Fatalf("second chunk = %q", second)
	}
	if out := f.callTool(t, "result_read", map[string]any{"goal": "done", "run": completedRun.ID, "cursor": "not-a-cursor"}); !strings.Contains(out, "invalid_cursor") {
		t.Errorf("invalid cursor = %q", out)
	}
	// Cursors are opaque and server-issued (DESIGN §5.4): caller-forged
	// offsets or tags are rejected, and one run's cursor cannot be
	// replayed against another run's output.
	for _, forged := range []string{"8", "0:deadbeef"} {
		if out := f.callTool(t, "result_read", map[string]any{"goal": "done", "run": completedRun.ID, "cursor": forged}); !strings.Contains(out, "invalid_cursor") {
			t.Errorf("forged cursor %q accepted: %q", forged, out)
		}
	}
	otherRun := f.completeSecondRun(t)
	if out := f.callTool(t, "result_read", map[string]any{"goal": "work", "run": otherRun.ID, "cursor": cursor}); !strings.Contains(out, "invalid_cursor") {
		t.Errorf("cross-run cursor transfer accepted: %q", out)
	}
	if out := f.callTool(t, "result_read", map[string]any{"goal": "ghost", "run": completedRun.ID}); !strings.Contains(out, "unknown_goal") {
		t.Errorf("unknown goal = %q", out)
	}
	if out := f.callTool(t, "result_read", map[string]any{"goal": "plan", "run": completedRun.ID}); !strings.Contains(out, "plan_goal_rejected") {
		t.Errorf("plan goal = %q", out)
	}
}

func TestResultReadSourceUnavailable(t *testing.T) {
	f := newFixture(t)
	// Tear the workspace down: the session source is gone (§5.5).
	f.fake.RemoveWorkspace(f.completedRun.WorkspaceID)
	out := f.callTool(t, "result_read", map[string]any{"goal": "done", "run": f.completedRun.ID})
	if !strings.Contains(out, "source-data-unavailable") {
		t.Fatalf("pruned source = %q, want source-data-unavailable", out)
	}
}

func TestServeBindsLoopback(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- f.srv.Serve(ctx, "127.0.0.1:0") }()
	deadline := time.Now().Add(2 * time.Second)
	for f.srv.Addr() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.srv.Addr() == nil {
		t.Fatal("server did not bind")
	}
	addr := f.srv.Addr().(*net.TCPAddr)
	if !addr.IP.IsLoopback() {
		t.Fatalf("bound non-loopback %v", addr)
	}
	conn, err := net.DialTimeout("tcp", addr.String(), time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.Close()
	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after cancel")
	}
}
