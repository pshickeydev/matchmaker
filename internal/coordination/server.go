// Package coordination hosts the one multiplexed coordination MCP server
// (HTTP transport, loopback) exposing note_send, note_read, and
// result_read to fleet instances (DESIGN §5.4, §9.4).
//
// Registration is an explicit per-project operator choice (onboarding
// writes the Matchmaker-owned crushrc block); once registered the tools
// are available project-wide and Matchmaker cannot enable or revoke them
// per step or run.
package coordination

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/pshickeydev/matchmaker/internal/config"
	"github.com/pshickeydev/matchmaker/internal/crushapi"
	"github.com/pshickeydev/matchmaker/internal/model"
	"github.com/pshickeydev/matchmaker/internal/notes"
	"github.com/pshickeydev/matchmaker/internal/store"
)

const (
	// serverName and serverVersion identify the coordination server on
	// the MCP wire.
	serverName    = "matchmaker"
	serverVersion = "v1"
	// audienceAll addresses every participant of a goal (DESIGN §5.4).
	audienceAll = "all"
	// resultCursorBase is the beginning-of-output cursor.
	resultCursorBase = 0
	// cursorKeySize is the HMAC key length behind opaque result cursors.
	cursorKeySize = 32
	// shutdownGrace bounds the graceful HTTP shutdown when Serve's
	// context ends, so a hung connection cannot block daemon exit.
	shutdownGrace = 5 * time.Second
)

// Server is the coordination MCP server. It binds loopback only and
// multiplexes across goals via the explicit goal argument of each tool.
type Server struct {
	svc    *notes.Service
	st     *store.Store
	client *crushapi.Client
	limits config.NoteLimits
	mcp    *server.MCPServer

	// cursorKey is the process-lifetime HMAC key behind opaque result
	// cursors; a daemon restart invalidates previously issued cursors.
	cursorKey []byte

	// mu guards listener, written by Serve and read by Addr (used by
	// onboarding on the RPC goroutine).
	mu       sync.Mutex
	listener net.Listener
	http     *http.Server
}

// New builds the server over the note service, store (for run references),
// the Crush client (for reading source session data), and limits.
func New(svc *notes.Service, st *store.Store, client *crushapi.Client, limits config.NoteLimits) *Server {
	key := make([]byte, cursorKeySize)
	if _, err := rand.Read(key); err != nil {
		// Without a cursor key the server could only issue forgeable
		// cursors; fail loudly instead (DESIGN §7).
		panic("coordination: cursor key: " + err.Error())
	}
	s := &Server{
		svc: svc, st: st, client: client, limits: limits, cursorKey: key,
		mcp: server.NewMCPServer(serverName, serverVersion),
	}
	s.registerTools()
	return s
}

// registerTools declares note_send, note_read, and result_read with
// their explicit goal multiplexing arguments (DESIGN §5.4, §9.4).
func (s *Server) registerTools() {
	send := mcp.NewTool("note_send",
		mcp.WithDescription("Pass a short coordination note to other agents working the same goal. "+
			"Notes are cooperative and non-confidential: never include secrets or trusted instructions."),
		mcp.WithString("goal", mcp.Required(), mcp.Description("Goal ID being worked")),
		mcp.WithString("from", mcp.Required(), mcp.Description("Your claimed project name")),
		mcp.WithString("to", mcp.Required(), mcp.Description("Destination project name, tag, or all")),
		mcp.WithString("body", mcp.Required(), mcp.Description("Note body")),
	)
	s.mcp.AddTool(send, s.handleNoteSend)

	read := mcp.NewTool("note_read",
		mcp.WithDescription("Read notes addressed to your claimed project, ascending by note_id, "+
			"after a last-observed note_id cursor."),
		mcp.WithString("goal", mcp.Required(), mcp.Description("Goal ID being worked")),
		mcp.WithString("for", mcp.Required(), mcp.Description("Your claimed project name")),
		mcp.WithNumber("since", mcp.Description("Last observed note_id (default 0)")),
		mcp.WithNumber("page_size", mcp.Description("Page size; may only lower the server maximum")),
	)
	s.mcp.AddTool(read, s.handleNoteRead)

	result := mcp.NewTool("result_read",
		mcp.WithDescription("Fetch the final output of a completed run in bounded chunks. "+
			"An absent next cursor means end of output."),
		mcp.WithString("goal", mcp.Required(), mcp.Description("Goal ID being worked")),
		mcp.WithString("run", mcp.Required(), mcp.Description("Run handle from an upstream reference")),
		mcp.WithString("cursor", mcp.Description("Opaque next cursor from a previous chunk")),
	)
	s.mcp.AddTool(result, s.handleResultRead)
}

