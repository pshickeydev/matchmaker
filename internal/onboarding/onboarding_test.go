package onboarding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pshickeydev/matchmaker/internal/config"
	"github.com/pshickeydev/matchmaker/internal/fleet"
	"github.com/pshickeydev/matchmaker/internal/store"
)

const testURL = "http://127.0.0.1:4763/mcp"

func TestGenerateFragmentIsShellSafe(t *testing.T) {
	fragment := GenerateFragment(testURL)
	want := "mcp add matchmaker --type http --url 'http://127.0.0.1:4763/mcp'\n" +
		"permissions allow mcp_matchmaker_note_send mcp_matchmaker_note_read mcp_matchmaker_result_read"
	if fragment != want {
		t.Errorf("fragment = %q", fragment)
	}
	// A URL containing a single quote is still literal-quoted safely:
	// the embedded quote is closed, escaped, and reopened per POSIX.
	quoted := GenerateFragment("http://127.0.0.1:1/x'y")
	if !strings.HasPrefix(quoted, "mcp add matchmaker --type http --url '") ||
		!strings.Contains(quoted, "x'\\''y") {
		t.Errorf("unsafe quoting: %q", quoted)
	}
}

func TestInstallWithheldApprovalWritesNothing(t *testing.T) {
	dir := t.TempDir()
	result, err := Install(dir, GenerateFragment(testURL), ApprovalWithheld)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if result.WroteNewFile || result.AppendedToExisting {
		t.Errorf("withheld approval wrote: %+v", result)
	}
	if result.Fragment == "" {
		t.Error("withheld approval did not return the fragment for manual installation")
	}
	if _, err := os.Stat(filepath.Join(dir, ".crushrc")); !os.IsNotExist(err) {
		t.Error("config written despite withheld approval")
	}
}

func TestInstallCreatesNewFile(t *testing.T) {
	dir := t.TempDir()
	result, err := Install(dir, GenerateFragment(testURL), ApprovalGranted)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !result.WroteNewFile || result.AppendedToExisting {
		t.Errorf("result = %+v", result)
	}
	content, err := os.ReadFile(filepath.Join(dir, ".crushrc"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), blockBegin) ||
		!strings.Contains(string(content), "mcp add matchmaker --type http --url 'http://127.0.0.1:4763/mcp'") ||
		!strings.Contains(string(content), blockEnd) {
		t.Errorf("new config = %q", content)
	}
	info, err := os.Stat(filepath.Join(dir, ".crushrc"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("new config perms = %v", info)
	}
	if err := VerifyParse("/bin/true", dir); err != nil {
		t.Errorf("VerifyParse on fresh config: %v", err)
	}
}

func TestInstallAppendsWithBackup(t *testing.T) {
	dir := t.TempDir()
	existing := "export MY_SETTING=1\n"
	if err := os.WriteFile(filepath.Join(dir, ".crushrc"), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := Install(dir, GenerateFragment(testURL), ApprovalGranted)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !result.AppendedToExisting || result.WroteNewFile {
		t.Errorf("result = %+v", result)
	}
	content, err := os.ReadFile(filepath.Join(dir, ".crushrc"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(content), existing) {
		t.Errorf("existing statements rewritten: %q", content)
	}
	if !strings.Contains(string(content), blockBegin) {
		t.Errorf("owned block missing: %q", content)
	}
	backup, err := os.ReadFile(filepath.Join(dir, ".crushrc"+backupSuffix))
	if err != nil || string(backup) != existing {
		t.Errorf("backup = %q err=%v", backup, err)
	}
}

func TestInstallReplacesOwnedBlockIdempotently(t *testing.T) {
	dir := t.TempDir()
	existing := "export MY_SETTING=1\n"
	if _, err := Install(dir, GenerateFragment(testURL), ApprovalGranted); err != nil {
		t.Fatal(err)
	}
	// An operator adds a statement after the owned block.
	path := filepath.Join(dir, ".crushrc")
	if err := os.WriteFile(path, append([]byte(existing), read(t, path)...), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Install(dir, GenerateFragment("http://127.0.0.1:9999/mcp"), ApprovalGranted)
	if err != nil {
		t.Fatalf("re-Install: %v", err)
	}
	if !result.AppendedToExisting {
		t.Errorf("result = %+v", result)
	}
	content := read(t, path)
	if strings.Count(string(content), blockBegin) != 1 {
		t.Errorf("owned block duplicated: %q", content)
	}
	if !strings.Contains(string(content), "http://127.0.0.1:9999/mcp") {
		t.Errorf("owned block not updated: %q", content)
	}
	if !strings.Contains(string(content), existing) {
		t.Errorf("operator statement lost: %q", content)
	}
}

func TestInstallRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real-crushrc")
	if err := os.WriteFile(target, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, ".crushrc")); err != nil {
		t.Fatal(err)
	}
	result, err := Install(dir, GenerateFragment(testURL), ApprovalGranted)
	if err == nil {
		t.Fatal("symlink install accepted")
	}
	if result.WroteNewFile || result.AppendedToExisting {
		t.Errorf("symlink install wrote: %+v", result)
	}
	if result.Fragment == "" {
		t.Error("fail-closed install did not return the fragment")
	}
}

func TestVerifyParseRejectsBrokenSyntax(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".crushrc"), []byte("if [ true\nthen\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyParse("/bin/true", dir); err == nil {
		t.Fatal("broken shell syntax accepted")
	}
	if err := VerifyParse("/nonexistent/crush", dir); err == nil {
		t.Fatal("missing crush binary accepted")
	}
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func TestCommitWithoutRegistrationFailsClosed(t *testing.T) {
	if err := CommitWithFingerprint("api"); err == nil {
		t.Fatal("commit without registered context succeeded")
	}
}

func TestCommitWithFingerprint(t *testing.T) {
	dir := t.TempDir()
	projectPath := filepath.Join(dir, "api")
	if err := os.MkdirAll(projectPath, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.Context(), filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	project := config.Project{
		Name: "api", Path: projectPath, Port: 41001,
		CrushOptions: config.CrushOptions{DataDir: filepath.Join(projectPath, ".crush")},
	}
	fleetConfig := &config.Fleet{Projects: []config.Project{project}}
	mm := &config.Matchmaker{CrushBinary: "/bin/true"}
	Register(st, fleetConfig, mm)

	if err := CommitWithFingerprint("ghost"); err == nil {
		t.Fatal("unknown project committed")
	}
	if err := CommitWithFingerprint("api"); err != nil {
		t.Fatalf("CommitWithFingerprint: %v", err)
	}
	approved, found, err := st.ApprovedFingerprint(t.Context(), "api")
	if err != nil || !found {
		t.Fatalf("approved fingerprint = %+v found=%v err=%v", approved, found, err)
	}
	current, err := fleet.ComputeFingerprint(project, mm)
	if err != nil {
		t.Fatal(err)
	}
	if len(approved.Entries) != len(current.Entries) {
		t.Errorf("approved fingerprint entries = %d, want %d", len(approved.Entries), len(current.Entries))
	}
	if approved.ApprovedBy != "operator-onboarding" {
		t.Errorf("approved_by = %q", approved.ApprovedBy)
	}
}
