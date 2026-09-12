package crushapi

import (
	"context"
	"encoding/json"

	"github.com/pshickeydev/matchmaker/internal/sse"
)

// Payload types of the SSE envelopes Matchmaker consumes (DESIGN §2).
// Frames arrive as data-only SSE carrying a JSON envelope
// {type, payload} (verified v0.94.1, internal/pubsub/events.go); the kind
// is the envelope's type field. Types not listed here (lsp_event,
// mcp_event, agent_event, file, config_changed, skills_event,
// update_available, permission_notification,
// question_batch_notification) are ignored.
const (
	KindMessage           = "message"
	KindPermissionRequest = "permission_request"
	KindQuestionBatch     = "question_batch_request"
	KindRunComplete       = "run_complete"
	KindSession           = "session"
)

// PermissionRequest is the typed correlation subset of a permission
// event payload. Params is the raw tool parameters carried through as
// opaque bytes: it is required to answer the grant endpoint (which echoes
// the full request back; verified v0.94.1 internal/proto/proto.go) but
// is never logged or persisted (DESIGN §6, §5.3).
type PermissionRequest struct {
	ID          string          `json:"id"`
	SessionID   string          `json:"session_id"`
	ToolCallID  string          `json:"tool_call_id"`
	ToolName    string          `json:"tool_name"`
	Description string          `json:"description"`
	Action      string          `json:"action"`
	Path        string          `json:"path"`
	Params      json.RawMessage `json:"params"`
}

// RunComplete is the authoritative end-of-run event payload (verified
// v0.94.1, internal/proto/proto.go): emitted exactly once per top-level
// agent turn, after all message updates for the turn have flushed.
// RunID echoes the caller-supplied AgentMessage RunID and is the only
// safe correlator. Error is non-empty when the run terminated with an
// error; Cancelled is true on context cancellation; a successful run is
// error empty and cancelled false. Text and MessageID reconcile output
// streamed from earlier message events.
type RunComplete struct {
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id,omitempty"`
	MessageID string `json:"message_id"`
	Text      string `json:"text,omitempty"`
	Error     string `json:"error,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`
}

// Event is one decoded SSE envelope. Permission payloads may carry full
// tool parameters: retain only the typed correlation fields and never
// persist raw parameters (DESIGN §6, §5.3). Question payloads' raw text
// and choices are never persisted.
type Event struct {
	Kind string

	SessionID string
	RunID     string // present on run_complete

	// Permission is the typed request subset on permission_request.
	Permission *PermissionRequest

	// QuestionID is the batch ID on question_batch_request.
	QuestionID string

	// Complete is the decoded payload on run_complete.
	Complete *RunComplete
}

// EventStream is an open workspace SSE connection. Holding it open is
// what keeps the workspace alive (C1); Close detaches. Frames are decoded
// by the sse reader (DESIGN §10.1), the wire-level driver below this
// client layer; payloads are discriminated on the JSON envelope type.
type EventStream struct {
	reader *sse.Reader
}

// Events returns the decoded event channel. The channel closes on stream
// loss; callers reconnect with backoff (DESIGN §5.3).
func (s *EventStream) Events() <-chan Event { panic(notImplemented) }

// Errors returns stream-level failures separate from events.
func (s *EventStream) Errors() <-chan error { panic(notImplemented) }

// Close detaches the SSE stream without releasing the workspace hold;
// workspace release uses DeleteWorkspace (DESIGN §5.1).
func (s *EventStream) Close() error { panic(notImplemented) }

// decodeEvent discriminates one envelope payload into an Event. It is
// exported for tests of the envelope mapping.
func decodeEvent(kind string, payload json.RawMessage) (Event, error) {
	panic(notImplemented)
}

// streamContext carries the stream's cancellation.
func (s *EventStream) streamContext() context.Context { panic(notImplemented) }
