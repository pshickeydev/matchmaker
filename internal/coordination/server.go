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
	"net"

	"github.com/pshickeydev/matchmaker/internal/config"
	"github.com/pshickeydev/matchmaker/internal/crushapi"
	"github.com/pshickeydev/matchmaker/internal/notes"
	"github.com/pshickeydev/matchmaker/internal/store"
)

const notImplemented = "not implemented"

// Server is the coordination MCP server. It binds loopback only and
// multiplexes across goals via the explicit goal argument of each tool.
type Server struct{}

// New builds the server over the note service, store (for run references),
// the Crush client (for reading source session data), and limits.
func New(svc *notes.Service, st *store.Store, client *crushapi.Client, limits config.NoteLimits) *Server {
	panic(notImplemented)
}

// Serve binds the loopback listener and serves the three tools until ctx
// is canceled. The implementation wires these handlers to
// mark3labs/mcp-go (DESIGN §10.1) with an HTTP transport.
func (s *Server) Serve(ctx context.Context, addr string) error { panic(notImplemented) }

// Addr returns the bound loopback address used in the generated crushrc
// registration fragment (DESIGN §5.4).
func (s *Server) Addr() net.Addr { panic(notImplemented) }

// Tool handlers. All take an explicit goal argument to multiplex across
// goals; from/for are caller-supplied claimed project names for
// addressing and cursor selection, not authenticated identities; plan
// goals are rejected (DESIGN §5.7).

// NoteSend implements note_send(goal, from, to, body).
func (s *Server) NoteSend(ctx context.Context, goalID, from, to, body string) (int64, error) {
	panic(notImplemented)
}

// NoteRead implements note_read(goal, for, since).
func (s *Server) NoteRead(ctx context.Context, goalID, forProject string, since int64, pageSize int) ([]noteReadItem, int64, error) {
	panic(notImplemented)
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
	panic(notImplemented)
}
