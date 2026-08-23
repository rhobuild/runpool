package capsule

import (
	"errors"
	"fmt"
	"testing"

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
