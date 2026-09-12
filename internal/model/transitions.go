package model

import (
	"fmt"
	"slices"
)

// RunIsTerminal reports whether status is terminal and therefore
// immutable (DESIGN §4.5).
func RunIsTerminal(s RunStatus) bool {
	switch s {
	case RunCompleted, RunFailed, RunCancelled, RunTimedOut, RunSkipped, RunAbandoned:
		return true
	default:
		return false
	}
}

// RunIsNonterminal reports the states that keep a target execution or
// workspace blocked from further dispatch: queued, dispatching, running,
// cancelling, and unknown.
func RunIsNonterminal(s RunStatus) bool {
	return !RunIsTerminal(s)
}

// RunTransitionAllowed enforces the run state machine of DESIGN §4.5.
// Transitions out of terminal states are always rejected.
func RunTransitionAllowed(from, to RunStatus) error {
	if RunIsTerminal(from) {
		return fmt.Errorf("run state %q is terminal and immutable", from)
	}
	allowed := map[RunStatus][]RunStatus{
		RunQueued:      {RunDispatching, RunSkipped, RunAbandoned},
		RunDispatching: {RunRunning, RunUnknown, RunAbandoned},
		RunRunning:     {RunCancelling, RunCompleted, RunFailed, RunCancelled, RunUnknown, RunAbandoned},
		RunCancelling:  {RunCompleted, RunCancelled, RunTimedOut, RunUnknown, RunAbandoned},
		RunUnknown:     {RunRunning, RunCancelling, RunCompleted, RunFailed, RunCancelled, RunTimedOut, RunAbandoned},
	}
	if slices.Contains(allowed[from], to) {
		return nil
	}
	return fmt.Errorf("run transition %q -> %q not permitted", from, to)
}

// InstanceRejectsDispatch reports whether an instance state blocks new run
// dispatch (DESIGN §4.2): approval_required, version_mismatch, draining,
// and failed reject new dispatch.
func InstanceRejectsDispatch(s InstanceState) bool {
	switch s {
	case InstanceApprovalRequired, InstanceVersionMismatch, InstanceDraining, InstanceFailed:
		return true
	default:
		return false
	}
}

// InstanceTransitionAllowed enforces the instance state machine of
// DESIGN §4.2.
func InstanceTransitionAllowed(from, to InstanceState) error {
	if from == to {
		return nil
	}
	allowed := map[InstanceState][]InstanceState{
		InstanceStopped:  {InstanceStarting, InstanceApprovalRequired, InstanceVersionMismatch, InstanceFailed},
		InstanceStarting: {InstanceReady, InstanceBusy, InstanceApprovalRequired, InstanceVersionMismatch, InstanceDraining, InstanceFailed},
		InstanceReady: {
			InstanceBusy, InstanceDraining, InstanceApprovalRequired,
			InstanceVersionMismatch, InstanceFailed,
		},
		InstanceBusy: {
			InstanceReady, InstanceDraining, InstanceApprovalRequired,
			InstanceFailed,
		},
		InstanceApprovalRequired: {InstanceStopped, InstanceBusy, InstanceFailed},
		InstanceVersionMismatch:  {InstanceStopped, InstanceFailed},
		InstanceDraining:         {InstanceStopped, InstanceFailed},
		InstanceFailed:           {InstanceStopped},
	}
	if slices.Contains(allowed[from], to) {
		return nil
	}
	return fmt.Errorf("instance transition %q -> %q not permitted", from, to)
}

// AcceptsPartialNeeds reports whether a step may run when an upstream
// fan-out step is partial (DESIGN §4.4, §5.3).
func (s Step) AcceptsPartialNeeds() bool { return s.AcceptPartialNeeds }

// SupervisionPolicyRecognized reports whether policy is valid in v1;
// the reserved "ask" policy is rejected (DESIGN §4.4).
func SupervisionPolicyRecognized(p SupervisionPolicy) bool {
	return p == SupervisionDeny || p == SupervisionGrantAll
}

// ExpandAudience resolves an audience to the set of participating project
// names within goal participants. Audience expansion is frozen when the
// note is accepted (DESIGN §5.4).
func ExpandAudience(a Audience, participants map[string][]string) []string {
	switch {
	case a.All:
		var all []string
		for project := range participants {
			all = append(all, project)
		}
		return all
	case a.Tag != "":
		return append([]string(nil), participants[a.Tag]...)
	default:
		return []string{a.Project}
	}
}
