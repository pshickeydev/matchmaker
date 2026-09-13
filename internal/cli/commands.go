// Package cli defines the matchmaker command surface (DESIGN §10.1):
// goal planning and submission, fleet management, report viewing, plus
// daemon control. Commands are short-lived RPC clients of the daemon.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/pshickeydev/matchmaker/internal/config"
	"github.com/pshickeydev/matchmaker/internal/daemon"
	"github.com/pshickeydev/matchmaker/internal/goalvalidate"
	"github.com/pshickeydev/matchmaker/internal/model"
	"github.com/pshickeydev/matchmaker/internal/onboarding"
	"github.com/pshickeydev/matchmaker/internal/render"
	"github.com/pshickeydev/matchmaker/internal/rpc"
)

const (
	// defaultStateDir mirrors the config default for client commands.
	defaultStateDir = "~/.local/state/matchmaker"
	// callTimeout bounds one client RPC.
	callTimeout = 30 * time.Second
	// notImplementedMilestone references deferred commands' milestone.
	notImplementedMilestone = "post-MVP backlog (M10, plan §3)"
)

// Execute runs the root command with args, returning its error.
func Execute(args []string) error {
	root := NewRootCmd()
	root.SetArgs(args)
	return root.Execute()
}

// NewRootCmd builds the command tree.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "matchmaker",
		Short: "Coordinate a fleet of Crush agents on step-DAG goals",
	}
	root.PersistentFlags().String("state-dir", defaultStateDir,
		"daemon state directory holding the store, lock, and RPC socket")
	root.AddCommand(
		newDaemonCmd(),
		newGoalCmd(),
		newFleetCmd(),
		newOnboardCmd(),
		newShutdownCmd(),
		newPruneCmd(),
		newTUICmd(),
	)
	return root
}

// stateDirOf resolves the effective state directory for a command.
func stateDirOf(cmd *cobra.Command) (string, error) {
	dir, err := cmd.Flags().GetString("state-dir")
	if err != nil {
		return "", err
	}
	if dir == defaultStateDir {
		if env := os.Getenv("MATCHMAKER_STATE_DIR"); env != "" {
			dir = env
		}
	}
	return expandTilde(dir)
}

// expandTilde expands a leading ~ in one path.
func expandTilde(path string) (string, error) {
	if len(path) == 0 || path[0] != '~' {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if path == "~" {
		return home, nil
	}
	return home + path[1:], nil
}

// call performs one RPC against the daemon running in the state dir.
func call(cmd *cobra.Command, request rpc.Request) (rpc.Response, error) {
	dir, err := stateDirOf(cmd)
	if err != nil {
		return rpc.Response{}, err
	}
	client, err := rpc.Connect(dir)
	if err != nil {
		return rpc.Response{}, err
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	return client.Call(ctx, request)
}

// newDaemonCmd runs the long-lived daemon in the foreground: acquire the
// state-dir lock, open the store, start reconciliation, dispatch,
// supervision, the coordination MCP server, and the local RPC socket.
func newDaemonCmd() *cobra.Command {
	command := &cobra.Command{
		Use:   "daemon",
		Short: "Run the matchmaker daemon in the foreground",
		RunE: func(cmd *cobra.Command, args []string) error {
			fleetPath, _ := cmd.Flags().GetString("fleet")
			mmPath, _ := cmd.Flags().GetString("config")
			stateDir, _ := cmd.Flags().GetString("state-dir")
			fleet, err := config.Load(fleetPath)
			if err != nil {
				return err
			}
			if err := fleet.Validate(); err != nil {
				return err
			}
			mm, err := config.LoadMatchmaker(mmPath)
			if err != nil {
				return err
			}
			if stateDir == "" {
				stateDir = mm.StateDir
			}
			stateDir, err = expandTilde(stateDir)
			if err != nil {
				return err
			}
			d, err := daemon.New(stateDir, fleet, mm)
			if err != nil {
				return err
			}
			return d.Run(cmd.Context())
		},
	}
	command.Flags().String("fleet", "fleet.toml", "fleet desired-state config path")
	command.Flags().String("config", "matchmaker.toml", "daemon configuration path")
	return command
}

// newGoalCmd groups: submit, plan, status, report, abandon.
func newGoalCmd() *cobra.Command {
	goal := &cobra.Command{Use: "goal", Short: "Submit and inspect goals"}
	goal.AddCommand(
		newGoalSubmitCmd(),
		newGoalPlanCmd(),
		newGoalStatusCmd(),
		newGoalReportCmd(),
		newGoalAbandonCmd(),
	)
	return goal
}

// newGoalSubmitCmd submits a goal file through the daemon; the daemon
// validates atomically (§5.2) and rejects with all validation errors,
// persisting nothing on any error.
func newGoalSubmitCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "submit <goal.json>",
		Short: "Submit a goal file for atomic validation and dispatch",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			draft, err := goalvalidate.ParseSubmission(data)
			if err != nil {
				return err
			}
			response, err := call(cmd, rpc.Request{
				IdempotencyKey: "submit-" + time.Now().UTC().Format(time.RFC3339Nano),
				Method:         rpc.MethodSubmitGoal,
				Submit:         &rpc.SubmitRequest{Draft: draft},
			})
			if err != nil {
				return err
			}
			if len(response.Validation) > 0 {
				for _, validationErr := range response.Validation {
					fmt.Println(render.Render(validationErr.Error()))
				}
				return errors.New("goal rejected")
			}
			fmt.Println("accepted", response.GoalID)
			return nil
		},
	}
}

