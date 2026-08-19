package capsule

import (
	"context"
	"fmt"
	"github.com/rhobuild/runpool/internal/capsule/protocol"
	"strings"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/platform/docker"
)

// InspectExecution classifies a prepared runtime whose start outcome is
// unknown. The result comes from the daemon and supervisor, never from the
// controller's intended transition.
func (m *Launcher) InspectExecution(ctx context.Context, prepared PreparedRuntime) (assignment.ExecutionObservation, error) {
	state, err := m.dock.ContainerStatus(ctx, prepared.RuntimeID)
	switch {
	case err == nil:
	case docker.IsNotFound(err):
		return assignment.ObservedAbsent, nil
	default:
		return assignment.ObservedUnavailable, err
	}
	obs, askSupervisor, err := classifyContainerState(prepared.RuntimeID, state)
	if !askSupervisor {
		return obs, err
	}

	code, out, err := m.dock.Exec(ctx, prepared.RuntimeID, []string{supervisorPath, "state"})
	if err != nil || code != 0 {
		return assignment.ObservedUnavailable, fmt.Errorf("capsule state unreadable (exit %d): %s", code, out)
	}
	return classifySupervisorState(strings.TrimSpace(out))
}

// SupervisorAbortedExitCode is the status the capsule supervisor exits
// with when it stops before handing the job to the runner. The
// declaration lives in internal/capsule/protocol, imported by the
// supervisor too, so the two sides cannot disagree.
const SupervisorAbortedExitCode = protocol.AbortedExitCode

// ClassifyExit reads a stopped capsule's exit code. Any code but the
// reserved one means the runner owned the job, which includes a job that
// failed: that is an execution outcome, not an unstarted runtime.
//
// It is exported because a capsule's exit reaches the controller by two
// paths — the launch that awaited it and the reconciliation that adopted
// one — and a classification that only one of them applies is how the
// same capsule settles two different ways.
func ClassifyExit(code int) assignment.ExecutionObservation {
	if code == SupervisorAbortedExitCode {
		return assignment.ObservedCreated
	}
	return assignment.ObservedExited
}

// classifySupervisorState reads the supervisor's own account of itself. The
// distinction that matters is whether the runner ever started: the supervisor
// writes `running` immediately before executing it, so `aborted` means the job
// was never handed over and `failed` means it was. Collapsing the two settles
// an attempt that never ran as complete, and nothing requeues it.
func classifySupervisorState(state string) (assignment.ExecutionObservation, error) {
	switch {
	case state == protocol.StateBooting, state == protocol.StateWaiting:
		return assignment.ObservedCreated, nil
	case state == protocol.StateRunning:
		return assignment.ObservedRunning, nil
	case strings.HasPrefix(state, protocol.AbortedPrefix):
		return assignment.ObservedCreated, nil
	case strings.HasPrefix(state, protocol.ExitedPrefix), strings.HasPrefix(state, protocol.FailedPrefix):
		return assignment.ObservedExited, nil
	default:
		return assignment.ObservedUnavailable, fmt.Errorf("capsule reports unrecognized state %q", state)
	}
}

// classifyContainerState decides what the daemon alone proves, and whether
// the supervisor still has to be asked. It is separated from the exec so
// the decision can be tested without a daemon: the defect this replaced
// was a short-circuit here, invisible to any test of the supervisor
// vocabulary because the supervisor was never reached.
//
// askSupervisor is true only while the container still runs, which is the
// only time an exec can succeed.
func classifyContainerState(runtimeID string, state docker.ContainerState) (obs assignment.ExecutionObservation, askSupervisor bool, err error) {
	switch state.Status {
	case "created":
		return assignment.ObservedCreated, false, nil
	case "exited", "dead":
		// A stopped capsule cannot be asked anything: exec needs a running
		// container and the control surface is tmpfs. The exit code is the
		// only account it left, which is why the supervisor reserves one
		// for "the runner never started".
		return ClassifyExit(state.ExitCode), false, nil
	case "running", "paused", "restarting":
		return assignment.ObservedUnavailable, true, nil
	default:
		return assignment.ObservedUnavailable, false,
			fmt.Errorf("capsule %s status %q does not prove whether execution began", runtimeID, state.Status)
	}
}
