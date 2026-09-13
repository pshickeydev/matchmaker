package crushapi

import (
	"context"
	"encoding/json"
	"net/http"

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

// envelope is the wire shape of one SSE data frame.
type envelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// unwrapPayload opens the nested {type, payload} event envelope one
// level so typed decodes see the actual event data; a payload without
// the nested shape is returned unchanged.
func unwrapPayload(payload json.RawMessage) json.RawMessage {
	var inner envelope
	if err := json.Unmarshal(payload, &inner); err != nil || len(inner.Payload) == 0 {
		return payload
	}
	return inner.Payload
}

// sessionPayload extracts the session identifier shared by several event
// types. Session events carry it as `id` (verified v0.94.1); message
// events carry `session_id`.
type sessionPayload struct {
	SessionID string `json:"session_id"`
	ID        string `json:"id"`
}

// questionPayload extracts the batch ID of a question event.
type questionPayload struct {
	ID         string `json:"id"`
	SessionID  string `json:"session_id"`
	ToolCallID string `json:"tool_call_id"`
}

// EventStream is an open workspace SSE connection. Holding it open is
// what keeps the workspace alive (C1); Close detaches. Frames are decoded
// by the sse reader (DESIGN §10.1), the wire-level driver below this
// client layer; payloads are discriminated on the JSON envelope type.
type EventStream struct {
	reader *sse.Reader
	events chan Event
	errors chan error
	ctx    context.Context
	cancel context.CancelFunc
}

// newEventStream wires one open SSE response into a decoded event stream.
func newEventStream(ctx context.Context, cancel context.CancelFunc, resp *http.Response) *EventStream {
	stream := &EventStream{
		events: make(chan Event),
		errors: make(chan error, 1),
		ctx:    ctx,
		cancel: cancel,
	}
	raw := make(chan []byte)
	stream.reader = sse.NewReader(ctx, resp, raw)
	stream.reader.Start()
	go stream.pump(raw)
	return stream
}

// pump decodes raw SSE payloads into typed events until stream loss.
func (s *EventStream) pump(raw chan []byte) {
	defer close(s.events)
	for payload := range raw {
		var env envelope
		if err := json.Unmarshal(payload, &env); err != nil {
			select {
			case s.errors <- err:
			default:
			}
			continue
		}
		event, err := decodeEvent(env.Type, env.Payload)
		if err != nil {
			select {
			case s.errors <- err:
			default:
			}
			continue
		}
		select {
		case s.events <- event:
		case <-s.ctx.Done():
			return
		}
	}
}

// Events returns the decoded event channel. The channel closes on stream
// loss; callers reconnect with backoff (DESIGN §5.3).
func (s *EventStream) Events() <-chan Event { return s.events }

// Errors returns stream-level failures separate from events.
func (s *EventStream) Errors() <-chan error { return s.errors }

// Close detaches the SSE stream without releasing the workspace hold;
// workspace release uses DeleteWorkspace (DESIGN §5.1).
func (s *EventStream) Close() error {
	s.cancel()
	return nil
}

// streamContext carries the stream's cancellation.
func (s *EventStream) streamContext() context.Context { return s.ctx }

// decodeEvent discriminates one envelope payload into an Event. The
// v0.94.1 wire nests one more layer than the outer envelope: the payload
// is itself {type, payload} holding the typed event data (verified
// against a live v0.94.1 server), so the nested layer is unwrapped
// before the typed decodes. It is exported for tests of the envelope
// mapping.
func decodeEvent(kind string, payload json.RawMessage) (Event, error) {
	payload = unwrapPayload(payload)
	event := Event{Kind: kind}
	switch kind {
	case KindMessage, KindSession:
		var session sessionPayload
		if err := json.Unmarshal(payload, &session); err != nil {
			return event, err
		}
		event.SessionID = session.SessionID
		if event.SessionID == "" {
			event.SessionID = session.ID
		}
	case KindPermissionRequest:
		var request PermissionRequest
		if err := json.Unmarshal(payload, &request); err != nil {
			return event, err
		}
		event.SessionID = request.SessionID
		event.Permission = &request
	case KindQuestionBatch:
		var question questionPayload
		if err := json.Unmarshal(payload, &question); err != nil {
			return event, err
		}
		event.SessionID = question.SessionID
		event.QuestionID = question.ID
	case KindRunComplete:
		var complete RunComplete
		if err := json.Unmarshal(payload, &complete); err != nil {
			return event, err
		}
		event.SessionID = complete.SessionID
		event.RunID = complete.RunID
		event.Complete = &complete
	}
	return event, nil
}
