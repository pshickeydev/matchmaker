package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestRootCommandTree(t *testing.T) {
	root := NewRootCmd()
	names := commandNames(root.Commands())
	for _, want := range []string{"daemon", "goal", "fleet", "onboard", "shutdown", "prune", "tui"} {
		if !contains(names, want) {
			t.Errorf("root command tree missing %q (has %v)", want, names)
		}
	}
	goal := findCommand(root, "goal")
	if goal == nil {
		t.Fatal("goal command missing")
	}
	for _, want := range []string{"submit", "plan", "status", "report", "abandon"} {
		if findCommand(goal, want) == nil {
			t.Errorf("goal command missing %q", want)
		}
	}
	fleet := findCommand(root, "fleet")
	if fleet == nil {
		t.Fatal("fleet command missing")
	}
	for _, want := range []string{"list", "approve", "override-version", "reset"} {
		if findCommand(fleet, want) == nil {
			t.Errorf("fleet command missing %q", want)
		}
	}
}

// commandNames lists subcommand names.
func commandNames(commands []*cobra.Command) []string {
	var names []string
	for _, command := range commands {
		names = append(names, command.Name())
	}
	return names
}

// findCommand returns a named subcommand.
func findCommand(parent *cobra.Command, name string) *cobra.Command {
	for _, command := range parent.Commands() {
		if command.Name() == name {
			return command
		}
	}
	return nil
}

// contains reports membership.
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestDeferredCommandsReferenceMilestone(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{args: []string{"goal", "plan", "do things"}, want: "planning runs land with"},
		{args: []string{"prune"}, want: "post-MVP backlog"},
		{args: []string{"tui"}, want: "post-MVP backlog"},
	}
	for _, tt := range tests {
		err := Execute(tt.args)
		if err == nil {
			t.Errorf("%v succeeded; deferred commands must error", tt.args)
			continue
		}
		if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v error = %q, want %q", tt.args, err.Error(), tt.want)
		}
	}
}

func TestExecutePrintsHelpWithoutArgs(t *testing.T) {
	var out bytes.Buffer
	root := NewRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{})
	if err := root.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "matchmaker") {
		t.Errorf("help output = %q", out.String())
	}
}
