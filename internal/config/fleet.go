// Package config loads and validates the fleet configuration (DESIGN §4.1)
// and Matchmaker's own operational configuration (limits, timings,
// policies from §5.2, §5.4, §5.7, §7).
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/pshickeydev/matchmaker/internal/model"
)

// defaultDataDirName is the per-project Crush data directory created
// inside each managed project when no explicit data_dir is declared
// (DESIGN §5.6).
const defaultDataDirName = ".crush"

// loopbackURLFmt formats a project's server base URL from its port
// (DESIGN §3: one Crush server per project on a dedicated loopback port).
const loopbackURLFmt = "http://127.0.0.1:%d"

// tagPattern is the syntax of one tag token and of each member of a tag
// expression (plan Decision 5).
var tagPattern = regexp.MustCompile(`^[a-zA-Z0-9_:.-]+$`)

// tagListSep separates the members of a v1 tag expression, read as a
// conjunction (plan Decision 5).
const tagListSep = ","

// rawProject mirrors one fleet.toml [[projects]] entry for strict TOML
// decoding.
type rawProject struct {
	Name         string       `toml:"name"`
	Path         string       `toml:"path"`
	Port         int          `toml:"port"`
	Tags         []string     `toml:"tags"`
	NoteOptOut   bool         `toml:"note_opt_out"`
	CrushOptions rawCrushOpts `toml:"crush_options"`
}

// rawCrushOpts is the typed, allowlisted optional server settings surface.
type rawCrushOpts struct {
	Debug          *bool  `toml:"debug"`
	DataDir        string `toml:"data_dir"`
	InheritDataDir bool   `toml:"inherit_data_dir"`
}

// rawFleet mirrors fleet.toml's top-level shape.
type rawFleet struct {
	Projects []rawProject `toml:"projects"`
}

// CrushOptions is the typed, allowlisted optional server settings
// surface; v1 supports debug, data_dir, and inherit_data_dir (DESIGN
// §4.1, §6).
type CrushOptions struct {
	Debug          *bool
	DataDir        string
	InheritDataDir bool
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
	var raw rawFleet
	meta, err := toml.DecodeFile(path, &raw)
	if err != nil {
		return nil, fmt.Errorf("decode fleet config %s: %w", path, err)
	}
	if err := rejectUndecoded(meta.Undecoded(), path); err != nil {
		return nil, err
	}
	fleet := &Fleet{Projects: make([]Project, 0, len(raw.Projects))}
	for i, rp := range raw.Projects {
		project, err := buildProject(rp)
		if err != nil {
			return nil, fmt.Errorf("fleet config %s: projects[%d]: %w", path, i, err)
		}
		fleet.Projects = append(fleet.Projects, project)
	}
	return fleet, nil
}

// buildProject canonicalizes one raw entry into a Project. DataDir is
// normalized to its effective value: the user's own data directory when
// inherit_data_dir is set, the declared directory if set, otherwise
// <project>/.crush (DESIGN §4.1, §5.6).
func buildProject(rp rawProject) (Project, error) {
	if rp.Name == "" {
		return Project{}, errors.New("name is required")
	}
	if rp.Path == "" {
		return Project{}, errors.New("path is required")
	}
	path, err := canonicalPath(rp.Path)
	if err != nil {
		return Project{}, fmt.Errorf("path %s: %w", rp.Path, err)
	}
	dataDir := rp.CrushOptions.DataDir
	switch {
	case rp.CrushOptions.InheritDataDir && dataDir != "":
		return Project{}, errors.New("crush_options: data_dir and inherit_data_dir are mutually exclusive")
	case rp.CrushOptions.InheritDataDir:
		userDir, dirErr := userCrushDataDir()
		if dirErr != nil {
			return Project{}, fmt.Errorf("inherit_data_dir: %w", dirErr)
		}
		if userDir, dirErr = canonicalPath(userDir); dirErr != nil {
			return Project{}, fmt.Errorf("inherit_data_dir %s: %w", userDir, dirErr)
		}
		dataDir = userDir
	case dataDir == "":
		dataDir = filepath.Join(path, defaultDataDirName)
	default:
		if dataDir, err = canonicalPath(dataDir); err != nil {
			return Project{}, fmt.Errorf("data_dir %s: %w", rp.CrushOptions.DataDir, err)
		}
	}
	return Project{
		Name: rp.Name,
		Path: path,
		Port: rp.Port,
		Tags: rp.Tags,
		CrushOptions: CrushOptions{
			Debug:          rp.CrushOptions.Debug,
			DataDir:        dataDir,
			InheritDataDir: rp.CrushOptions.InheritDataDir,
		},
		NoteOptOut: rp.NoteOptOut,
	}, nil
}

