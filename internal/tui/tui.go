// Package tui implements the bubbletea dashboard: a live view of goals,
// runs, and notes (DESIGN §10.1). All agent-controlled strings pass
// through the terminal renderer before display (§5.5).
package tui

import tea "github.com/charmbracelet/bubbletea"

const notImplemented = "not implemented"

// Model is the dashboard's bubbletea model.
type Model struct{}

// New creates the dashboard model over the RPC client.
func New(client Client) Model { panic(notImplemented) }

// Client is the minimal status surface the TUI consumes from the daemon.
type Client interface {
	Status() (StatusSnapshot, error)
}

// StatusSnapshot is one polled view of the fleet, goals, and runs.
type StatusSnapshot struct {
	Goals     []GoalRow
	Instances []InstanceRow
	Notes     []NoteRow
}

// GoalRow, InstanceRow, and NoteRow are the display rows.
type GoalRow struct {
	ID     string
	Status string
	Steps  string
}

type InstanceRow struct {
	Project    string
	State      string
	Generation int64
}

type NoteRow struct {
	NoteID int64
	From   string
	Body   string
}

// Init implements bubbletea.Model.
func (m Model) Init() tea.Cmd { panic(notImplemented) }

// Update implements bubbletea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) { panic(notImplemented) }

// View implements bubbletea.Model; every agent-derived string is passed
// through render.Render first.
func (m Model) View() string { panic(notImplemented) }
