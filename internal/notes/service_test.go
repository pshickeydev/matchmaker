package notes

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/pshickeydev/matchmaker/internal/config"
	"github.com/pshickeydev/matchmaker/internal/model"
	"github.com/pshickeydev/matchmaker/internal/store"
)

func testLimits() config.NoteLimits {
	return config.NoteLimits{
		MaxNoteBodyBytes:     32,
		MaxNotesPerGoal:      3,
		MaxInjectedNoteBytes: 20,
		MaxReadPageSize:      2,
		MaxResultChunkBytes:  1024,
		RateWindow:           time.Minute,
		MaxRequestsPerWindow: 5,
		MaxRetainedNotes:     100,
	}
}

// seedStore opens a store with one active work goal whose participants
// are api (lang:go, team:platform) and web (lang:ts, team:platform), plus
// one plan goal and one finalized goal.
func seedStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	goal := model.Goal{
		ID: "work", Type: model.GoalTypeWork, Status: model.GoalActive,
		Steps: []model.Step{{ID: "a", GoalID: "work", PromptTemplate: "p",
			Supervision: model.SupervisionDeny, Timeout: time.Minute}},
		FrozenTargets: map[string][]model.ResolvedTarget{
			"a": {
				{StepID: "a", Project: "api", Instance: "api", ServerURL: "http://127.0.0.1:1",
					Tags: []string{"lang:go", "team:platform"}},
				{StepID: "a", Project: "web", Instance: "web", ServerURL: "http://127.0.0.1:2",
					Tags: []string{"lang:ts", "team:platform"}},
			},
		},
	}
	if err := st.WithinTx(ctx, func(tx *store.Tx) error { return tx.CreateGoal(ctx, goal) }); err != nil {
		t.Fatalf("work goal: %v", err)
	}
	plan := goal
	plan.ID = "plan"
	plan.Type = model.GoalTypePlan
	for i := range plan.Steps {
		plan.Steps[i].GoalID = "plan"
	}
	planTargets := map[string][]model.ResolvedTarget{
		"a": {{StepID: "a", Project: "api", Instance: "api", ServerURL: "http://127.0.0.1:1", Tags: []string{"lang:go"}}},
	}
	plan.FrozenTargets = planTargets
	if err := st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.CreatePlanningGoal(ctx, plan, "plan the thing")
	}); err != nil {
		t.Fatalf("plan goal: %v", err)
	}
	done := goal
	done.ID = "done"
	for i := range done.Steps {
		done.Steps[i].GoalID = "done"
	}
	done.FrozenTargets = planTargets
	if err := st.WithinTx(ctx, func(tx *store.Tx) error { return tx.CreateGoal(ctx, done) }); err != nil {
		t.Fatalf("done goal: %v", err)
	}
	if err := st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.UpdateGoalStatus(ctx, "done", model.GoalSucceeded)
	}); err != nil {
		t.Fatal(err)
	}
	return st, "work"
}

func TestSendHappyPath(t *testing.T) {
	st, goalID := seedStore(t)
	service := New(st, testLimits())
	ctx := context.Background()

	first, err := service.Send(ctx, goalID, "api", model.Audience{Project: "web"}, "hi web")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	second, err := service.Send(ctx, goalID, "api", model.Audience{Tag: "team:platform"}, "hi team")
	if err != nil {
		t.Fatalf("Send tag: %v", err)
	}
	third, err := service.Send(ctx, goalID, "web", model.Audience{All: true}, "hi all")
	if err != nil {
		t.Fatalf("Send all: %v", err)
	}
	if second <= first || third <= second {
		t.Errorf("note ids not monotonic: %d %d %d", first, second, third)
	}
}

func TestSendRejections(t *testing.T) {
	st, goalID := seedStore(t)
	limits := testLimits()
	// The shared service outlives several subtests; only the dedicated
	// rate-limit subtest exercises the window, so keep it wide here.
	limits.MaxRequestsPerWindow = 50
	service := New(st, limits)
	ctx := context.Background()

	tests := []struct {
		name string
		run  func(t *testing.T) error
		want ErrCode
	}{
		{name: "unknown goal", run: func(*testing.T) error {
			_, err := service.Send(ctx, "ghost", "api", model.Audience{All: true}, "x")
			return err
		}, want: CodeUnknownGoal},
		{name: "completed goal", run: func(*testing.T) error {
			_, err := service.Send(ctx, "done", "api", model.Audience{All: true}, "x")
			return err
		}, want: CodeUnknownGoal},
		{name: "plan goal", run: func(*testing.T) error {
			_, err := service.Send(ctx, "plan", "api", model.Audience{All: true}, "x")
			return err
		}, want: CodePlanGoal},
		{name: "sender not participant", run: func(*testing.T) error {
			_, err := service.Send(ctx, goalID, "ghost", model.Audience{All: true}, "x")
			return err
		}, want: CodeNotParticipant},
		{name: "audience member not participant", run: func(*testing.T) error {
			_, err := service.Send(ctx, goalID, "api", model.Audience{Project: "ghost"}, "x")
			return err
		}, want: CodeNotParticipant},
		{name: "body too large", run: func(*testing.T) error {
			_, err := service.Send(ctx, goalID, "api", model.Audience{Project: "web"},
				string(make([]byte, limits.MaxNoteBodyBytes+1)))
			return err
		}, want: CodeNoteTooLarge},
		{name: "notes per goal exhausted", run: func(t *testing.T) error {
			for i := 0; i < limits.MaxNotesPerGoal; i++ {
				if _, err := service.Send(ctx, goalID, "api", model.Audience{Project: "web"}, "n"); err != nil {
					t.Fatalf("seed note %d: %v", i, err)
				}
			}
			_, err := service.Send(ctx, goalID, "api", model.Audience{Project: "web"}, "one too many")
			return err
		}, want: CodeGoalNoteLimit},
		{name: "rate limited", run: func(t *testing.T) error {
			tight := New(st, config.NoteLimits{
				MaxNoteBodyBytes: 32, MaxNotesPerGoal: 100, MaxReadPageSize: 10,
				RateWindow: time.Minute, MaxRequestsPerWindow: 1,
			})
			if _, err := tight.Send(ctx, goalID, "api", model.Audience{Project: "web"}, "a"); err != nil {
				t.Fatalf("first: %v", err)
			}
			_, err := tight.Send(ctx, goalID, "api", model.Audience{Project: "web"}, "b")
			return err
		}, want: CodeRateLimited},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run(t)
			code, ok := ErrCodeOf(err)
			if !ok {
				t.Fatalf("error %v is not a stable notes error", err)
			}
			if code != tt.want {
				t.Errorf("code = %s, want %s", code, tt.want)
			}
		})
	}
}

