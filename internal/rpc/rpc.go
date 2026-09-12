// Package rpc implements the operator-only local Unix socket RPC that
// CLI and TUI clients use to drive the daemon (DESIGN §3). The socket
// lives in a 0700 directory inside the state directory; peer OS identity
// plus socket permissions are the v1 trust boundary. Requests carry
// idempotency keys so a client retry cannot duplicate a goal or operator
// action.
package rpc

import (
	"context"

	"github.com/pshickeydev/matchmaker/internal/goalvalidate"
	"github.com/pshickeydev/matchmaker/internal/model"
	"github.com/pshickeydev/matchmaker/internal/planner"
)

const notImplemented = "not implemented"

// Request is one RPC call; IdempotencyKey deduplicates retries.
type Request struct {
	IdempotencyKey string
	Method         Method
	Submit         *SubmitRequest
	Plan           *planner.Request
	GoalID         string
	RunID          string
	Project        string
	Version        string
	BuildID        string
	Shutdown       model.DrainMode // graceful vs forced shutdown (§5.1)
	FilePath       string          // operator-selected destination for report export (§5.5)
}

// Method enumerates RPC methods: planning requests, goal submission,
// status, approvals, abandonment, report access, and shutdown.
type Method string

const (
	MethodSubmitGoal         Method = "submit_goal"
	MethodPlanGoal           Method = "plan_goal"
	MethodGoalStatus         Method = "goal_status"
	MethodListGoals          Method = "list_goals"
	MethodApproveFingerprint Method = "approve_fingerprint"
	MethodApproveVersion     Method = "approve_version"
	MethodAbandonRun         Method = "abandon_run"
	MethodResetInstance      Method = "reset_instance"
	MethodFleetStatus        Method = "fleet_status"
	MethodExportReport       Method = "export_report"
	MethodShutdown           Method = "shutdown"
)

// SubmitRequest carries a validated-or-not goal draft; validation happens
// atomically inside the daemon (DESIGN §5.2).
type SubmitRequest struct {
	Draft goalvalidate.GoalDraft
}

// Response returns method-specific results.
type Response struct {
	GoalID       string
	Validation   []goalvalidate.ValidationError
	Goal         *model.Goal
	Instances    []model.Instance
	PlanAccepted bool
}

// Handler is what the daemon implements.
type Handler interface {
	Handle(ctx context.Context, req Request) (Response, error)
}

// Server listens on the local socket.
type Server struct{}

// NewServer builds the RPC server for a handler and state directory. The
// socket is created under a 0700 directory; stale socket files are
// removed only while the daemon holds the exclusive lock (DESIGN §3).
func NewServer(stateDir string, handler Handler) *Server { panic(notImplemented) }

// Listen creates the socket and starts accepting connections after
// verifying peer OS identity.
func (s *Server) Listen() error { panic(notImplemented) }

// Close stops accepting and removes the socket.
func (s *Server) Close() error { panic(notImplemented) }

// Client is the short-lived client used by CLI and TUI invocations.
type Client struct{}

// Connect dials the daemon's socket in the state directory.
func Connect(stateDir string) (*Client, error) { panic(notImplemented) }

// Call performs one RPC with an idempotency key.
func (c *Client) Call(ctx context.Context, req Request) (Response, error) { panic(notImplemented) }

// Close drops the connection.
func (c *Client) Close() error { panic(notImplemented) }
