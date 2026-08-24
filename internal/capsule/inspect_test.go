package capsule

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/engine"
)

// TestAnUndecidableExecutionIsHeldRatherThanSettled: absence ends an
// attempt and unreachability does not, and everything the daemon has no
// name for is unreachability.
//
// The distinction decides whether work is settled or held for a person,
// and it is the one a classification can quietly narrow: give the daemon
// three named answers and it is tempting to let the unnamed ones fall
// somewhere convenient. They fall to undecided, which is the only answer
// that cannot lose a job — and no live daemon can be asked to produce
// them, which is why this reaches it through a seam.
func TestAnUndecidableExecutionIsHeldRatherThanSettled(t *testing.T) {
	prepared := PreparedRuntime{RuntimeID: assignment.RuntimeID("runtime-1")}

	for name, testCase := range map[string]struct {
		err     error
		want    assignment.ExecutionObservation
		reports bool
	}{
		"a container that is gone ended": {
			err:  fmt.Errorf("inspect: %w", engine.ErrNotFound),
			want: assignment.ObservedAbsent,
		},
		"a daemon that cannot be reached decides nothing": {
			err:     fmt.Errorf("inspect: %w", engine.ErrUnavailable),
			want:    assignment.ObservedUnavailable,
			reports: true,
		},
		"an answer with no name decides nothing either": {
			err:     errors.New("i/o timeout"),
			want:    assignment.ObservedUnavailable,
			reports: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			launcher := &Launcher{dock: &fakeDaemon{
				status: func(string) (engine.ContainerState, error) {
					return engine.ContainerState{}, testCase.err
				},
			}}
			got, err := launcher.InspectExecution(t.Context(), prepared)
			if got != testCase.want {
				t.Errorf("observed %q; want %q", got, testCase.want)
			}
			if testCase.reports && err == nil {
				t.Error("an undecided observation must carry the reason it is undecided")
			}
			if !testCase.reports && err != nil {
				t.Errorf("a decided observation carried an error: %v", err)
			}
		})
	}
}

// TestAnUnreadableStateSaysWhyItCouldNotBeRead: this inspection is what
// an ambiguous start is held on, and the operator deciding about that
// attempt reads its error. A transport failure leaves the exec's output
// empty, so reporting the exit code alone handed them a page with no
// reason on it — "capsule state unreadable (exit -1): " and nothing more.
func TestAnUnreadableStateSaysWhyItCouldNotBeRead(t *testing.T) {
	boom := errors.New("daemon connection reset")
	m := &Launcher{dock: &fakeDaemon{
		status: func(string) (engine.ContainerState, error) {
			return engine.ContainerState{Status: engine.StatusRunning}, nil
		},
		exec: func(string, []string) (int, string, error) { return -1, "", boom },
	}}

	obs, err := m.InspectExecution(t.Context(), PreparedRuntime{RuntimeID: "runner-1"})
	if obs != assignment.ObservedUnavailable {
		t.Errorf("observation = %s; an unreadable state proves nothing either way", obs)
	}
	if !errors.Is(err, boom) {
		t.Errorf("error = %v; the cause has to travel, or the page names no reason", err)
	}
}

// TestACapsuleThatDiedIsRefusedAsIncompatible: a tier may name its own
// capsule image, and one whose entrypoint crashes writes no protocol
// file — ever. Waiting out the declaration deadline for it spent thirty
// seconds per attempt and then reported a read failure, which is not the
// incompatibility the caller holds on: the tier retried the same broken
// image until its budget ran out, under a reason that named none of it.
func TestACapsuleThatDiedIsRefusedAsIncompatible(t *testing.T) {
	m := &Launcher{dock: &fakeDaemon{
		status: func(string) (engine.ContainerState, error) {
			return engine.ContainerState{Status: engine.StatusExited, ExitCode: 127}, nil
		},
		exec: func(string, []string) (int, string, error) {
			return -1, "", errors.New("container is not running")
		},
	}}

	start := time.Now()
	err := m.awaitProtocol(t.Context(), "runner-1")
	if !errors.Is(err, ErrIncompatibleImage) {
		t.Fatalf("error = %v; want ErrIncompatibleImage, which is what holds the attempt "+
			"instead of retrying the same image", err)
	}
	if !strings.Contains(err.Error(), "127") {
		t.Errorf("error = %q; it has to name the exit code, which is the one fact "+
			"an operator can act on", err)
	}
	if elapsed := time.Since(start); elapsed > protocolTimeout/2 {
		t.Errorf("refusing a dead capsule took %s against a %s deadline; the point is "+
			"not spending it", elapsed.Round(time.Millisecond), protocolTimeout)
	}
}
