package goalvalidate

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pshickeydev/matchmaker/internal/config"
	"github.com/pshickeydev/matchmaker/internal/model"
)

func TestExampleGoalParsesAndValidates(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "note-passing-goal.json"))
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	draft, err := ParseSubmission(data)
	if err != nil {
		t.Fatalf("ParseSubmission: %v", err)
	}
	if len(draft.Steps) != 2 {
		t.Fatalf("example steps = %d", len(draft.Steps))
	}
	snapshot := (&config.Fleet{Projects: []config.Project{
		{Name: "api", Port: 41001, Tags: []string{"lang:go"}},
		{Name: "web", Port: 41002, Tags: []string{"lang:ts"}},
	}}).Snapshot()
	limits := config.GoalLimits{
		MaxSteps: 50, MaxDepsPerStep: 10, MaxPromptBytes: 8192,
		MaxTargetsPerStep: 10, MaxTotalRunsPerGoal: 200,
		MinTimeout: 5 * time.Second, MaxTimeout: time.Hour, MaxRetries: 3,
	}
	errs, frozen := ValidateDraft(draft, snapshot, limits)
	if len(errs) > 0 {
		for _, validationErr := range errs {
			t.Errorf("validation: %s", validationErr.Error())
		}
		return
	}
	if len(frozen) != 2 {
		t.Errorf("frozen expansions = %d, want one per step", len(frozen))
	}
	if frozen["web"][0].Project != "web" || frozen["api"][0].Project != "api" {
		t.Errorf("frozen targets = %+v", frozen)
	}
	if draft.Steps[1].Needs[0] != "api" || draft.Steps[0].Supervision != model.SupervisionDeny {
		t.Errorf("draft = %+v", draft.Steps)
	}
}
