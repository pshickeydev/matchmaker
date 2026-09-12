// Package onboarding installs the Matchmaker-owned crushrc registration
// block for per-project coordination MCP registration (DESIGN §5.4).
package onboarding

const notImplemented = "not implemented"

// GenerateFragment produces the deterministic shell fragment containing
// one literal `mcp add matchmaker --type http --url ...` command. All
// generated values use shell-safe literal quoting; no value derived from
// agent or note content ever enters config.
func GenerateFragment(coordinationURL string) string { panic(notImplemented) }

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
func Install(projectPath, fragment string, approval Approval) (Result, error) { panic(notImplemented) }

// VerifyParse checks that Crush can parse the resulting config file.
func VerifyParse(crushBinary, projectPath string) error { panic(notImplemented) }

// CommitWithFingerprint records the approved write together with the new
// config fingerprint so the deterministic change does not trigger a
// self-change alert (DESIGN §5.4). Fingerprint approval and the write are
// committed atomically.
func CommitWithFingerprint(project string) error { panic(notImplemented) }