// Serve binds the loopback listener and serves the three tools until ctx
// is canceled. The implementation wires these handlers to
// mark3labs/mcp-go (DESIGN §10.1) with an HTTP transport.
func (s *Server) Serve(ctx context.Context, addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("coordination bind %s: %w", addr, err)
	}
	if !loopback(listener.Addr()) {
		listener.Close()
		return fmt.Errorf("coordination bind %s is not loopback", addr)
	}
	s.mu.Lock()
	s.listener = listener
	s.mu.Unlock()
	handler := server.NewStreamableHTTPServer(s.mcp, server.WithStateLess(true))
	s.http = &http.Server{Handler: handler}
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.http.Serve(listener) }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		s.http.Shutdown(shutdownCtx)
		return nil
	case err := <-serveErr:
		return err
	}
}

// loopback reports whether the bound address is loopback (DESIGN §5.4,
// §6: coordination binds loopback only).
func loopback(addr net.Addr) bool {
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok {
		return false
	}
	return tcpAddr.IP.IsLoopback()
}

// Addr returns the bound loopback address used in the generated crushrc
// registration fragment (DESIGN §5.4); nil before Serve binds.
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

// Tool handlers. All take an explicit goal argument to multiplex across
// goals; from/for are caller-supplied claimed project names for
// addressing and cursor selection, not authenticated identities; plan
// goals are rejected (DESIGN §5.7).

// toolArgs extracts one call's typed arguments.
func toolArgs(request mcp.CallToolRequest) map[string]any {
	args, _ := request.Params.Arguments.(map[string]any)
	if args == nil {
		return map[string]any{}
	}
	return args
}

// handleNoteSend implements note_send(goal, from, to, body).
func (s *Server) handleNoteSend(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := toolArgs(request)
	goalID, _ := args["goal"].(string)
	from, _ := args["from"].(string)
	to, _ := args["to"].(string)
	body, _ := args["body"].(string)
	noteID, err := s.NoteSend(ctx, goalID, from, to, body)
	if err != nil {
		return mcp.NewToolResultErrorFromErr(err.Error(), err), nil
	}
	return mcp.NewToolResultText(fmt.Sprintf("accepted note_id=%d", noteID)), nil
}

// handleNoteRead implements note_read(goal, for, since).
func (s *Server) handleNoteRead(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := toolArgs(request)
	goalID, _ := args["goal"].(string)
	forProject, _ := args["for"].(string)
	since := argInt(args, "since", 0)
	pageSize := int(argInt(args, "page_size", 0))
	items, next, err := s.NoteRead(ctx, goalID, forProject, since, pageSize)
	if err != nil {
		return mcp.NewToolResultErrorFromErr(err.Error(), err), nil
	}
	var lines []string
	for _, item := range items {
		lines = append(lines, fmt.Sprintf("note_id=%d from=%s body=%s", item.NoteID, item.From, item.Body))
	}
	lines = append(lines, fmt.Sprintf("next_cursor=%d", next))
	return mcp.NewToolResultText(strings.Join(lines, "\n")), nil
}

