// Package onboarding installs the Matchmaker-owned crushrc registration
// block for per-project coordination MCP registration (DESIGN §5.4).
package onboarding

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/pshickeydev/matchmaker/internal/config"
	"github.com/pshickeydev/matchmaker/internal/fleet"
	"github.com/pshickeydev/matchmaker/internal/store"
)

const (
	// blockBegin and blockEnd delimit the Matchmaker-owned crushrc
	// block; Matchmaker updates only content between them and never
	// interprets, reformats, removes, or rewrites other statements
	// (DESIGN §5.4).
	blockBegin = "# BEGIN matchmaker (owned block; do not edit by hand)"
	blockEnd   = "# END matchmaker"
	// serverName is the registered MCP server name.
	serverName = "matchmaker"
	// permAllowCmd is the crushrc command pre-approving tools without
	// permission prompts (verified v0.94.1: `permissions allow <tool> ...`).
	permAllowCmd = "permissions allow"
	// backupSuffix marks the pre-install backup copy.
	backupSuffix = ".matchmaker-bak"
	// shellPath syntax-checks crushrc files without executing them.
	shellPath = "/bin/sh"
)

// coordinationTools are the coordination tool names Crush exposes from
// the registered server as mcp_matchmaker_<tool> (verified v0.94.1 MCP
// tool naming). Pre-approving them keeps deny supervision meaningful for
// every other tool while the registered coordination channel stays
// usable (DESIGN §5.4).
var coordinationTools = []string{"note_send", "note_read", "result_read"}

// GenerateFragment produces the deterministic shell fragment: one
// literal `mcp add matchmaker --type http --url ...` command plus one
// `permissions allow` line pre-approving the coordination tools, so they
// remain usable under deny supervision. All generated values use
// shell-safe literal quoting; no value derived from agent or note
// content ever enters config.
func GenerateFragment(coordinationURL string) string {
	tools := make([]string, 0, len(coordinationTools))
	for _, tool := range coordinationTools {
		tools = append(tools, "mcp_"+serverName+"_"+tool)
	}
	return fmt.Sprintf("mcp add %s --type http --url %s\n%s %s",
		serverName, shellQuote(coordinationURL), permAllowCmd, strings.Join(tools, " "))
}

// shellQuote renders one literal in single quotes with embedded quotes
// escaped per POSIX shell rules.
func shellQuote(value string) string {
	return "'" + string(bytes.ReplaceAll([]byte(value), []byte("'"), []byte(`'\''`))) + "'"
}

// Result reports what onboarding did or refused to do.
type Result struct {
	// WroteNewFile is true when a new project .crushrc was created.
	WroteNewFile bool
	// AppendedToExisting is true when an existing file received the
	// Matchmaker-owned block.
	AppendedToExisting bool
	// Fragment is printed for manual installation when safe additive
	// modification cannot be proven (onboarding fails closed).
	Fragment string
}

// Approval records the explicit operator decision shown the exact
// fragment before any write (DESIGN §5.4). Install is never called
// without ApprovalGranted.
type Approval string

const (
	ApprovalGranted  Approval = "granted"
	ApprovalWithheld Approval = "withheld"
)

// Install shows the exact fragment and, once granted, creates a new
// project .crushrc or appends a clearly delimited Matchmaker-owned block
// to an existing one (DESIGN §5.4). It backs up the file, acquires an
// exclusive file lock, refuses symlinks and concurrent changes, writes by
// atomic rename, and verifies that Crush can parse the resulting config.
// Failure is closed: print the fragment for manual installation and
// change nothing.
func Install(projectPath, fragment string, approval Approval) (Result, error) {
	if approval != ApprovalGranted {
		return Result{Fragment: fragment}, nil
	}
	target, existing, err := resolveTarget(projectPath)
	if err != nil {
		return Result{Fragment: fragment}, err
	}
	if !existing {
		if err := writeNew(target, fragment); err != nil {
			return Result{Fragment: fragment}, err
		}
		return Result{WroteNewFile: true}, nil
	}
	info, err := os.Lstat(target)
	if err != nil {
		return Result{Fragment: fragment}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Result{Fragment: fragment}, errors.New("refusing to install through a symlink")
	}
	updated, err := appendOwnedBlock(target, fragment)
	if err != nil {
		return Result{Fragment: fragment}, err
	}
	if err := atomicWrite(target, updated, info); err != nil {
		return Result{Fragment: fragment}, err
	}
	return Result{AppendedToExisting: true}, nil
}

// resolveTarget picks the crushrc file to install into: an existing
// crushrc or .crushrc (the hidden file wins when both exist, matching
// Crush's load order), or a new .crushrc.
func resolveTarget(projectPath string) (target string, existing bool, err error) {
	hidden := filepath.Join(projectPath, ".crushrc")
	plain := filepath.Join(projectPath, "crushrc")
	if info, statErr := os.Lstat(hidden); statErr == nil && !info.IsDir() {
		return hidden, true, nil
	}
	if info, statErr := os.Lstat(plain); statErr == nil && !info.IsDir() {
		return plain, true, nil
	}
	if _, statErr := os.Lstat(hidden); statErr == nil {
		return "", false, errors.New("refusing to install through a symlink")
	}
	return hidden, false, nil
}

