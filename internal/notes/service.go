// Package notes implements note passing between agents working the same
// goal (DESIGN §4.6, §5.4): acceptance with audience expansion, durable
// per-(goal, instance) delivery cursors, at-least-once read semantics,
// and bounded selection for prompt injection.
package notes

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/pshickeydev/matchmaker/internal/config"
	"github.com/pshickeydev/matchmaker/internal/model"
	"github.com/pshickeydev/matchmaker/internal/store"
)

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
type Service struct {
	st     *store.Store
	limits config.NoteLimits

	rateMu      sync.Mutex
	rateSamples []time.Time
}

// New builds the note service over the store and limits.
func New(st *store.Store, limits config.NoteLimits) *Service {
	return &Service{st: st, limits: limits}
}

// loadGoal resolves the active work goal and its participants. Notes are
// accepted only for active goals; plan goals are rejected (DESIGN §5.7).
func (s *Service) loadGoal(ctx context.Context, goalID string) (map[string][]string, error) {
	goal, err := s.st.Goal(ctx, goalID)
	if err != nil {
		return nil, &SendError{CodeUnknownGoal}
	}
	if goal.Status != model.GoalActive {
		return nil, &SendError{CodeUnknownGoal}
	}
	if goal.Type == model.GoalTypePlan {
		return nil, &SendError{CodePlanGoal}
	}
	participants := make(map[string][]string)
	for _, targets := range goal.FrozenTargets {
		for _, target := range targets {
			if _, seen := participants[target.Project]; seen {
				continue
			}
			participants[target.Project] = target.Tags
		}
	}
	if len(participants) == 0 {
		return nil, &SendError{CodeUnknownGoal}
	}
	return participants, nil
}

// rateLimit enforces the coordination request window before any write or
// response allocation (DESIGN §5.4).
func (s *Service) rateLimit() error {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	now := time.Now()
	windowStart := now.Add(-s.limits.RateWindow)
	kept := s.rateSamples[:0]
	for _, sample := range s.rateSamples {
		if sample.After(windowStart) {
			kept = append(kept, sample)
		}
	}
	if len(kept) >= s.limits.MaxRequestsPerWindow {
		s.rateSamples = kept
		return &SendError{CodeRateLimited}
	}
	s.rateSamples = append(kept, now)
	return nil
}

// Send validates that from and every expanded audience member participate
// in the active goal (acknowledging that any project session can claim
// any participant), checks limits before any write, freezes audience
// expansion, and stores the note with a store-generated monotonically
// increasing note_id (DESIGN §5.4). Plan goals are rejected.
func (s *Service) Send(ctx context.Context, goalID, from string, to model.Audience, body string) (int64, error) {
	if len(body) > s.limits.MaxNoteBodyBytes {
		return 0, &SendError{CodeNoteTooLarge}
	}
	if err := s.rateLimit(); err != nil {
		return 0, err
	}
	participants, err := s.loadGoal(ctx, goalID)
	if err != nil {
		return 0, err
	}
	if _, ok := participants[from]; !ok {
		return 0, &SendError{CodeNotParticipant}
	}
	audience := model.ExpandAudience(to, participants)
	if len(audience) == 0 {
		return 0, &SendError{CodeNotParticipant}
	}
	for _, member := range audience {
		if _, ok := participants[member]; !ok {
			return 0, &SendError{CodeNotParticipant}
		}
	}
	count, err := s.st.CountNotes(ctx, goalID)
	if err != nil {
		return 0, err
	}
	if count >= s.limits.MaxNotesPerGoal {
		return 0, &SendError{CodeGoalNoteLimit}
	}
	var noteID int64
	err = s.st.WithinTx(ctx, func(tx *store.Tx) error {
		var err error
		noteID, err = tx.AppendNote(ctx, model.Note{
			GoalID: goalID,
			From:   from,
			To:     to,
			Body:   body,
		})
		return err
	})
	if err != nil {
		return 0, err
	}
	return noteID, nil
}

// Read validates that for participates in the active goal, interprets
// since as the last observed note_id, returns notes addressed to the
// claimed project with greater IDs ascending, and includes an opaque next
// cursor. Reads never advance the durable delivery cursor. Page size may
// only lower the server maxima; truncation returns a next cursor and
// never splits a note body.
func (s *Service) Read(ctx context.Context, goalID, forProject string, since int64, pageSize int) ([]model.Note, int64, error) {
	if since < 0 {
		return nil, 0, &SendError{CodeInvalidCursor}
	}
	participants, err := s.loadGoal(ctx, goalID)
	if err != nil {
		return nil, 0, err
	}
	if _, ok := participants[forProject]; !ok {
		return nil, 0, &SendError{CodeNotParticipant}
	}
	if pageSize <= 0 || pageSize > s.limits.MaxReadPageSize {
		pageSize = s.limits.MaxReadPageSize
	}
	all, err := s.st.NotesFor(ctx, goalID, forProject, since)
	if err != nil {
		return nil, 0, err
	}
	if len(all) <= pageSize {
		return all, lastNoteID(all, since), nil
	}
	page := all[:pageSize]
	return page, lastNoteID(page, since), nil
}

// lastNoteID is the next cursor of one returned page.
func lastNoteID(notes []model.Note, fallback int64) int64 {
	if len(notes) == 0 {
		return fallback
	}
	return notes[len(notes)-1].NoteID
}

// Selector picks notes for prompt injection.
type Selector struct {
	st *store.Store
}

// NewSelector builds the injection selector over the store. The selector
// reads the durable audience cursor and addressed notes; cursor
// advancement itself stays with dispatch's submission transaction
// (DESIGN §5.4).
func NewSelector(st *store.Store) *Selector {
	return &Selector{st: st}
}

// SelectForInjection returns addressed notes after the target's durable
// cursor, including as many whole notes as fit the injection-byte limit,
// and the selected high-water note_id (DESIGN §5.4). The high-water is
// recorded with the run and the audience cursor advances in the same
// transaction as accepted prompt submission; a crash before that
// transaction may re-inject, a crash after it never loses. Selection is a
// prefix: the first note that does not fit stops the page so the cursor
// can never skip an un-injected note.
func (sel *Selector) SelectForInjection(ctx context.Context, goalID, project string, byteLimit int) ([]model.Note, int64, error) {
	cursor, err := sel.st.NoteCursor(ctx, goalID, project)
	if err != nil {
		return nil, 0, err
	}
	all, err := sel.st.NotesFor(ctx, goalID, project, cursor)
	if err != nil {
		return nil, 0, err
	}
	var selected []model.Note
	budget := byteLimit
	for _, note := range all {
		if len(note.Body) > budget {
			break
		}
		budget -= len(note.Body)
		selected = append(selected, note)
	}
	return selected, lastNoteID(selected, cursor), nil
}

// ErrCodeOf extracts the stable code of one notes error, if any.
func ErrCodeOf(err error) (ErrCode, bool) {
	var sendErr *SendError
	if errors.As(err, &sendErr) {
		return sendErr.Code, true
	}
	return "", false
}
