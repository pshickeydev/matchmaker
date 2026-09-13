// Package rpc implements the operator-only local Unix socket RPC that
// CLI and TUI clients use to drive the daemon (DESIGN §3). The socket
// lives in a 0700 directory inside the state directory; peer OS identity
// plus socket permissions are the v1 trust boundary. Requests carry
// idempotency keys so a client retry cannot duplicate a goal or operator
// action.
package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/pshickeydev/matchmaker/internal/goalvalidate"
	"github.com/pshickeydev/matchmaker/internal/model"
	"github.com/pshickeydev/matchmaker/internal/planner"
)

const (
	// sockName is the RPC socket file inside the state directory; the
	// daemonlock package records the same path for diagnostics.
	sockName = "matchmaker.sock"
	// sockDirMode and sockMode are the operator-only permissions of the
	// v1 trust boundary (DESIGN §3, §6).
	sockDirMode os.FileMode = 0o700
	sockMode    os.FileMode = 0o600
)

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
	Approval       string          // explicit operator approval for onboarding installs (plan M8 addition)
}

// Method enumerates RPC methods: planning requests, goal submission,
// status, approvals, abandonment, report access, and shutdown. The v1
// set carries one MVP addition: onboard (plan Decision 2 / M8).
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
	MethodOnboard            Method = "onboard"
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
	// MVP additions (plan Decision 2 / M8): onboarding and goal listing.
	Goals []model.Goal
	// Runs are the per-target attempts of the requested goal's steps
	// (goal_status detail; run IDs enable explicit abandonment).
	Runs      []model.Run
	Onboarded bool
	Fragment  string
	Message   string
}

// Handler is what the daemon implements.
type Handler interface {
	Handle(ctx context.Context, req Request) (Response, error)
}

// Server listens on the local socket.
type Server struct {
	stateDir string
	handler  Handler
	listener net.Listener
}

// NewServer builds the RPC server for a handler and state directory. The
// socket is created under a 0700 directory; stale socket files are
// removed only while the daemon holds the exclusive lock (DESIGN §3).
func NewServer(stateDir string, handler Handler) *Server {
	return &Server{stateDir: stateDir, handler: handler}
}

// Listen creates the socket and starts accepting connections after
// verifying peer OS identity.
func (s *Server) Listen() error {
	sockDir := filepath.Join(s.stateDir, "rpc")
	if err := os.MkdirAll(sockDir, sockDirMode); err != nil {
		return fmt.Errorf("rpc socket dir: %w", err)
	}
	if err := os.Chmod(sockDir, sockDirMode); err != nil {
		return err
	}
	sockPath := filepath.Join(sockDir, sockName)
	// The daemon holds the exclusive lock here: any leftover socket file
	// is stale (DESIGN §3).
	if err := removeStale(sockPath); err != nil {
		return err
	}
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("rpc listen: %w", err)
	}
	if err := os.Chmod(sockPath, sockMode); err != nil {
		listener.Close()
		return err
	}
	s.listener = listener
	go s.accept()
	return nil
}

// removeStale deletes a leftover socket file.
func removeStale(sockPath string) error {
	if _, err := os.Stat(sockPath); err == nil {
		return os.Remove(sockPath)
	}
	return nil
}

// accept serves connections until the listener closes.
func (s *Server) accept() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.serveConn(conn)
	}
}

// serveConn answers exactly one request with one response (plan
// Decision 7) and enforces the peer OS identity trust boundary.
func (s *Server) serveConn(conn net.Conn) {
	defer conn.Close()
	if err := verifyPeer(conn); err != nil {
		return
	}
	reader := bufio.NewReader(conn)
	var request Request
	if err := json.NewDecoder(reader).Decode(&request); err != nil {
		return
	}
	response, err := s.handler.Handle(context.Background(), request)
	if err != nil {
		response = Response{Message: err.Error()}
	}
	if err := json.NewEncoder(conn).Encode(response); err != nil {
		return
	}
}

// peerUID reads the connecting peer's OS user ID (SO_PEERCRED).
func peerUID(file *os.File) (uint32, error) {
	creds, err := syscall.GetsockoptUcred(int(file.Fd()), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	if err != nil {
		return 0, err
	}
	return creds.Uid, nil
}

// verifyPeer enforces the same-OS-user trust boundary (DESIGN §3).
func verifyPeer(conn net.Conn) error {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return errors.New("peer is not a unix connection")
	}
	file, err := unixConn.File()
	if err != nil {
		return err
	}
	defer file.Close()
	uid, err := peerUID(file)
	if err != nil {
		return err
	}
	if uid != uint32(os.Getuid()) {
		return fmt.Errorf("peer uid %d is not the daemon user", uid)
	}
	return nil
}

// Close stops accepting and removes the socket.
func (s *Server) Close() error {
	if s.listener == nil {
		return nil
	}
	err := s.listener.Close()
	sockPath := filepath.Join(s.stateDir, "rpc", sockName)
	if removeErr := removeStale(sockPath); removeErr != nil && err == nil {
		err = removeErr
	}
	return err
}

// Client is the short-lived client used by CLI and TUI invocations.
type Client struct {
	conn net.Conn
}

// Connect dials the daemon's socket in the state directory.
func Connect(stateDir string) (*Client, error) {
	sockPath := filepath.Join(stateDir, "rpc", sockName)
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("connect to daemon at %s: %w", sockPath, err)
	}
	return &Client{conn: conn}, nil
}

// Call performs one RPC with an idempotency key.
func (c *Client) Call(ctx context.Context, req Request) (Response, error) {
	var response Response
	if deadline, ok := ctx.Deadline(); ok {
		c.conn.SetDeadline(deadline)
	} else {
		c.conn.SetDeadline(defaultDeadline())
	}
	encoder := json.NewEncoder(c.conn)
	if err := encoder.Encode(req); err != nil {
		return response, err
	}
	decoder := json.NewDecoder(bufio.NewReader(c.conn))
	if err := decoder.Decode(&response); err != nil {
		return response, err
	}
	return response, nil
}

// defaultDeadline bounds one client call.
func defaultDeadline() (deadline time.Time) {
	return timeNow().Add(callTimeout)
}

// Close drops the connection.
func (c *Client) Close() error { return c.conn.Close() }

// timeNow is swappable for tests.
var timeNow = func() time.Time { return time.Now() }

// callTimeout bounds one RPC round trip.
const callTimeout = 30 * time.Second
