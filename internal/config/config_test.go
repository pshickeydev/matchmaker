package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pshickeydev/matchmaker/internal/model"
)

// writeConfig writes content to a temp TOML file and returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadFleetRoundTrip(t *testing.T) {
	dir := t.TempDir()
	api := filepath.Join(dir, "api")
	web := filepath.Join(dir, "web")
	for _, project := range []string{api, web} {
		if err := os.MkdirAll(project, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := writeConfig(t, `
[[projects]]
name = "api"
path = "`+api+`"
port = 41001
tags = ["lang:go", "team:platform"]

[[projects]]
name = "web"
path = "`+web+`"
port = 41002
tags = ["lang:ts"]

[[projects]]
name = "docs"
path = "`+web+`/docs"
port = 41003
note_opt_out = true

[projects.crush_options]
debug = true
data_dir = "`+filepath.Join(dir, "docsdata")+`"
`)
	fleet, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(fleet.Projects) != 3 {
		t.Fatalf("got %d projects, want 3", len(fleet.Projects))
	}
	got := fleet.Projects[0]
	if got.Name != "api" || got.Port != 41001 {
		t.Errorf("project 0 = %+v", got)
	}
	if got.Path != mustCanonical(t, api) {
		t.Errorf("api path = %q, want canonical %q", got.Path, mustCanonical(t, api))
	}
	if got.CrushOptions.DataDir != filepath.Join(mustCanonical(t, api), ".crush") {
		t.Errorf("api effective data_dir = %q, want default", got.CrushOptions.DataDir)
	}
	if got.CrushOptions.Debug != nil {
		t.Errorf("api debug unexpectedly set")
	}
	docs := fleet.Projects[2]
	if !docs.NoteOptOut {
		t.Error("docs note_opt_out lost")
	}
	if docs.CrushOptions.DataDir != mustCanonical(t, filepath.Join(dir, "docsdata")) {
		t.Errorf("docs data_dir = %q", docs.CrushOptions.DataDir)
	}
	if docs.CrushOptions.Debug == nil || !*docs.CrushOptions.Debug {
		t.Error("docs debug flag lost")
	}
	if err := fleet.Validate(); err != nil {
		t.Errorf("Validate on clean fleet: %v", err)
	}
}

func mustCanonical(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		resolved = path
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func TestLoadFleetStrictRejectsUnknownKeys(t *testing.T) {
	path := writeConfig(t, `
[[projects]]
name = "api"
path = "/tmp/api"
port = 41001
yolo = true
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "yolo") {
		t.Errorf("unknown key not rejected: %v", err)
	}
}

func TestLoadFleetRequiresNameAndPath(t *testing.T) {
	path := writeConfig(t, `
[[projects]]
name = ""
path = "/tmp/api"
port = 41001
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Errorf("missing name not rejected: %v", err)
	}
}

func TestValidateCollectsAllCollisions(t *testing.T) {
	dir := t.TempDir()
	fleet := &Fleet{Projects: []Project{
		{Name: "api", Path: filepath.Join(dir, "a"), Port: 41001, CrushOptions: CrushOptions{DataDir: filepath.Join(dir, "a", ".crush")}},
		{Name: "api", Path: filepath.Join(dir, "b"), Port: 41002, CrushOptions: CrushOptions{DataDir: filepath.Join(dir, "b", ".crush")}},
		{Name: "web", Path: filepath.Join(dir, "b"), Port: 41002, CrushOptions: CrushOptions{DataDir: filepath.Join(dir, "b", ".crush")}},
		{Name: "docs", Path: filepath.Join(dir, "d"), Port: 70000, CrushOptions: CrushOptions{DataDir: filepath.Join(dir, "b", ".crush")}, Tags: []string{"bad tag!"}},
	}}
	err := fleet.Validate()
	if err == nil {
		t.Fatal("collisions not reported")
	}
	for _, want := range []string{
		"duplicate project name api",
		"duplicate port 41002",
		"duplicate canonical path",
		"duplicate effective data_dir",
		"port 70000 out of range",
		`invalid tag "bad tag!"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestProjectLookup(t *testing.T) {
	fleet := &Fleet{Projects: []Project{{Name: "api"}}}
	if _, err := fleet.Project("api"); err != nil {
		t.Errorf("existing project: %v", err)
	}
	if _, err := fleet.Project("nope"); err == nil {
		t.Error("absent project returned")
	}
}

func snapshotFor(t *testing.T) *FleetSnapshot {
	t.Helper()
	return (&Fleet{Projects: []Project{
		{Name: "api", Port: 41001, Tags: []string{"lang:go", "team:platform"}},
		{Name: "web", Port: 41002, Tags: []string{"lang:ts", "team:platform"}},
		{Name: "docs", Port: 41003, Tags: []string{"lang:md"}},
	}}).Snapshot()
}

func TestResolveTargetExplicit(t *testing.T) {
	s := snapshotFor(t)
	targets, err := s.ResolveTarget(model.TargetSpec{Explicit: []string{"api", "web"}})
	if err != nil {
		t.Fatalf("ResolveTarget: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("got %d targets, want 2", len(targets))
	}
	if targets[0].Project != "api" || targets[0].ServerURL != "http://127.0.0.1:41001" {
		t.Errorf("target 0 = %+v", targets[0])
	}
	if _, err := s.ResolveTarget(model.TargetSpec{Explicit: []string{"api", "gone"}}); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Errorf("missing project not reported: %v", err)
	}
}

func TestResolveTargetTagConjunction(t *testing.T) {
	s := snapshotFor(t)
	targets, err := s.ResolveTarget(model.TargetSpec{TagExpr: "team:platform"})
	if err != nil {
		t.Fatalf("single tag: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("got %d targets, want 2", len(targets))
	}
	targets, err = s.ResolveTarget(model.TargetSpec{TagExpr: "team:platform, lang:go"})
	if err != nil {
		t.Fatalf("conjunction: %v", err)
	}
	if len(targets) != 1 || targets[0].Project != "api" {
		t.Fatalf("conjunction = %+v", targets)
	}
	if _, err := s.ResolveTarget(model.TargetSpec{TagExpr: "lang:rust"}); err == nil {
		t.Error("unresolvable tag expression accepted")
	}
	if _, err := s.ResolveTarget(model.TargetSpec{TagExpr: "team:platform, "}); err == nil {
		t.Error("empty tag member accepted")
	}
}

func TestResolveTargetAll(t *testing.T) {
	s := snapshotFor(t)
	targets, err := s.ResolveTarget(model.TargetSpec{All: true})
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if len(targets) != 3 {
		t.Fatalf("got %d targets, want 3", len(targets))
	}
	if targets[0].Project != "api" || targets[2].Project != "web" {
		t.Errorf("all targets not name-ordered: %+v", targets)
	}
}

func TestResolveTargetSpecShape(t *testing.T) {
	s := snapshotFor(t)
	if _, err := s.ResolveTarget(model.TargetSpec{}); err == nil {
		t.Error("empty target spec accepted")
	}
	spec := model.TargetSpec{Explicit: []string{"api"}, All: true}
	if _, err := s.ResolveTarget(spec); err == nil {
		t.Error("over-determined target spec accepted")
	}
}

func TestValidateTagExpr(t *testing.T) {
	valid := []string{"lang:go", "team:platform, lang:go", " lang:go ,lang:ts "}
	for _, expr := range valid {
		if err := ValidateTagExpr(expr); err != nil {
			t.Errorf("ValidateTagExpr(%q) = %v", expr, err)
		}
	}
	invalid := []string{"", "   ", "a,", ",", "lang:go,,ts", "bad tag", "tag;rm"}
	for _, expr := range invalid {
		if err := ValidateTagExpr(expr); err == nil {
			t.Errorf("ValidateTagExpr(%q) accepted", expr)
		}
	}
}

func TestSnapshotIsFrozenCopy(t *testing.T) {
	fleet := &Fleet{Projects: []Project{{Name: "api", Tags: []string{"t1"}}}}
	snap := fleet.Snapshot()
	fleet.Projects[0].Tags[0] = "mutated"
	if snap.Projects[0].Tags[0] != "t1" {
		t.Error("snapshot shares mutable tags with fleet")
	}
}

func TestInheritDataDir(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	userDir := filepath.Join(os.Getenv("XDG_DATA_HOME"), "crush")
	path := writeConfig(t, `
[[projects]]
name = "a"
path = "`+t.TempDir()+`"
port = 41001
crush_options = { inherit_data_dir = true }

[[projects]]
name = "b"
path = "`+t.TempDir()+`"
port = 41002
crush_options = { inherit_data_dir = true }
`)
	fleet, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, project := range fleet.Projects {
		if project.CrushOptions.DataDir != userDir {
			t.Errorf("%s data dir = %q, want user dir %q", project.Name, project.CrushOptions.DataDir, userDir)
		}
		if !project.CrushOptions.InheritDataDir {
			t.Errorf("%s inherit flag lost", project.Name)
		}
	}
	// Inheriting projects share the user's data dir by design: the
	// uniqueness check exempts them.
	if err := fleet.Validate(); err != nil {
		t.Errorf("inheriting fleet invalid: %v", err)
	}
}

func TestInheritDataDirExclusivity(t *testing.T) {
	path := writeConfig(t, `
[[projects]]
name = "a"
path = "`+t.TempDir()+`"
port = 41001
crush_options = { data_dir = "/var/lib/a", inherit_data_dir = true }
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("dual data_dir declaration = %v, want mutual-exclusion error", err)
	}
}

func TestLoadMatchmakerDefaults(t *testing.T) {
	path := writeConfig(t, `
state_dir = "`+t.TempDir()+`"
`)
	mm, err := LoadMatchmaker(path)
	if err != nil {
		t.Fatalf("LoadMatchmaker: %v", err)
	}
	if mm.Reconcile.TickInterval != 30*time.Second {
		t.Errorf("tick default = %s", mm.Reconcile.TickInterval)
	}
	if mm.Reconcile.MaxRestartsPerWindow != 5 {
		t.Errorf("restarts default = %d", mm.Reconcile.MaxRestartsPerWindow)
	}
	if mm.GoalLimits.MaxSteps != 50 || mm.GoalLimits.MaxPromptBytes != 64*1024 {
		t.Errorf("goal limits defaults = %+v", mm.GoalLimits)
	}
	if mm.GoalLimits.MinTimeout != 5*time.Second || mm.GoalLimits.MaxTimeout != time.Hour {
		t.Errorf("timeout bounds defaults = %+v", mm.GoalLimits)
	}
	if mm.NoteLimits.MaxNoteBodyBytes != 4096 || mm.NoteLimits.MaxReadPageSize != 50 {
		t.Errorf("note limits defaults = %+v", mm.NoteLimits)
	}
	if mm.Planner.MaxRevisions != 3 {
		t.Errorf("planner defaults = %+v", mm.Planner)
	}
	if mm.Supervision.CancelGrace != time.Minute {
		t.Errorf("cancel grace default = %s", mm.Supervision.CancelGrace)
	}
	if mm.Fleet.ApprovedVersion != PinnedCrushVersion || mm.Fleet.ApprovedBuildID != "" {
		t.Errorf("pin defaults = %+v", mm.Fleet)
	}
	if mm.CrushBinary != "crush" {
		t.Errorf("crush binary default = %q", mm.CrushBinary)
	}
	if mm.CoordinationAddr != "127.0.0.1:4763" {
		t.Errorf("coordination addr default = %q", mm.CoordinationAddr)
	}
}

func TestLoadMatchmakerOverridesAndErrors(t *testing.T) {
	path := writeConfig(t, `
state_dir = "`+t.TempDir()+`"
crush_binary = "/usr/local/bin/crush"
coordination_addr = "127.0.0.1:5151"

[reconcile]
tick_interval = "45s"

[goal_limits]
max_steps = 10
min_timeout = "10s"
max_timeout = "5m"
`)
	mm, err := LoadMatchmaker(path)
	if err != nil {
		t.Fatalf("LoadMatchmaker: %v", err)
	}
	if mm.Reconcile.TickInterval != 45*time.Second {
		t.Errorf("tick override = %s", mm.Reconcile.TickInterval)
	}
	if mm.CrushBinary != "/usr/local/bin/crush" || mm.CoordinationAddr != "127.0.0.1:5151" {
		t.Errorf("scalar overrides = %q %q", mm.CrushBinary, mm.CoordinationAddr)
	}
	if mm.GoalLimits.MaxSteps != 10 {
		t.Errorf("max_steps override = %d", mm.GoalLimits.MaxSteps)
	}
}

func TestLoadMatchmakerPassEnv(t *testing.T) {
	path := writeConfig(t, `
state_dir = "`+t.TempDir()+`"
pass_env = ["DIGITALOCEAN_API_KEY", "ANTHROPIC_API_KEY"]
`)
	mm, err := LoadMatchmaker(path)
	if err != nil {
		t.Fatalf("LoadMatchmaker: %v", err)
	}
	want := []string{"DIGITALOCEAN_API_KEY", "ANTHROPIC_API_KEY"}
	if len(mm.PassEnv) != 2 || mm.PassEnv[0] != want[0] || mm.PassEnv[1] != want[1] {
		t.Errorf("pass_env = %v, want %v", mm.PassEnv, want)
	}
}

func TestLoadMatchmakerRejectsBadValues(t *testing.T) {
	tests := []struct {
		name string
		toml string
		want string
	}{
		{name: "unknown key", toml: "state_dir = \"/tmp/x\"\nviper = true\n", want: "viper"},
		{name: "bad pass_env name", toml: "state_dir = \"/tmp/x\"\npass_env = [\"1BAD-NAME\"]\n", want: "pass_env"},
		{name: "bad duration", toml: "state_dir = \"/tmp/x\"\n[reconcile]\ntick_interval = \"fast\"\n", want: "tick_interval"},
		{name: "inverted timeout bounds", toml: "state_dir = \"/tmp/x\"\n[goal_limits]\nmin_timeout = \"10m\"\nmax_timeout = \"1m\"\n", want: "min_timeout"},
		{name: "negative count", toml: "state_dir = \"/tmp/x\"\n[goal_limits]\nmax_steps = -5\n", want: "max_steps"},
		{name: "relative state dir", toml: "state_dir = \"relative/dir\"\n", want: "not absolute"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadMatchmaker(writeConfig(t, tt.toml))
			if err == nil {
				t.Fatalf("bad config accepted")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q missing %q", err, tt.want)
			}
		})
	}
}

func TestErrorsJoinAcrossChecks(t *testing.T) {
	combined := errors.Join(errors.New("a"), errors.New("b"))
	if combined == nil || !strings.Contains(combined.Error(), "a") {
		t.Fatal("errors.Join sanity")
	}
}