// handleResultRead implements result_read(goal, run, cursor).
func (s *Server) handleResultRead(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := toolArgs(request)
	goalID, _ := args["goal"].(string)
	runID, _ := args["run"].(string)
	cursorText, _ := args["cursor"].(string)
	chunk, next, err := s.ResultRead(ctx, goalID, runID, []byte(cursorText))
	if err != nil {
		return mcp.NewToolResultErrorFromErr(err.Error(), err), nil
	}
	if len(next) == 0 {
		return mcp.NewToolResultText(string(chunk)), nil
	}
	return mcp.NewToolResultText(string(chunk) + "\nnext_cursor=" + string(next)), nil
}

// argInt reads one numeric tool argument with a default.
func argInt(args map[string]any, name string, fallback int64) int64 {
	value, ok := args[name]
	if !ok {
		return fallback
	}
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int64:
		return typed
	case int:
		return int64(typed)
	case string:
		if parsed, err := strconv.ParseInt(typed, 10, 64); err == nil {
			return parsed
		}
	}
	return fallback
}

// NoteSend implements note_send(goal, from, to, body).
func (s *Server) NoteSend(ctx context.Context, goalID, from, to, body string) (int64, error) {
	participants, err := s.goalParticipants(ctx, goalID)
	if err != nil {
		return 0, err
	}
	audience, err := parseAudience(to, participants)
	if err != nil {
		return 0, err
	}
	return s.svc.Send(ctx, goalID, from, audience, body)
}

// NoteRead implements note_read(goal, for, since).
func (s *Server) NoteRead(ctx context.Context, goalID, forProject string, since int64, pageSize int) ([]noteReadItem, int64, error) {
	read, next, err := s.svc.Read(ctx, goalID, forProject, since, pageSize)
	if err != nil {
		return nil, 0, err
	}
	items := make([]noteReadItem, 0, len(read))
	for _, note := range read {
		items = append(items, noteReadItem{NoteID: note.NoteID, From: note.From, Body: note.Body})
	}
	return items, next, nil
}

// noteReadItem is one returned note plus its note_id ordering key.
type noteReadItem struct {
	NoteID int64
	From   string
	Body   string
}

// ResultRead implements result_read(goal, run, cursor) (DESIGN §5.4): it
// accepts only a completed run belonging to the supplied goal, resolves
// the run's persisted workspace, session, and final message references,
// reads the source data from Crush's durable session store, and returns a
// bounded chunk plus an opaque next cursor; an absent next cursor means
// end of output. Invalid goals, runs, references, and cursors return
// explicit errors. The byte limit is server-configured and cannot be
// increased by the caller. If referenced session data was pruned, it
// returns an explicit source-data-unavailable status (§5.5).
func (s *Server) ResultRead(ctx context.Context, goalID, runID string, cursor []byte) (chunk []byte, next []byte, err error) {
	goal, err := s.st.Goal(ctx, goalID)
	if err != nil {
		return nil, nil, &notes.SendError{Code: notes.CodeUnknownGoal}
	}
	if goal.Type == model.GoalTypePlan {
		return nil, nil, &notes.SendError{Code: notes.CodePlanGoal}
	}
	run, err := s.st.GetRun(ctx, runID)
	if err != nil || run.GoalID != goalID {
		return nil, nil, errors.New("invalid_run")
	}
	if run.Status != model.RunCompleted {
		return nil, nil, errors.New("run_not_completed")
	}
	offset := resultCursorBase
	if len(cursor) > 0 {
		var cursorErr error
		offset, cursorErr = s.decodeCursor(runID, cursor)
		if cursorErr != nil {
			return nil, nil, cursorErr
		}
	}
	info, err := s.client.GetSession(ctx, run.ServerURL, run.WorkspaceID, run.SessionID)
	if err != nil {
		return nil, nil, fmt.Errorf("source-data-unavailable: %w", err)
	}
	output := finalOutput(info.Messages)
	if output == "" {
		return nil, nil, errors.New("source-data-unavailable")
	}
	if offset > len(output) {
		return nil, nil, &notes.SendError{Code: notes.CodeInvalidCursor}
	}
	end := offset + s.limits.MaxResultChunkBytes
	if end > len(output) {
		end = len(output)
	}
	chunk = []byte(output[offset:end])
	if end < len(output) {
		next = s.encodeCursor(runID, end)
	}
	return chunk, next, nil
}