// newGoalPlanCmd implements `matchmaker goal plan "<objective>" --on
// <project>` (§5.7): sends the plan request, waits for the planning run,
// returns the draft for operator review rendered through the terminal
// renderer, and writes it to an operator-selected path with operator-only
// permissions. --auto-submit submits a valid draft immediately but never
// one whose steps use grant_all.
func newGoalPlanCmd() *cobra.Command {
	command := &cobra.Command{
		Use:   "plan <objective>",
		Short: "Draft a goal with a planning run (not implemented in the MVP)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("goal plan is not implemented in the MVP; planning runs land with %s", notImplementedMilestone)
		},
	}
	command.Flags().String("on", "", "planner project (no default, no semantic selection)")
	command.Flags().Bool("auto-submit", false, "submit a valid draft immediately (never with grant_all)")
	return command
}

// newGoalStatusCmd shows live goal, step, target, and run status.
func newGoalStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status [<goal-id>]",
		Short: "Show goal status",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				response, err := call(cmd, rpc.Request{Method: rpc.MethodListGoals})
				if err != nil {
					return err
				}
				for _, goal := range response.Goals {
					fmt.Printf("%s %s %s\n", goal.ID, goal.Status, render.Render(goal.Objective))
				}
				return nil
			}
			response, err := call(cmd, rpc.Request{Method: rpc.MethodGoalStatus, GoalID: args[0]})
			if err != nil {
				return err
			}
			if response.Goal == nil {
				return errors.New("goal not found")
			}
			fmt.Printf("goal %s %s\n", response.Goal.ID, response.Goal.Status)
			fmt.Println(render.Render(response.Goal.Objective))
			for _, step := range response.Goal.Steps {
				runs := 0
				for _, run := range response.Runs {
					if run.StepID != step.ID {
						continue
					}
					runs++
					fmt.Printf("  step %s %s %s %s\n", step.ID, run.Project, run.ID, run.Status)
				}
				if runs == 0 {
					fmt.Printf("  step %s\n", step.ID)
				}
			}
			return nil
		},
	}
}

// newGoalReportCmd builds and optionally exports the per-goal report
// (§5.5); export streams lazily resolved Crush content to an
// operator-selected file and never inserts it into the store.
func newGoalReportCmd() *cobra.Command {
	command := &cobra.Command{
		Use:   "report <goal-id>",
		Short: "Show or export the per-goal report",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			export, _ := cmd.Flags().GetString("export")
			if export == "" {
				response, err := call(cmd, rpc.Request{Method: rpc.MethodGoalStatus, GoalID: args[0]})
				if err != nil {
					return err
				}
				if response.Goal == nil {
					return errors.New("goal not found")
				}
				fmt.Printf("goal %s %s\n", response.Goal.ID, response.Goal.Status)
				return nil
			}
			response, err := call(cmd, rpc.Request{
				Method:   rpc.MethodExportReport,
				GoalID:   args[0],
				FilePath: export,
			})
			if err != nil {
				return err
			}
			fmt.Println(response.Message)
			return nil
		},
	}
	command.Flags().String("export", "", "export the full report to this file (operator-only permissions)")
	return command
}

// newGoalAbandonCmd performs the explicit operator abandonment of an
// unresolved run, accepting the outcome and releasing serialization.
func newGoalAbandonCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "abandon <run-id>",
		Short: "Abandon an unresolved run and release its serialization",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			response, err := call(cmd, rpc.Request{Method: rpc.MethodAbandonRun, RunID: args[0]})
			if err != nil {
				return err
			}
			fmt.Println(response.Message)
			return nil
		},
	}
}

// newFleetCmd groups: list, approve, override-version, reset.
func newFleetCmd() *cobra.Command {
	fleet := &cobra.Command{Use: "fleet", Short: "Inspect and manage the fleet"}
	fleet.AddCommand(
		newFleetListCmd(),
		newFleetApproveCmd(),
		newFleetOverrideCmd(),
		newFleetResetCmd(),
	)
	return fleet
}

