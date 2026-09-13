package goalvalidate

import (
	"strings"
	"testing"
	"time"

	"github.com/pshickeydev/matchmaker/internal/config"
	"github.com/pshickeydev/matchmaker/internal/model"
)

func testLimits() config.GoalLimits {
	return config.GoalLimits{
		MaxSteps:            5,
		MaxDepsPerStep:      2,
		MaxPromptBytes:      1024,
		MaxTargetsPerStep:   2,
		MaxTotalRunsPerGoal: 4,
		MinTimeout:          5 * time.Second,
		MaxTimeout:          time.Hour,
		MaxRetries:          3,
	}
}

func testSnapshot() *config.FleetSnapshot {
	return (&config.Fleet{Projects: []config.Project{
		{Name: "api", Port: 41001, Tags: []string{"lang:go", "team:platform"}},
		{Name: "web", Port: 41002, Tags: []string{"lang:ts", "team:platform"}},
		{Name: "docs", Port: 41003, Tags: []string{"lang:md"}},
	}}).Snapshot()
}

func step(id, prompt string, target model.TargetSpec) model.Step {
	return model.Step{ID: id, PromptTemplate: prompt, Target: target,
		Supervision: model.SupervisionDeny, Timeout: 10 * time.Minute}
}

func explicit(names ...string) model.TargetSpec {
	return model.TargetSpec{Explicit: names}
}

// errText concatenates all validation errors for substring checks.
func errText(errs []ValidationError) string {
	var parts []string
	for _, err := range errs {
		parts = append(parts, err.Error())
	}
	return strings.Join(parts, "\n")
}

func TestParseSubmissionHappyPath(t *testing.T) {
	data := `{
		"objective": "release the api",
		"steps": [
			{
				"id": "build",
				"prompt": "build {{.Objective}}",
				"target": {"explicit": ["api"]},
				"supervision": "grant_all",
				"timeout": "15m",
				"retries": 1
			},
			{
				"id": "review",
				"prompt": "review upstream {{range .Upstream}}{{.StepID}} {{end}}",
				"target": {"tag": "team:platform"},
				"needs": ["build"],
				"accept_partial_needs": true
			}
		]
	}`
	draft, err := ParseSubmission([]byte(data))
	if err != nil {
		t.Fatalf("ParseSubmission: %v", err)
	}
	if draft.Objective != "release the api" || len(draft.Steps) != 2 {
		t.Fatalf("draft = %+v", draft)
	}
	if draft.Steps[0].Timeout != 15*time.Minute || draft.Steps[0].Retries != 1 {
		t.Errorf("step 0 = %+v", draft.Steps[0])
	}
	if draft.Steps[1].Timeout != DefaultStepTimeout {
		t.Errorf("default timeout not applied: %s", draft.Steps[1].Timeout)
	}
	if draft.Steps[1].Supervision != model.SupervisionDeny {
		t.Errorf("default supervision not applied: %q", draft.Steps[1].Supervision)
	}
	if !draft.Steps[1].AcceptPartialNeeds {
		t.Error("accept_partial_needs lost")
	}
}

func TestParseSubmissionRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "session reuse field", data: `{"objective":"o","steps":[{"id":"a","prompt":"p","target":{"all":true},"session_id":"s1"}]}`, want: "unknown field"},
		{name: "pinned session field", data: `{"objective":"o","steps":[{"id":"a","prompt":"p","target":{"all":true},"session":"pinned"}]}`, want: "unknown field"},
		{name: "unknown top key", data: `{"objective":"o","steps":[{"id":"a","prompt":"p","target":{"all":true}}],"viper":true}`, want: "unknown field"},
		{name: "missing objective", data: `{"steps":[{"id":"a","prompt":"p","target":{"all":true}}]}`, want: "objective"},
		{name: "no steps", data: `{"objective":"o","steps":[]}`, want: "at least one step"},
		{name: "bad json", data: `{`, want: "schema"},
		{name: "bad duration", data: `{"objective":"o","steps":[{"id":"a","prompt":"p","target":{"all":true},"timeout":"soon"}]}`, want: "timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseSubmission([]byte(tt.data))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("ParseSubmission error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestValidateDraftHappyPath(t *testing.T) {
	draft := GoalDraft{
		Objective: "release",
		Steps: []model.Step{
			step("build", "build {{.Objective}}", explicit("api")),
			{ID: "review", PromptTemplate: "review {{range .Upstream}}{{.Project}}{{end}}",
				Target: model.TargetSpec{TagExpr: "team:platform"}, Needs: []string{"build"},
				Supervision: model.SupervisionGrantAll, Timeout: 10 * time.Minute},
		},
	}
	errs, frozen := ValidateDraft(draft, testSnapshot(), testLimits())
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %s", errText(errs))
	}
	if len(frozen["build"]) != 1 || frozen["build"][0].Project != "api" || frozen["build"][0].StepID != "build" {
		t.Errorf("frozen build = %+v", frozen["build"])
	}
	if len(frozen["review"]) != 2 {
		t.Errorf("frozen review = %+v", frozen["review"])
	}
}

