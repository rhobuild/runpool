package capsule

import (
	"testing"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/platform/docker"
)

// TestClassifySupervisorStateSeparatesAbortFromExit is the job-loss guard.
// `aborted` and `failed` both mean the supervisor stopped early, but only one
// of them means the runner had the job. Reading an abort as an exit records
// exit_observed, which disposes the attempt as completed and never requeues
// it — a job GitHub handed over, that never ran, and that nobody retries.
func TestClassifySupervisorStateSeparatesAbortFromExit(t *testing.T) {
	for state, want := range map[string]assignment.ExecutionObservation{
		"waiting": assignment.ObservedCreated,
		"running": assignment.ObservedRunning,
		"aborted:start authorized but no credential": assignment.ObservedCreated,
		"aborted:prepare volatile runner config":     assignment.ObservedCreated,
		"exited:0":                                   assignment.ObservedExited,
		"exited:1":                                   assignment.ObservedExited,
		"failed:runner killed after drain timeout":   assignment.ObservedExited,
	} {
		got, err := classifySupervisorState(state)
		if err != nil {
			t.Errorf("state %q: %v", state, err)
			continue
		}
		if got != want {
			t.Errorf("state %q classified %q; want %q", state, got, want)
		}
	}
}

// An unknown state refuses to guess rather than settling the attempt on an
// assumption: the whole point of the observation is that it is observed.
func TestClassifySupervisorStateRefusesToGuess(t *testing.T) {
	for _, state := range []string{"", "wat", "exited", "aborted", "done:0"} {
		got, err := classifySupervisorState(state)
		if err == nil {
			t.Errorf("state %q classified %q; want an error", state, got)
		}
		if got != assignment.ObservedUnavailable {
			t.Errorf("state %q = %q; want unavailable", state, got)
		}
	}
}

// TestClassifySupervisorExitSeparatesAbortFromExit is the job-loss guard at
// the only place it can still be applied. A stopped capsule cannot be
// asked anything — exec needs a running container and the control surface
// is tmpfs — so the state file that distinguishes an abort is already gone
// when the controller looks. The reserved exit code is what survives.
func TestClassifySupervisorExitSeparatesAbortFromExit(t *testing.T) {
	if got := classifySupervisorExit(SupervisorAbortedExitCode); got != assignment.ObservedCreated {
		t.Errorf("the reserved abort code classified %q; want an unstarted runtime", got)
	}
	// Every other code means the runner owned the job, a failed job
	// included: that is an execution outcome, not an unstarted runtime.
	for _, code := range []int{0, 1, 2, 78, 80, 125, 137, 255, -1} {
		if got := classifySupervisorExit(code); got != assignment.ObservedExited {
			t.Errorf("exit code %d classified %q; want an execution outcome", code, got)
		}
	}
}

// TestClassifyContainerStateNeverAsksAStoppedCapsule is the shape of the
// defect this replaced: an exited container short-circuited to
// assignment.ObservedExited before the supervisor was ever consulted, so an abort
// could not be seen no matter what vocabulary the supervisor used. The
// daemon's own facts have to carry that distinction, because a stopped
// container cannot be asked.
func TestClassifyContainerStateNeverAsksAStoppedCapsule(t *testing.T) {
	for _, tc := range []struct {
		status        string
		exit          int
		want          assignment.ExecutionObservation
		askSupervisor bool
	}{
		{"created", 0, assignment.ObservedCreated, false},
		{"running", 0, assignment.ObservedUnavailable, true},
		{"paused", 0, assignment.ObservedUnavailable, true},
		{"restarting", 0, assignment.ObservedUnavailable, true},
		{"exited", 0, assignment.ObservedExited, false},
		{"exited", 1, assignment.ObservedExited, false},
		{"exited", SupervisorAbortedExitCode, assignment.ObservedCreated, false},
		{"dead", SupervisorAbortedExitCode, assignment.ObservedCreated, false},
	} {
		got, ask, err := classifyContainerState("c1", docker.ContainerState{Status: tc.status, ExitCode: tc.exit})
		if err != nil {
			t.Errorf("status %q exit %d: %v", tc.status, tc.exit, err)
			continue
		}
		if ask != tc.askSupervisor {
			t.Errorf("status %q: askSupervisor = %v; want %v", tc.status, ask, tc.askSupervisor)
		}
		if !tc.askSupervisor && got != tc.want {
			t.Errorf("status %q exit %d classified %q; want %q", tc.status, tc.exit, got, tc.want)
		}
	}

	// An unknown status refuses to guess rather than settling an attempt.
	if _, ask, err := classifyContainerState("c1", docker.ContainerState{Status: "removing"}); err == nil || ask {
		t.Error("an unrecognised status must be an error, not a supervisor query")
	}
}