// newFleetListCmd shows instance states, generations, and stream health.
func newFleetListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List fleet instances and their states",
		RunE: func(cmd *cobra.Command, args []string) error {
			response, err := call(cmd, rpc.Request{Method: rpc.MethodFleetStatus})
			if err != nil {
				return err
			}
			for _, instance := range response.Instances {
				fmt.Printf("%s %s gen=%d workspace=%s\n",
					instance.Project, instance.State, instance.Generation, instance.WorkspaceID)
			}
			return nil
		},
	}
}

// newFleetApproveCmd records explicit operator approval of a changed
// project config fingerprint, resuming reconciliation (§5.1).
func newFleetApproveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "approve <project>",
		Short: "Approve a project's changed Crush config fingerprint",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			response, err := call(cmd, rpc.Request{
				IdempotencyKey: "approve-" + args[0] + time.Now().UTC().Format(time.RFC3339Nano),
				Method:         rpc.MethodApproveFingerprint,
				Project:        args[0],
			})
			if err != nil {
				return err
			}
			fmt.Println(response.Message)
			return nil
		},
	}
}

// newFleetOverrideCmd records an explicit compatibility override for an
// observed Crush version and build_id (§9.3).
func newFleetOverrideCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "override-version <project> <version> [<build-id>]",
		Short: "Record a compatibility override for an observed Crush version",
		Args:  cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			buildID := ""
			if len(args) == 3 {
				buildID = args[2]
			}
			response, err := call(cmd, rpc.Request{
				IdempotencyKey: "override-" + args[0] + time.Now().UTC().Format(time.RFC3339Nano),
				Method:         rpc.MethodApproveVersion,
				Project:        args[0],
				Version:        args[1],
				BuildID:        buildID,
			})
			if err != nil {
				return err
			}
			fmt.Println(response.Message)
			return nil
		},
	}
}

// newFleetResetCmd moves a failed instance back to stopped for explicit
// operator reset (§4.2).
func newFleetResetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reset <project>",
		Short: "Reset a failed instance to stopped",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			response, err := call(cmd, rpc.Request{
				IdempotencyKey: "reset-" + args[0] + time.Now().UTC().Format(time.RFC3339Nano),
				Method:         rpc.MethodResetInstance,
				Project:        args[0],
			})
			if err != nil {
				return err
			}
			fmt.Println(response.Message)
			return nil
		},
	}
}

// newOnboardCmd shows the generated crushrc fragment and, after explicit
// approval, installs the Matchmaker-owned registration block (§5.4).
// Projects that opt out lose live tools but still receive injected notes.
func newOnboardCmd() *cobra.Command {
	command := &cobra.Command{
		Use:   "onboard <project>",
		Short: "Register the coordination MCP server in a project's crushrc",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			approval := ""
			if approved, _ := cmd.Flags().GetBool("approve"); approved {
				approval = string(onboarding.ApprovalGranted)
			}
			response, err := call(cmd, rpc.Request{
				IdempotencyKey: "onboard-" + args[0] + time.Now().UTC().Format(time.RFC3339Nano),
				Method:         rpc.MethodOnboard,
				Project:        args[0],
				Approval:       approval,
			})
			if err != nil {
				return err
			}
			if response.Fragment != "" {
				fmt.Println(render.Render(response.Fragment))
			}
			fmt.Println(response.Message)
			return nil
		},
	}
	command.Flags().Bool("approve", false, "install the shown fragment after review")
	return command
}

// newPruneCmd removes Matchmaker metadata per operator policy; Crush
// owns session-log retention (§5.5).
func newPruneCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "prune",
		Short: "Remove Matchmaker metadata per policy",
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("prune is not implemented in the MVP; it lands with %s", notImplementedMilestone)
		},
	}
}

// newShutdownCmd requests a clean daemon shutdown through the RPC:
// parallel instance drain under the global deadline, then client-claim
// retirement.
func newShutdownCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "shutdown",
		Short: "Request a clean daemon shutdown",
		RunE: func(cmd *cobra.Command, args []string) error {
			forced, _ := cmd.Flags().GetBool("force")
			mode := model.DrainGraceful
			if forced {
				mode = model.DrainForce
			}
			response, err := call(cmd, rpc.Request{Method: rpc.MethodShutdown, Shutdown: mode})
			if err != nil {
				return err
			}
			fmt.Println(response.Message)
			return nil
		},
	}
}

// newTUICmd runs the bubbletea dashboard: a live view of goals, runs,
// and notes (§10.1).
func newTUICmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Run the live dashboard",
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("tui is not implemented in the MVP; it lands with %s", notImplementedMilestone)
		},
	}
}