// encodeCursor issues one opaque result cursor: the offset is bound to
// its run with an HMAC under the server's process-lifetime key, so
// callers can neither forge cursors nor transfer them between runs
// (DESIGN §5.4). The cursor is base-10 offset plus hex tag.
func (s *Server) encodeCursor(runID string, offset int) []byte {
	return []byte(fmt.Sprintf("%d:%s", offset, s.cursorTag(runID, offset)))
}

// decodeCursor opens one previously issued cursor for runID. Anything
// not issued by this process for exactly this run is an invalid cursor.
func (s *Server) decodeCursor(runID string, cursor []byte) (int, error) {
	offsetText, tag, found := strings.Cut(string(cursor), ":")
	if !found {
		return 0, &notes.SendError{Code: notes.CodeInvalidCursor}
	}
	offset, err := strconv.Atoi(offsetText)
	if err != nil || offset < 0 {
		return 0, &notes.SendError{Code: notes.CodeInvalidCursor}
	}
	if !hmac.Equal([]byte(tag), []byte(s.cursorTag(runID, offset))) {
		return 0, &notes.SendError{Code: notes.CodeInvalidCursor}
	}
	return offset, nil
}

// cursorTag computes the HMAC binding one offset to one run.
func (s *Server) cursorTag(runID string, offset int) string {
	mac := hmac.New(sha256.New, s.cursorKey)
	fmt.Fprintf(mac, "result_read:%s:%d", runID, offset)
	return hex.EncodeToString(mac.Sum(nil))
}

// finalOutput extracts the final assistant message content of a session.
func finalOutput(messages []crushapi.SessionMessage) string {
	output := ""
	for _, message := range messages {
		if message.Role == "assistant" && message.Content != "" {
			output = message.Content
		}
	}
	return output
}

// goalParticipants loads a goal's participants for audience parsing.
func (s *Server) goalParticipants(ctx context.Context, goalID string) (map[string][]string, error) {
	goal, err := s.st.Goal(ctx, goalID)
	if err != nil {
		return nil, &notes.SendError{Code: notes.CodeUnknownGoal}
	}
	if goal.Type == model.GoalTypePlan {
		return nil, &notes.SendError{Code: notes.CodePlanGoal}
	}
	participants := make(map[string][]string)
	for _, targets := range goal.FrozenTargets {
		for _, target := range targets {
			if _, seen := participants[target.Project]; !seen {
				participants[target.Project] = target.Tags
			}
		}
	}
	return participants, nil
}

// parseAudience interprets one destination as a participant project
// name, a participant tag, or all (DESIGN §5.4).
func parseAudience(to string, participants map[string][]string) (model.Audience, error) {
	switch {
	case to == audienceAll:
		return model.Audience{All: true}, nil
	case projectNamed(participants, to):
		return model.Audience{Project: to}, nil
	case tagCarried(participants, to):
		return model.Audience{Tag: to}, nil
	}
	return model.Audience{}, &notes.SendError{Code: notes.CodeNotParticipant}
}

// projectNamed reports whether to names a participant project. A project
// with no tags still maps to a non-nil empty slice, so presence in the
// map is checked explicitly.
func projectNamed(participants map[string][]string, to string) bool {
	_, ok := participants[to]
	return ok
}

// tagCarried reports whether any participant carries to as a tag.
func tagCarried(participants map[string][]string, to string) bool {
	for _, tags := range participants {
		for _, tag := range tags {
			if tag == to {
				return true
			}
		}
	}
	return false
}
