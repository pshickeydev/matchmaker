// Package crushapi is Matchmaker's hand-rolled, allowlist-scoped typed
// client for the pinned Crush server REST API (DESIGN §2, §9.6),
// verified against Crush v0.94.1 source (internal/server, internal/proto).
//
// Allowlist invariant (C6, DESIGN §6): this package implements only the
// endpoints enumerated in DESIGN §2 and §5. It must never grow wrappers
// for the unauthenticated shell endpoint, permissions/skip, or
// config-mutation endpoints (config/set, config/provider-key) — all
// still present in the v0.94.1 surface.
//
// Workspace API responses embed the full effective config including
// provider API keys: retain only required typed fields, never log raw
// responses, never persist them in the store.
package crushapi

import (
	"context"
	"net/http"
	"time"
)

const notImplemented = "not implemented"

// VersionInfo is the payload of GET /v1/version (DESIGN §9.6). BuildID
// distinguishes same-version rebuilds.
type VersionInfo struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildID   string `json:"build_id"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
}

// Workspace is the retained view of POST /v1/workspaces responses.
// Only typed fields required by Matchmaker are kept; the response's
// embedded effective config (including provider API keys) is discarded.
type Workspace struct {
	ID      string `json:"id"`
	Path    string `json:"path"`
	Yolo    bool   `json:"yolo"`
	DataDir string `json:"data_dir"`
}

// Client talks to one Crush server per base URL. ClientID is the process
// lifetime UUID identity used for workspace claims and SSE streams (C1).
type Client struct {
	http      *http.Client
	clientID  string
	UserAgent string
}

// NewClient builds a client bound to the given process-lifetime client ID.
func NewClient(clientID string, httpClient *http.Client) *Client {
	panic(notImplemented)
}

// ClientID returns the process-wide Crush client identity. It is retired
// only as final cleanup on daemon shutdown (DESIGN §5.1).
func (c *Client) ClientID() string { panic(notImplemented) }

// Health probes GET /v1/health for readiness.
func (c *Client) Health(ctx context.Context, baseURL string) error {
	panic(notImplemented)
}

// Version fetches GET /v1/version for the version/build pin check (C4).
func (c *Client) Version(ctx context.Context, baseURL string) (VersionInfo, error) {
	panic(notImplemented)
}

// NewClientID generates the process-lifetime UUID client identity
// (DESIGN §9.6). One identity per daemon process; it is retired only as
// final cleanup on shutdown.
func NewClientID() string { panic(notImplemented) }

// NewSessionID generates the dedicated per-attempt session identity.
// A session belongs to exactly one run attempt (DESIGN §5.3): Matchmaker
// persists the session ID before submitting the prompt, which is what
// lets permission events (which carry no RunID) be correlated to one
// attempt.
func NewSessionID() string { panic(notImplemented) }

// CreateWorkspace creates (or duplicates) a workspace keyed by path.
// The request always sets explicit path, yolo:false, data_dir, and env
// values (THREAT_MODEL §4.3.2): Matchmaker never creates a yolo
// workspace and never lets creation inherit unreviewed defaults. The
// workspace's lifetime is tied to this client's SSE claim (C1).
// Duplicates are first-create-wins on yolo/data_dir/env (DESIGN §9.6),
// so a foreign prior creator triggers recreation.
func (c *Client) CreateWorkspace(ctx context.Context, baseURL, projectPath, dataDir string, env []string) (Workspace, error) {
	panic(notImplemented)
}

// GetWorkspace fetches current workspace state.
func (c *Client) GetWorkspace(ctx context.Context, baseURL, workspaceID string) (Workspace, error) {
	panic(notImplemented)
}

// DeleteWorkspace releases Matchmaker's workspace creation hold
// (DELETE /v1/workspaces/{id}?client_id={client_id}) during drain.
func (c *Client) DeleteWorkspace(ctx context.Context, baseURL, workspaceID string) error {
	panic(notImplemented)
}

// Session is the retained view of POST /v1/workspaces/{id}/sessions
// responses. The server generates the session ID; Matchmaker persists
// it before submitting the prompt (DESIGN §5.3). Busy is computed by the
// server, not persisted with the session.
type Session struct {
	ID    string `json:"id"`
	Busy  bool   `json:"is_busy"`
	Title string `json:"title"`
}

// CreateSession creates the run attempt's dedicated session
// (POST /v1/workspaces/{id}/sessions). A session belongs to exactly one
// attempt (DESIGN §5.3): Matchmaker persists the returned ID before
// submitting the prompt, which is what lets permission events (which
// carry no RunID) be correlated to one attempt.
func (c *Client) CreateSession(ctx context.Context, baseURL, workspaceID string) (Session, error) {
	panic(notImplemented)
}

// SubmitPrompt submits a run prompt on the run's dedicated session with
// the caller-supplied RunID (POST /v1/workspaces/{id}/agent, body
// {session_id, run_id, prompt}). The endpoint is fire-and-forget: it
// validates, accepts with 202 and an empty body, and dispatches the run
// on a goroutine (verified v0.94.1 internal/server/proto.go); outcomes
// arrive only through SSE run_complete events echoing the RunID.
// Matchmaker persists the session ID, RunID, and rendered-prompt hash
// before calling this; the same RunID is never reused to submit a second
// prompt, and Crush's queued-prompts endpoint is never used.
func (c *Client) SubmitPrompt(ctx context.Context, baseURL, workspaceID, sessionID, runID, prompt string) error {
	panic(notImplemented)
}

// GrantAction enumerates permission responses.
type GrantAction string

const (
	GrantAllow GrantAction = "allow"
	GrantDeny  GrantAction = "deny"
)

// RespondPermission answers one correlated permission_request through
// the /permissions/grant endpoint. The request body echoes the full
// PermissionRequest payload back alongside the action (verified v0.94.1
// internal/proto/proto.go), which is why the typed request (with opaque
// params) is carried on the event. Crush also defines an allow_session
// action; Matchmaker uses only allow and deny — grant_all is per event,
// never server-wide, and never via permissions/skip (C6, DESIGN §5.3).
func (c *Client) RespondPermission(ctx context.Context, baseURL, workspaceID string, req PermissionRequest, action GrantAction) error {
	panic(notImplemented)
}

// CancelQuestion immediately cancels the pending question batch for
// the workspace (POST /v1/workspaces/{id}/questions/cancel, no request
// body; verified v0.94.1). V1 has no interactive question policy;
// cancellation is fail closed (DESIGN §5.3).
func (c *Client) CancelQuestion(ctx context.Context, baseURL, workspaceID string) error {
	panic(notImplemented)
}

// CancelSession sends POST .../sessions/{sid}/cancel once; the request
// time is persisted so recovery does not repeatedly issue it (§5.3).
func (c *Client) CancelSession(ctx context.Context, baseURL, workspaceID, sessionID string) error {
	panic(notImplemented)
}

// SessionInfo is the durable-session view used by recovery and result
// reading (GET /v1/workspaces/{id}/sessions/{sid} plus
// .../sessions/{sid}/messages; equivalent to `crush session show --json`,
// DESIGN §5.3). Busy is server-computed from in-flight agent runs.
type SessionInfo struct {
	ID       string
	Busy     bool
	Messages []SessionMessage
}

// SessionMessage is one retained session message. Recovery uses a
// persisted matching user message (available via
// .../sessions/{sid}/messages/user) as proof of prompt acceptance; a
// busy session returns the attempt to running; session idle alone is
// never terminal evidence (DESIGN §5.3).
type SessionMessage struct {
	Role    string // "user" | "assistant" | ...
	Content string
}

// GetSession reads session state and messages through the workspace
// session API.
func (c *Client) GetSession(ctx context.Context, baseURL, workspaceID, sessionID string) (SessionInfo, error) {
	panic(notImplemented)
}

// ControlAction enumerates POST /v1/control actions (DESIGN §2, §9.6).
type ControlAction string

const (
	ControlShutdown       ControlAction = "shutdown"
	ControlShutdownIfIdle ControlAction = "shutdown_if_idle"
)

// Control issues POST /v1/control. shutdown is idle-guarded: Matchmaker
// releases workspace streams and holds before calling shutdown_if_idle
// (DESIGN §9.6).
func (c *Client) Control(ctx context.Context, baseURL string, action ControlAction) error {
	panic(notImplemented)
}

// DeleteClient announces client exit (DELETE /v1/clients/{id}). Used only
// as final cleanup on clean daemon shutdown; never to stop one instance
// (DESIGN §5.1).
func (c *Client) DeleteClient(ctx context.Context, baseURL string) error {
	panic(notImplemented)
}

// StreamEvents opens the workspace SSE stream
// (GET /v1/workspaces/{id}/events). Matchmaker holds it open to keep the
// workspace alive (C1) and reconnects with backoff on loss.
func (c *Client) StreamEvents(ctx context.Context, baseURL, workspaceID string) (*EventStream, error) {
	panic(notImplemented)
}

// RequestTimeout is the default per-request HTTP timeout applied to
// non-SSE calls.
const RequestTimeout = 30 * time.Second