// userCrushDataDir resolves the user's own default Crush data directory
// (`$XDG_DATA_HOME/crush`, else `~/.local/share/crush`): the directory
// interactive `crush login` writes OAuth credentials into, which
// inherit_data_dir projects share (DESIGN §4.1).
func userCrushDataDir() (string, error) {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, "crush"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "crush"), nil
}

// canonicalPath expands a leading ~, makes the path absolute, and
// resolves symlinks where the path exists.
func canonicalPath(path string) (string, error) {
	expanded, err := expandHome(path)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

// expandHome replaces a leading ~ with the user's home directory.
func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand ~: %w", err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}

// rejectUndecoded turns any strict-decoding leftover key into an error.
func rejectUndecoded(keys []toml.Key, path string) error {
	if len(keys) == 0 {
		return nil
	}
	names := make([]string, 0, len(keys))
	for _, key := range keys {
		names = append(names, key.String())
	}
	return fmt.Errorf("unknown config key(s) in %s: %s", path, strings.Join(names, ", "))
}

// Validate enforces the uniqueness constraints of DESIGN §4.1: unique
// names, ports, canonical paths, and effective data_dir values. All
// collisions are collected and reported together; any error must be
// surfaced before daemon reconciliation starts.
func (f *Fleet) Validate() error {
	var errs []error
	errs = append(errs, checkUnique(f.Projects, "project name", func(p Project) string { return p.Name })...)
	errs = append(errs, checkUnique(f.Projects, "port", func(p Project) string { return fmt.Sprint(p.Port) })...)
	errs = append(errs, checkUnique(f.Projects, "canonical path", func(p Project) string { return p.Path })...)
	// inherit_data_dir projects share the user's data dir by design and
	// are exempt from the data-dir uniqueness check (DESIGN §4.1).
	declared := make([]Project, 0, len(f.Projects))
	for _, project := range f.Projects {
		if !project.CrushOptions.InheritDataDir {
			declared = append(declared, project)
		}
	}
	errs = append(errs, checkUnique(declared, "effective data_dir", func(p Project) string { return p.CrushOptions.DataDir })...)
	for _, project := range f.Projects {
		if project.Port < 1 || project.Port > 65535 {
			errs = append(errs, fmt.Errorf("project %s: port %d out of range", project.Name, project.Port))
		}
		for _, tag := range project.Tags {
			if !tagPattern.MatchString(tag) {
				errs = append(errs, fmt.Errorf("project %s: invalid tag %q", project.Name, tag))
			}
		}
	}
	return errors.Join(errs...)
}

// checkUnique reports each value used by more than one project.
func checkUnique(projects []Project, what string, value func(Project) string) []error {
	seen := make(map[string]string)
	var errs []error
	for _, project := range projects {
		v := value(project)
		if first, dup := seen[v]; dup {
			errs = append(errs, fmt.Errorf("duplicate %s %s: projects %s and %s", what, v, first, project.Name))
			continue
		}
		seen[v] = project.Name
	}
	return errs
}

// Project returns the named project, or an error if absent.
func (f *Fleet) Project(name string) (Project, error) {
	for _, project := range f.Projects {
		if project.Name == name {
			return project, nil
		}
	}
	return Project{}, fmt.Errorf("project %q not in fleet", name)
}

// Snapshot returns an immutable point-in-time copy of the fleet used as
// the single validation context for a goal submission (DESIGN §5.2).
func (f *Fleet) Snapshot() *FleetSnapshot {
	projects := make([]Project, len(f.Projects))
	for i, project := range f.Projects {
		tags := slices.Clone(project.Tags)
		projects[i] = project
		projects[i].Tags = tags
	}
	return &FleetSnapshot{Projects: projects}
}

// FleetSnapshot is an immutable fleet view frozen for validation and
// target expansion; fleet changes after goal acceptance do not alter a
// goal's target set.
type FleetSnapshot struct {
	Projects []Project
}

// Project returns the named project from the snapshot.
func (s *FleetSnapshot) Project(name string) (Project, error) {
	for _, project := range s.Projects {
		if project.Name == name {
			return project, nil
		}
	}
	return Project{}, fmt.Errorf("project %q not in fleet snapshot", name)
}

// ResolveTarget expands a step's TargetSpec against the snapshot into
// fully resolved targets (DESIGN §5.2: expansion happens during
// validation so fan-out consumes the same limits as explicit targets).
func (s *FleetSnapshot) ResolveTarget(spec model.TargetSpec) ([]model.ResolvedTarget, error) {
	set := 0
	for _, isSet := range []bool{len(spec.Explicit) > 0, spec.TagExpr != "", spec.All} {
		if isSet {
			set++
		}
	}
	if set != 1 {
		return nil, errors.New("target must set exactly one of explicit projects, a tag expression, or all")
	}
	switch {
	case spec.All:
		return s.resolveAll(), nil
	case spec.TagExpr != "":
		if err := ValidateTagExpr(spec.TagExpr); err != nil {
			return nil, err
		}
		return s.resolveTags(strings.Split(spec.TagExpr, tagListSep))
	default:
		return s.resolveExplicit(spec.Explicit)
	}
}

// resolveAll fans out to every project in the snapshot, ordered by name.
func (s *FleetSnapshot) resolveAll() []model.ResolvedTarget {
	names := make([]string, 0, len(s.Projects))
	for _, project := range s.Projects {
		names = append(names, project.Name)
	}
	slices.Sort(names)
	targets := make([]model.ResolvedTarget, 0, len(names))
	for _, name := range names {
		project, err := s.Project(name)
		if err != nil {
			continue
		}
		targets = append(targets, resolvedFor(project))
	}
	return targets
}

// resolveExplicit resolves named projects; every name must exist.
func (s *FleetSnapshot) resolveExplicit(names []string) ([]model.ResolvedTarget, error) {
	var missing []string
	targets := make([]model.ResolvedTarget, 0, len(names))
	for _, name := range names {
		project, err := s.Project(name)
		if err != nil {
			missing = append(missing, name)
			continue
		}
		targets = append(targets, resolvedFor(project))
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("unknown project(s): %s", strings.Join(missing, ", "))
	}
	return targets, nil
}

// resolveTags resolves a conjunction tag list (plan Decision 5): a
// project matches when it carries every listed tag.
func (s *FleetSnapshot) resolveTags(tags []string) ([]model.ResolvedTarget, error) {
	trimmed := make([]string, 0, len(tags))
	for _, tag := range tags {
		trimmed = append(trimmed, strings.TrimSpace(tag))
	}
	var targets []model.ResolvedTarget
	for _, project := range s.Projects {
		matches := true
		for _, tag := range trimmed {
			if !slices.Contains(project.Tags, tag) {
				matches = false
				break
			}
		}
		if matches {
			targets = append(targets, resolvedFor(project))
		}
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("tag expression %q resolves to no projects", strings.Join(trimmed, tagListSep))
	}
	return targets, nil
}

// resolvedFor builds one resolved target for a project.
func resolvedFor(project Project) model.ResolvedTarget {
	return model.ResolvedTarget{
		Project:   project.Name,
		Instance:  project.Name,
		ServerURL: fmt.Sprintf(loopbackURLFmt, project.Port),
		Tags:      slices.Clone(project.Tags),
	}
}

// ValidateTagExpr checks that a tag expression is syntactically valid
// without resolving it.
func ValidateTagExpr(expr string) error {
	if strings.TrimSpace(expr) == "" {
		return errors.New("tag expression is empty")
	}
	for tag := range strings.SplitSeq(expr, tagListSep) {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			return fmt.Errorf("tag expression %q has an empty member", expr)
		}
		if !tagPattern.MatchString(tag) {
			return fmt.Errorf("tag expression %q has invalid tag %q", expr, tag)
		}
	}
	return nil
}
