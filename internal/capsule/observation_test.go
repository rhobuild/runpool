package capsule

import (
	"errors"
	"strings"
	"testing"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/capsule/protocol"
	"github.com/rhobuild/runpool/internal/platform/docker"
)

// TestClassifySupervisorStateSeparatesAbortFromExit is the job-loss guard.
// `aborted` and `failed` both mean the supervisor stopped early, but only one
// of them means the runner had the job. Reading an abort as an exit records
// exit_observed, which disposes the attempt as completed and never requeues
// it — a job GitHub handed over, that never ran, and that nobody retries.
func TestClassifySupervisorStateSeparatesAbortFromExit(t *testing.T) {
	for state, want := range map[string]assignment.ExecutionObservation{
		"waiting":  assignment.ObservedCreated,
		"starting": assignment.ObservedUnavailable,
		"running":  assignment.ObservedRunning,
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
	if got := ClassifyExit(SupervisorAbortedExitCode); got != assignment.ObservedCreated {
		t.Errorf("the reserved abort code classified %q; want an unstarted runtime", got)
	}
	// Every other code means the runner owned the job, a failed job
	// included: that is an execution outcome, not an unstarted runtime.
	for _, code := range []int{0, 1, 2, 78, 80, 125, 137, 255, -1} {
		if got := ClassifyExit(code); got != assignment.ObservedExited {
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

// TestAwaitStateGivesUpOnEveryTerminalState: a container that has
// reached a state it can never leave is done being polled.
//
// The reason a supervisor writes beside a terminal state is the only
// account of what went wrong, and it dies with the container. Treating
// one as "not the state I wanted, keep asking" spends the whole
// readiness deadline and then reports a timeout — the operator gets
// "did not reach waiting in 90s" instead of "dockerd did not become
// ready in time", and the wait is burned as well as the reason.
func TestAwaitStateGivesUpOnEveryTerminalState(t *testing.T) {
	const want = protocol.StateWaiting
	for name, tc := range map[string]struct {
		code     int
		out      string
		execErr  error
		wantDone bool
		wantErr  string // substring; empty means done with no error, or not done
	}{
		"the wanted state":          {0, want, nil, true, ""},
		"trailing newline":          {0, want + "\n", nil, true, ""},
		"still booting":             {0, protocol.StateBooting, nil, false, ""},
		"aborted before the runner": {0, protocol.AbortedPrefix + " dockerd did not become ready in time", nil, true, "dockerd did not become ready"},
		"failed after the runner":   {0, protocol.FailedPrefix + " runner exploded", nil, true, "runner exploded"},
		"already exited":            {0, protocol.ExitedPrefix + "0", nil, true, "exited:0"},
		"the exec could not run":    {0, "", errors.New("daemon unreachable"), false, ""},
		"the exec reported failure": {1, "no such file", nil, false, ""},
	} {
		t.Run(name, func(t *testing.T) {
			done, err := stateVerdict(want, tc.code, tc.out, tc.execErr)
			if done != tc.wantDone {
				t.Fatalf("done = %v, want %v (err %v)", done, tc.wantDone, err)
			}
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr == "":
				return
			case err == nil:
				t.Fatalf("no error; want one containing %q", tc.wantErr)
			case !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestOnlyWaitingProvesTheRunnerNeverStarted: the launcher requeues an
// assignment on exactly one answer, so no other state may reach it.
//
// `starting` is the near miss. It sits between an authorization landing
// and fork/exec returning, and for that stretch the capsule genuinely
// cannot say whether the job was handed over. Classifying it with
// `waiting` reads "not yet" as "never", and the assignment is served a
// second time while the first capsule starts its runner.
func TestOnlyWaitingProvesTheRunnerNeverStarted(t *testing.T) {
	requeues := map[string]bool{}
	for _, state := range []string{
		protocol.StateBooting, protocol.StateWaiting, protocol.StateStarting,
		protocol.StateRunning, protocol.AbortedPrefix + "no credential",
		protocol.ExitedPrefix + "0", protocol.FailedPrefix + "drained",
	} {
		obs, err := classifySupervisorState(state)
		if err != nil {
			t.Fatalf("state %q: %v", state, err)
		}
		requeues[state] = obs == assignment.ObservedCreated
	}
	if requeues[protocol.StateStarting] {
		t.Error("a capsule that has accepted a start authorization is classified as one that " +
			"never started; the assignment is requeued while the runner is being forked")
	}
	for _, proves := range []string{protocol.StateBooting, protocol.StateWaiting,
		protocol.AbortedPrefix + "no credential"} {
		if !requeues[proves] {
			t.Errorf("state %q no longer requeues; a job that was never handed over is left "+
				"for a person or settled as though it ran", proves)
		}
	}
}