func TestValidateDAGFailures(t *testing.T) {
	tests := []struct {
		name  string
		steps []model.Step
		want  string
	}{
		{name: "empty id", steps: []model.Step{step("", "p", explicit("api"))}, want: "step id is empty"},
		{name: "duplicate id", steps: []model.Step{
			step("a", "p", explicit("api")), step("a", "p", explicit("api")),
		}, want: "duplicate step id"},
		{name: "unknown needs", steps: []model.Step{
			{ID: "a", PromptTemplate: "p", Target: explicit("api"), Needs: []string{"ghost"}, Supervision: model.SupervisionDeny, Timeout: time.Minute},
		}, want: `unknown step "ghost"`},
		{name: "self dependency", steps: []model.Step{
			{ID: "a", PromptTemplate: "p", Target: explicit("api"), Needs: []string{"a"}, Supervision: model.SupervisionDeny, Timeout: time.Minute},
		}, want: "depends on itself"},
		{name: "two step cycle", steps: []model.Step{
			{ID: "a", PromptTemplate: "p", Target: explicit("api"), Needs: []string{"b"}, Supervision: model.SupervisionDeny, Timeout: time.Minute},
			{ID: "b", PromptTemplate: "p", Target: explicit("api"), Needs: []string{"a"}, Supervision: model.SupervisionDeny, Timeout: time.Minute},
		}, want: "dependency cycle"},
		{name: "three step cycle", steps: []model.Step{
			{ID: "a", PromptTemplate: "p", Target: explicit("api"), Needs: []string{"c"}, Supervision: model.SupervisionDeny, Timeout: time.Minute},
			{ID: "b", PromptTemplate: "p", Target: explicit("api"), Needs: []string{"a"}, Supervision: model.SupervisionDeny, Timeout: time.Minute},
			{ID: "c", PromptTemplate: "p", Target: explicit("api"), Needs: []string{"b"}, Supervision: model.SupervisionDeny, Timeout: time.Minute},
		}, want: "dependency cycle"},
		{name: "duplicate dependency", steps: []model.Step{
			step("a", "p", explicit("api")),
			{ID: "b", PromptTemplate: "p", Target: explicit("api"), Needs: []string{"a", "a"}, Supervision: model.SupervisionDeny, Timeout: time.Minute},
		}, want: `duplicate dependency "a"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := ValidateDAG(tt.steps)
			if !strings.Contains(errText(errs), tt.want) {
				t.Errorf("ValidateDAG = %q, want %q", errText(errs), tt.want)
			}
		})
	}
}

func TestValidateTemplatesFailures(t *testing.T) {
	tests := []struct {
		name   string
		prompt string
		want   string
	}{
		{name: "unknown function", prompt: `{{ evilfunc .Objective }}`, want: "template parse"},
		{name: "missing field", prompt: `{{ .Nonexistent }}`, want: "template render"},
		{name: "bad syntax", prompt: `{{ .Objective `, want: "template parse"},
		{name: "upstream fields available", prompt: `{{ range .Upstream }}{{ .Excerpt }}{{ end }}`, want: ""},
		{name: "notes fields available", prompt: `{{ range .Notes }}{{ .From }}: {{ .Body }}{{ end }}`, want: ""},
		{name: "printf builtin allowed", prompt: `{{ printf "%s" .Objective }}`, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := ValidateTemplates([]model.Step{step("a", tt.prompt, explicit("api"))})
			if tt.want == "" {
				if len(errs) != 0 {
					t.Errorf("valid prompt rejected: %s", errText(errs))
				}
				return
			}
			if !strings.Contains(errText(errs), tt.want) {
				t.Errorf("ValidateTemplates = %q, want %q", errText(errs), tt.want)
			}
		})
	}
}

func TestExpandTargetsFailures(t *testing.T) {
	tests := []struct {
		name   string
		target model.TargetSpec
		want   string
	}{
		{name: "unknown project", target: explicit("api", "ghost"), want: "unknown project"},
		{name: "empty target", target: model.TargetSpec{}, want: "exactly one"},
		{name: "overdetermined target", target: model.TargetSpec{Explicit: []string{"api"}, All: true}, want: "exactly one"},
		{name: "bad tag expression", target: model.TargetSpec{TagExpr: "bad tag"}, want: "invalid tag"},
		{name: "unresolvable tag", target: model.TargetSpec{TagExpr: "lang:rust"}, want: "resolves to no projects"},
		{name: "per step fan-out limit", target: model.TargetSpec{All: true}, want: "exceed max_targets_per_step"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limits := testLimits()
			if tt.name == "per step fan-out limit" {
				limits.MaxTargetsPerStep = 2
			}
			_, errs := ExpandTargets([]model.Step{step("a", "p", tt.target)}, testSnapshot(), limits)
			if !strings.Contains(errText(errs), tt.want) {
				t.Errorf("ExpandTargets = %q, want %q", errText(errs), tt.want)
			}
		})
	}
}

func TestTotalRunLimitConsumedByFanOut(t *testing.T) {
	limits := testLimits()
	limits.MaxTotalRunsPerGoal = 2
	draft := GoalDraft{Objective: "o", Steps: []model.Step{
		step("a", "p", model.TargetSpec{TagExpr: "team:platform"}),
		step("b", "p", model.TargetSpec{TagExpr: "team:platform"}),
	}}
	errs, frozen := ValidateDraft(draft, testSnapshot(), limits)
	if !strings.Contains(errText(errs), "max_total_runs_per_goal") {
		t.Errorf("total run limit not enforced: %q", errText(errs))
	}
	if frozen != nil {
		t.Error("frozen expansion returned despite errors")
	}
}

func TestValidatePoliciesFailures(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*model.Step)
		want string
	}{
		{name: "timeout below minimum", mut: func(s *model.Step) { s.Timeout = time.Second }, want: "outside"},
		{name: "timeout above maximum", mut: func(s *model.Step) { s.Timeout = 2 * time.Hour }, want: "outside"},
		{name: "retries above max", mut: func(s *model.Step) { s.Retries = 9 }, want: "exceeds max_retries"},
		{name: "reserved ask policy", mut: func(s *model.Step) { s.Supervision = model.SupervisionAsk }, want: "reserved"},
		{name: "unknown policy", mut: func(s *model.Step) { s.Supervision = "yolo" }, want: "unrecognized supervision"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := step("a", "p", explicit("api"))
			tt.mut(&base)
			errs := ValidatePolicies([]model.Step{base}, testLimits())
			if !strings.Contains(errText(errs), tt.want) {
				t.Errorf("ValidatePolicies = %q, want %q", errText(errs), tt.want)
			}
		})
	}
}

func TestGoalWideLimits(t *testing.T) {
	limits := testLimits()
	draft := GoalDraft{Objective: "o", Steps: []model.Step{
		step("a", "p", explicit("api")),
		step("b", "p", explicit("api")),
		step("c", "p", explicit("api")),
		step("d", "p", explicit("api")),
		step("e", "p", explicit("api")),
		step("f", "p", explicit("api")),
	}}
	errs, _ := ValidateDraft(draft, testSnapshot(), limits)
	if !strings.Contains(errText(errs), "max_steps") {
		t.Errorf("max_steps not enforced: %q", errText(errs))
	}

	limits = testLimits()
	deep := step("a", strings.Repeat("x", limits.MaxPromptBytes+1), explicit("api"))
	errs, _ = ValidateDraft(GoalDraft{Objective: "o", Steps: []model.Step{deep}}, testSnapshot(), limits)
	if !strings.Contains(errText(errs), "max_prompt_bytes") {
		t.Errorf("max_prompt_bytes not enforced: %q", errText(errs))
	}

	limits = testLimits()
	manyDeps := step("a", "p", explicit("api"))
	manyDeps.Needs = []string{"x", "y", "z"}
	errs, _ = ValidateDraft(GoalDraft{Objective: "o", Steps: []model.Step{
		manyDeps, step("x", "p", explicit("api")), step("y", "p", explicit("api")), step("z", "p", explicit("api")),
	}}, testSnapshot(), limits)
	if !strings.Contains(errText(errs), "max_deps_per_step") {
		t.Errorf("max_deps_per_step not enforced: %q", errText(errs))
	}
}

func TestValidateDraftReturnsAllErrors(t *testing.T) {
	draft := GoalDraft{Objective: "o", Steps: []model.Step{
		{ID: "a", PromptTemplate: "{{ bad }}", Target: explicit("ghost"), Supervision: "yolo", Timeout: time.Hour * 2},
	}}
	errs, frozen := ValidateDraft(draft, testSnapshot(), testLimits())
	joined := errText(errs)
	for _, want := range []string{"template parse", "unknown project", "unrecognized supervision", "outside"} {
		if !strings.Contains(joined, want) {
			t.Errorf("errors %q missing %q", joined, want)
		}
	}
	if frozen != nil {
		t.Error("frozen expansion returned despite errors")
	}
}

func TestAllowedTemplateFuncsCoversBuiltins(t *testing.T) {
	funcs := AllowedTemplateFuncs()
	for _, want := range []string{"printf", "len", "eq", "index", "and", "or", "not"} {
		found := false
		for _, name := range funcs {
			if name == want {
				found = true
			}
		}
		if !found {
			t.Errorf("AllowedTemplateFuncs missing %q", want)
		}
	}
}