func TestReadPagingAndCursor(t *testing.T) {
	st, goalID := seedStore(t)
	limits := testLimits()
	limits.MaxNotesPerGoal = 10
	service := New(st, limits)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		if _, err := service.Send(ctx, goalID, "api", model.Audience{All: true}, "note"); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}

	// Page size may only lower the maxima; larger requests clamp.
	page, next, err := service.Read(ctx, goalID, "web", 0, 100)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(page) != limits.MaxReadPageSize {
		t.Fatalf("page = %d notes, want clamped %d", len(page), limits.MaxReadPageSize)
	}
	if next != page[len(page)-1].NoteID {
		t.Errorf("next cursor = %d, want last note id", next)
	}
	page2, next2, err := service.Read(ctx, goalID, "web", next, 0)
	if err != nil {
		t.Fatalf("Read page 2: %v", err)
	}
	if len(page2) != 2 || next2 != page2[len(page2)-1].NoteID {
		t.Errorf("page 2 = %+v next=%d", page2, next2)
	}
	page3, next3, err := service.Read(ctx, goalID, "web", next2, 0)
	if err != nil {
		t.Fatalf("Read page 3: %v", err)
	}
	if len(page3) != 0 || next3 != next2 {
		t.Errorf("empty page 3 = %+v next=%d want next=%d", page3, next3, next2)
	}
	// Reads never advance the durable delivery cursor.
	cursor, err := st.NoteCursor(ctx, goalID, "web")
	if err != nil || cursor != 0 {
		t.Errorf("durable cursor = %d err=%v after reads", cursor, err)
	}
	if _, _, err := service.Read(ctx, goalID, "web", -1, 1); err == nil {
		t.Error("negative cursor accepted")
	} else if code, _ := ErrCodeOf(err); code != CodeInvalidCursor {
		t.Errorf("negative cursor = %v, want invalid_cursor", err)
	}
}

func TestSelectForInjectionPrefixBudget(t *testing.T) {
	st, goalID := seedStore(t)
	limits := testLimits()
	limits.MaxNotesPerGoal = 10
	limits.MaxNoteBodyBytes = 64
	service := New(st, limits)
	ctx := context.Background()

	// Three notes for web: small, too-big-for-budget, small.
	if _, err := service.Send(ctx, goalID, "api", model.Audience{Project: "web"}, "tiny"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(ctx, goalID, "api", model.Audience{Project: "web"}, "this body is deliberately larger than the whole budget"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(ctx, goalID, "api", model.Audience{Project: "web"}, "also tiny"); err != nil {
		t.Fatal(err)
	}

	selector := NewSelector(st)
	selected, highWater, err := selector.SelectForInjection(ctx, goalID, "web", limits.MaxInjectedNoteBytes)
	if err != nil {
		t.Fatalf("SelectForInjection: %v", err)
	}
	// Prefix semantics: the oversized second note stops the page; the
	// later small note is not selected, so the cursor never skips it.
	if len(selected) != 1 || selected[0].Body != "tiny" {
		t.Fatalf("selected = %+v", selected)
	}
	notes, _ := st.NotesFor(ctx, goalID, "web", 0)
	if highWater != notes[0].NoteID {
		t.Errorf("high water = %d, want first note id %d", highWater, notes[0].NoteID)
	}

	// After the submission transaction advances the cursor past the first
	// note, the next selection starts at the oversized note and selects
	// nothing within the budget.
	if err := st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.AdvanceNoteCursor(ctx, goalID, "web", highWater)
	}); err != nil {
		t.Fatal(err)
	}
	selected2, highWater2, err := selector.SelectForInjection(ctx, goalID, "web", limits.MaxInjectedNoteBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected2) != 0 || highWater2 != highWater {
		t.Errorf("second selection = %+v high water %d, want empty and unchanged", selected2, highWater2)
	}
}
