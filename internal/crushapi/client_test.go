package crushapi

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/pshickeydev/matchmaker/internal/crushtest"
)

func newTestClient(t *testing.T) (*Client, *crushtest.Server) {
	t.Helper()
	fake := crushtest.New()
	t.Cleanup(fake.Close)
	client := NewClient(NewClientID(), nil)
	return client, fake
}

func TestHealthAndVersion(t *testing.T) {
	client, fake := newTestClient(t)
	ctx := context.Background()
	if err := client.Health(ctx, fake.BaseURL()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	info, err := client.Version(ctx, fake.BaseURL())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if info.Version != "v0.94.1" || info.BuildID == "" {
		t.Errorf("version info = %+v", info)
	}
	fake.SetVersion("v0.95.0", "other-build")
	info, err = client.Version(ctx, fake.BaseURL())
	if err != nil || info.Version != "v0.95.0" || info.BuildID != "other-build" {
		t.Errorf("overridden version = %+v err=%v", info, err)
	}
}

func TestWorkspaceLifecycle(t *testing.T) {
	client, fake := newTestClient(t)
	ctx := context.Background()
	workspace, err := client.CreateWorkspace(ctx, fake.BaseURL(), "/repo/api", "/repo/api/.crush", []string{"HOME=/home/op"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if workspace.ID == "" || workspace.Path != "/repo/api" || workspace.Yolo {
		t.Errorf("workspace = %+v", workspace)
	}
	if workspace.DataDir != "/repo/api/.crush" {
		t.Errorf("data_dir = %q", workspace.DataDir)
	}
	created, ok := fake.Workspace(workspace.ID)
	if !ok || created.ClientID != client.ClientID() || !contains(created.Env, "HOME=/home/op") {
		t.Errorf("created record = %+v", created)
	}
	dup, err := client.CreateWorkspace(ctx, fake.BaseURL(), "/repo/api", "/repo/api/.crush", nil)
	if err != nil || dup.ID != workspace.ID {
		t.Errorf("duplicate create = %+v err=%v", dup, err)
	}
	got, err := client.GetWorkspace(ctx, fake.BaseURL(), workspace.ID)
	if err != nil || got.ID != workspace.ID {
		t.Errorf("GetWorkspace = %+v err=%v", got, err)
	}
	if _, err := client.GetWorkspace(ctx, fake.BaseURL(), "missing"); err == nil {
		t.Error("unknown workspace fetch succeeded")
	}
	if err := client.DeleteWorkspace(ctx, fake.BaseURL(), workspace.ID); err != nil {
		t.Fatalf("DeleteWorkspace: %v", err)
	}
	if deletes := fake.WorkspaceDeletes(); len(deletes) != 1 || deletes[0] != workspace.ID {
		t.Errorf("workspace deletes = %v", deletes)
	}
}

func TestCreateWorkspaceRejectsBadClientID(t *testing.T) {
	_, fake := newTestClient(t)
	bad := NewClient("not-a-uuid", nil)
	if _, err := bad.CreateWorkspace(context.Background(), fake.BaseURL(), "/repo/api", "", nil); err == nil {
		t.Fatal("invalid client_id accepted")
	}
}

func TestSessionSubmitAndCancel(t *testing.T) {
	client, fake := newTestClient(t)
	ctx := context.Background()
	workspace, err := client.CreateWorkspace(ctx, fake.BaseURL(), "/repo/api", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.CreateSession(ctx, fake.BaseURL(), workspace.ID)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if session.ID == "" {
		t.Fatal("empty session id")
	}
	if err := client.SubmitPrompt(ctx, fake.BaseURL(), workspace.ID, session.ID, "run-1", "do the thing"); err != nil {
		t.Fatalf("SubmitPrompt: %v", err)
	}
	submits := fake.Submits()
	if len(submits) != 1 || submits[0].SessionID != session.ID ||
		submits[0].RunID != "run-1" || submits[0].Prompt != "do the thing" {
		t.Fatalf("submits = %+v", submits)
	}
	if err := client.CancelSession(ctx, fake.BaseURL(), workspace.ID, session.ID); err != nil {
		t.Fatalf("CancelSession: %v", err)
	}
	if cancels := fake.Cancels(); len(cancels) != 1 || cancels[0].SessionID != session.ID {
		t.Errorf("cancels = %+v", cancels)
	}
}

func TestGetSessionFlattensMessages(t *testing.T) {
	client, fake := newTestClient(t)
	ctx := context.Background()
	workspace, err := client.CreateWorkspace(ctx, fake.BaseURL(), "/repo/api", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.CreateSession(ctx, fake.BaseURL(), workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	fake.SetBusy(session.ID, true)
	fake.AddMessage(session.ID, "user", "do the thing")
	fake.AddMessage(session.ID, "assistant", "done")
	info, err := client.GetSession(ctx, fake.BaseURL(), workspace.ID, session.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !info.Busy {
		t.Error("busy flag lost")
	}
	if len(info.Messages) != 2 || info.Messages[0].Role != "user" || info.Messages[0].Content != "do the thing" {
		t.Errorf("messages = %+v", info.Messages)
	}
}

func TestRespondPermissionEchoesRequest(t *testing.T) {
	client, fake := newTestClient(t)
	ctx := context.Background()
	workspace, err := client.CreateWorkspace(ctx, fake.BaseURL(), "/repo/api", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	request := PermissionRequest{
		ID:         "perm-1",
		SessionID:  "sess-1",
		ToolCallID: "call-1",
		ToolName:   "bash",
		Action:     "exec",
		Params:     json.RawMessage(`{"command":"ls"}`),
	}
	if err := client.RespondPermission(ctx, fake.BaseURL(), workspace.ID, request, GrantDeny); err != nil {
		t.Fatalf("RespondPermission: %v", err)
	}
	grants := fake.Grants()
	if len(grants) != 1 || grants[0].RequestID != "perm-1" || grants[0].Action != "deny" || grants[0].SessionID != "sess-1" {
		t.Fatalf("grants = %+v", grants)
	}
}

func TestQuestionCancel(t *testing.T) {
	client, fake := newTestClient(t)
	ctx := context.Background()
	workspace, err := client.CreateWorkspace(ctx, fake.BaseURL(), "/repo/api", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.CancelQuestion(ctx, fake.BaseURL(), workspace.ID); err != nil {
		t.Fatalf("CancelQuestion: %v", err)
	}
	if canceled := fake.QuestionsCanceled(); len(canceled) != 1 || canceled[0] != workspace.ID {
		t.Errorf("questions canceled = %v", canceled)
	}
}

func TestControlIdleGuarded(t *testing.T) {
	client, fake := newTestClient(t)
	ctx := context.Background()
	workspace, err := client.CreateWorkspace(ctx, fake.BaseURL(), "/repo/api", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Control(ctx, fake.BaseURL(), ControlShutdownIfIdle); err == nil {
		t.Fatal("shutdown_if_idle accepted while workspace live")
	}
	if err := client.DeleteWorkspace(ctx, fake.BaseURL(), workspace.ID); err != nil {
		t.Fatal(err)
	}
	if err := client.Control(ctx, fake.BaseURL(), ControlShutdownIfIdle); err != nil {
		t.Fatalf("shutdown_if_idle after release: %v", err)
	}
	if controls := fake.Controls(); len(controls) != 2 {
		t.Errorf("controls = %v", controls)
	}
}

func TestDeleteClient(t *testing.T) {
	client, fake := newTestClient(t)
	ctx := context.Background()
	if err := client.DeleteClient(ctx, fake.BaseURL()); err != nil {
		t.Fatalf("DeleteClient: %v", err)
	}
	if retired := fake.ClientDeletes(); len(retired) != 1 || retired[0] != client.ClientID() {
		t.Errorf("client deletes = %v", retired)
	}
	if _, err := client.CreateWorkspace(ctx, fake.BaseURL(), "/repo/late", "", nil); err == nil {
		t.Error("retired client created a workspace")
	}
}

func TestStreamEventsDecodesEnvelopes(t *testing.T) {
	client, fake := newTestClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workspace, err := client.CreateWorkspace(ctx, fake.BaseURL(), "/repo/api", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := client.StreamEvents(ctx, fake.BaseURL(), workspace.ID)
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	defer stream.Close()

	if err := fake.Emit(workspace.ID, "message", map[string]any{"session_id": "s1"}); err != nil {
		t.Fatal(err)
	}
	if err := fake.EmitPermission(workspace.ID, map[string]any{
		"id": "perm-9", "session_id": "s1", "tool_name": "bash", "params": map[string]string{"command": "ls"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := fake.Emit(workspace.ID, "lsp_event", map[string]any{"state": "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := fake.CompleteRun(workspace.ID, "s1", "run-7", "all done", "", false); err != nil {
		t.Fatal(err)
	}

	expectEvent(t, stream, "message", func(event Event) bool {
		return event.SessionID == "s1"
	})
	expectEvent(t, stream, "permission_request", func(event Event) bool {
		return event.Permission != nil && event.Permission.ID == "perm-9" &&
			event.Permission.SessionID == "s1" && event.Permission.ToolName == "bash"
	})
	expectEvent(t, stream, "lsp_event", func(event Event) bool {
		return event.Kind == "lsp_event"
	})
	expectEvent(t, stream, "run_complete", func(event Event) bool {
		return event.Complete != nil && event.Complete.RunID == "run-7" &&
			event.Complete.Text == "all done" && !event.Complete.Cancelled
	})
}

func TestStreamLossClosesChannel(t *testing.T) {
	client, fake := newTestClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workspace, err := client.CreateWorkspace(ctx, fake.BaseURL(), "/repo/api", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := client.StreamEvents(ctx, fake.BaseURL(), workspace.ID)
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}
	defer stream.Close()
	fake.DropStream(workspace.ID)
	select {
	case _, ok := <-stream.Events():
		if ok {
			t.Fatal("event arrived after stream drop")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("events channel not closed on stream loss")
	}
}

func TestStreamUnknownWorkspace(t *testing.T) {
	client, fake := newTestClient(t)
	if _, err := client.StreamEvents(context.Background(), fake.BaseURL(), "nope"); err == nil {
		t.Fatal("unknown workspace stream succeeded")
	}
}

func TestDecodeEventQuestionBatch(t *testing.T) {
	event, err := decodeEvent("question_batch_request", json.RawMessage(`{"id":"q1","session_id":"s1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if event.QuestionID != "q1" || event.SessionID != "s1" {
		t.Errorf("question event = %+v", event)
	}
}

// expectEvent pulls the next event and asserts kind and predicate.
func expectEvent(t *testing.T, stream *EventStream, kind string, check func(Event) bool) {
	t.Helper()
	select {
	case event, ok := <-stream.Events():
		if !ok {
			t.Fatalf("stream closed while waiting for %s", kind)
		}
		if event.Kind != kind {
			t.Fatalf("got event %q, want %q", event.Kind, kind)
		}
		if !check(event) {
			t.Fatalf("%s event failed check: %+v", kind, event)
		}
	case err := <-stream.Errors():
		t.Fatalf("stream error waiting for %s: %v", kind, err)
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", kind)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
