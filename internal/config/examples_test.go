package config

import (
	"path/filepath"
	"testing"
)

func TestExampleConfigsParse(t *testing.T) {
	fleet, err := Load(filepath.Join("..", "..", "examples", "fleet.toml"))
	if err != nil {
		t.Fatalf("examples/fleet.toml: %v", err)
	}
	if len(fleet.Projects) == 0 {
		t.Fatal("example fleet has no projects")
	}
	if err := fleet.Validate(); err != nil {
		t.Errorf("example fleet invalid: %v", err)
	}
	mm, err := LoadMatchmaker(filepath.Join("..", "..", "examples", "matchmaker.toml"))
	if err != nil {
		t.Fatalf("examples/matchmaker.toml: %v", err)
	}
	if mm.StateDir == "" || mm.Fleet.ApprovedVersion != PinnedCrushVersion {
		t.Errorf("example matchmaker config = %+v", mm)
	}
}