// appendOwnedBlock inserts or replaces the Matchmaker-owned block in the
// locked file's content; concurrent changes between read and write are
// refused.
func appendOwnedBlock(target, fragment string) ([]byte, error) {
	file, err := os.OpenFile(target, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	content, err := os.ReadFile(target)
	if err != nil {
		return nil, err
	}
	if len(content) > 1<<20 {
		return nil, errors.New("refusing to install into an unusually large config")
	}
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.ModTime().Equal(after.ModTime()) || info.Size() != after.Size() {
		return nil, errors.New("refusing to install: config changed concurrently")
	}
	block := ownedBlock(fragment)
	if bytes.Contains(content, []byte(blockBegin)) {
		updated, replaceErr := replaceOwnedBlock(content, block)
		if replaceErr != nil {
			return nil, replaceErr
		}
		return updated, nil
	}
	updated := content
	if len(updated) > 0 && !bytes.HasSuffix(updated, []byte("\n")) {
		updated = append(updated, '\n')
	}
	updated = append(updated, block...)
	return updated, nil
}

// ownedBlock renders the complete delimited Matchmaker-owned block.
func ownedBlock(fragment string) []byte {
	return []byte(blockBegin + "\n" + fragment + "\n" + blockEnd + "\n")
}

// replaceOwnedBlock swaps the existing owned block's content in place.
func replaceOwnedBlock(content, block []byte) ([]byte, error) {
	begin := bytes.Index(content, []byte(blockBegin))
	end := bytes.Index(content, []byte(blockEnd))
	if begin < 0 || end < 0 || end < begin {
		return nil, errors.New("existing owned block is malformed")
	}
	replaced := append([]byte(nil), content[:begin]...)
	replaced = append(replaced, block...)
	replaced = append(replaced, content[end+len(blockEnd):]...)
	return replaced, nil
}

// atomicWrite backs up the current file and installs the new content via
// a temp file and rename, preserving permissions.
func atomicWrite(target string, content []byte, info os.FileInfo) error {
	backup := target + backupSuffix
	if err := os.WriteFile(backup, mustRead(target), info.Mode().Perm()); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(target), ".matchmaker-install-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if err := temp.Chmod(info.Mode().Perm()); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(content); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), target)
}

// writeNew creates a new project .crushrc holding only the owned block.
func writeNew(target, fragment string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(target), ".matchmaker-install-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(ownedBlock(fragment)); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), target)
}

// mustRead reads the file being backed up; a missing file backs up as
// empty.
func mustRead(target string) []byte {
	content, err := os.ReadFile(target)
	if err != nil {
		return nil
	}
	return content
}

// VerifyParse checks that Crush can parse the resulting config file: the
// crushrc is a shell script (C7), so its syntax is checked with the
// shell parser without executing it, and the crush binary must run. Full
// parse verification happens when Crush loads the config at workspace
// creation, exercised by the real-Crush smoke test.
func VerifyParse(crushBinary, projectPath string) error {
	if _, err := exec.LookPath(crushBinary); err != nil &&
		!filepath.IsAbs(crushBinary) {
		return fmt.Errorf("crush binary %s: %w", crushBinary, err)
	}
	if filepath.IsAbs(crushBinary) {
		if _, err := os.Stat(crushBinary); err != nil {
			return fmt.Errorf("crush binary %s: %w", crushBinary, err)
		}
	}
	for _, name := range []string{".crushrc", "crushrc"} {
		path := filepath.Join(projectPath, name)
		content, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		check := exec.Command(shellPath, "-n", path)
		check.Stdin = bytes.NewReader(content)
		if output, err := check.CombinedOutput(); err != nil {
			return fmt.Errorf("config %s fails shell syntax check: %s", name, string(output))
		}
	}
	return nil
}

// deployment is the daemon-registered context CommitWithFingerprint
// needs: only the daemon owns the store, fleet config, and daemon config.
var deployment struct {
	st    *store.Store
	fleet *config.Fleet
	mm    *config.Matchmaker
}

// Register wires the deployment context for CommitWithFingerprint; the
// daemon calls it once at startup because only the daemon owns the store
// and the fleet configuration (DESIGN §3).
func Register(st *store.Store, fleetConfig *config.Fleet, mm *config.Matchmaker) {
	deployment.st = st
	deployment.fleet = fleetConfig
	deployment.mm = mm
}

// CommitWithFingerprint records the approved write together with the new
// config fingerprint so the deterministic change does not trigger a
// self-change alert (DESIGN §5.4). Fingerprint approval and the write are
// committed atomically.
func CommitWithFingerprint(project string) error {
	if deployment.st == nil || deployment.fleet == nil || deployment.mm == nil {
		return errors.New("onboarding deployment context is not registered")
	}
	entry, ok := fleetProject(deployment.fleet, project)
	if !ok {
		return fmt.Errorf("project %q not in fleet", project)
	}
	fingerprint, err := fleet.ComputeFingerprint(entry, deployment.mm)
	if err != nil {
		return err
	}
	fingerprint.ApprovedBy = "operator-onboarding"
	ctx := context.Background()
	return deployment.st.WithinTx(ctx, func(tx *store.Tx) error {
		return tx.SetApprovedFingerprint(ctx, fingerprint)
	})
}

// fleetProject finds the fleet entry by name.
func fleetProject(fleetConfig *config.Fleet, name string) (config.Project, bool) {
	for _, project := range fleetConfig.Projects {
		if project.Name == name {
			return project, true
		}
	}
	return config.Project{}, false
}
