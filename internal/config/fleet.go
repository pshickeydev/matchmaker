// Package config loads and validates the fleet configuration (DESIGN §4.1)
// and Matchmaker's own operational configuration (limits, timings,
// policies from §5.2, §5.4, §5.7, §7).
package config

import (
	"github.com/pshickeydev/matchmaker/internal/model"
)

const notImplemented = "not implemented"

// CrushOptions is the typed, allowlisted optional server settings surface;
// v1 supports only debug and data_dir (DESIGN §4.1, §6).
type CrushOptions struct {
	Debug   *bool
	DataDir string
}

// Project is one entry of the statically declared fleet (DESIGN §4.1).
type Project struct {
	Name         string
	Path         string
	Port         int
	Tags         []string
	CrushOptions CrushOptions
	NoteOptOut   bool // projects may opt out of coordination MCP registration (§5.4, §5.6)
}

// Fleet is the desired-state input to the reconciliation loop (DESIGN §5.1).
type Fleet struct {
	Projects []Project
}

// Load reads and parses the fleet config from path, canonicalizing
// project paths and data directories.
func Load(path string) (*Fleet, error) {
	panic(notImplemented)
}

// Validate enforces the uniqueness constraints of DESIGN §4.1: unique
// names, ports, canonical paths, and effective data_dir values. All
// collisions are collected and reported together; any error must be
// surfaced before daemon reconciliation starts.
func (f *Fleet) Validate() error {
	panic(notImplemented)
}

// Project returns the named project, or an error if absent.
func (f *Fleet) Project(name string) (Project, error) {
	panic(notImplemented)
}

// Snapshot returns an immutable point-in-time copy of the fleet used as
// the single validation context for a goal submission (DESIGN §5.2).
func (f *Fleet) Snapshot() *FleetSnapshot {
	panic(notImplemented)
}

// FleetSnapshot is an immutable fleet view frozen for validation and
// target expansion; fleet changes after goal acceptance do not alter a
// goal's target set.
type FleetSnapshot struct {
	Projects []Project
}

// ResolveTarget expands a step's TargetSpec against the snapshot into
// fully resolved targets (DESIGN §5.2: expansion happens during
// validation so fan-out consumes the same limits as explicit targets).
func (s *FleetSnapshot) ResolveTarget(spec model.TargetSpec) ([]model.ResolvedTarget, error) {
	panic(notImplemented)
}

// ValidateTagExpr checks that a tag expression is syntactically valid
// without resolving it.
func ValidateTagExpr(expr string) error {
	panic(notImplemented)
}
