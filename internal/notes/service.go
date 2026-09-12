// Package notes implements note passing between agents working the same
// goal (DESIGN §4.6, §5.4): acceptance with audience expansion, durable
// per-(goal, instance) delivery cursors, at-least-once read semantics,
// and bounded selection for prompt injection.
package notes

import (
	"context"

	"github.com/pshickeydev/matchmaker/internal/config"
	"github.com/pshickeydev/matchmaker/internal/model"
	"github.com/pshickeydev/matchmaker/internal/store"
)

const notImplemented = "not implemented"

// SendError enumerates the stable note error codes of DESIGN §5.4:
// note_too_large, goal_note_limit, rate_limited, invalid_cursor, plus
// goal-scoping rejections for unknown or completed goals.
type SendError struct {
	Code ErrCode
}

func (e *SendError) Error() string { return string(e.Code) }

// ErrCode is the typed set of stable error codes.
type ErrCode string

// Stable error codes.
const (
	CodeNoteTooLarge   ErrCode = "note_too_large"
	CodeGoalNoteLimit  ErrCode = "goal_note_limit"
	CodeRateLimited    ErrCode = "rate_limited"
	CodeInvalidCursor  ErrCode = "invalid_cursor"
	CodeUnknownGoal    ErrCode = "unknown_goal"
	CodeNotParticipant ErrCode = "not_participant"
	CodePlanGoal       ErrCode = "plan_goal_rejected"
)

// Service validates and accepts notes.
type Service struct{}

// New builds the note service over the store and limits.
func New(st *store.Store, limits config.NoteLimits) *Service { panic(notImplemented) }

// Send validates that from and every expanded audience member participate
// in the active goal (acknowledging that any project session can claim
// any participant), checks limits before any write, freezes audience
// expansion, and stores the note with a store-generated monotonically
// increasing note_id (DESIGN §5.4). Plan goals are rejected.
func (s *Service) Send(ctx context.Context, goalID, from string, to model.Audience, body string) (int64, error) {
	panic(notImplemented)
}

// Read validates that for participates in the active goal, interprets
// since as the last observed note_id, returns notes addressed to the
// claimed project with greater IDs ascending, and includes an opaque next
// cursor. Reads never advance the durable delivery cursor. Page size may
// only lower the server maxima; truncation returns a next cursor and
// never splits a note body.
func (s *Service) Read(ctx context.Context, goalID, forProject string, since int64, pageSize int) ([]model.Note, int64, error) {
	panic(notImplemented)
}

// Selector picks notes for prompt injection.
type Selector struct{}

// SelectForInjection returns addressed notes after the target's durable
// cursor, including as many whole notes as fit the injection-byte limit,
// and the selected high-water note_id (DESIGN §5.4). The high-water is
// recorded with the run and the audience cursor advances in the same
// transaction as accepted prompt submission; a crash before that
// transaction may re-inject, a crash after it never loses.
func (sel *Selector) SelectForInjection(ctx context.Context, goalID, project string, byteLimit int) ([]model.Note, int64, error) {
	panic(notImplemented)
}
