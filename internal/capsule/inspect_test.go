package capsule

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/engine"
	"unicode/utf8"
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

// TestACapsuleThatDiedSaysWhy: the exit code is what an operator can act
// on only if it names something. 79 does not.
//
// The case this was written for is an operator's derived capsule image
// ending `USER runner`. The published capsule stays root deliberately —
// the supervisor is PID 1, boots the inner daemon and drops the runner
// to uid 1001 itself — and the control surface it writes is a root-owned
// tmpfs this launcher mounts. Under any other user the supervisor's
// first write fails, the abort it would have written fails onto the same
// unwritable tmpfs, and what is left is a container that exited 79 and
// an error reading "the capsule image and this controller are not a
// pair" — which sends an operator to re-check a digest that was correct.
//
// The permission denial was in the container log the whole time.
func TestACapsuleThatDiedSaysWhy(t *testing.T) {
	const denial = `level=ERROR msg="capsule boot failed" error="mkdir /run/runpool-docker: permission denied"`
	m := &Launcher{dock: &fakeDaemon{
		status: func(string) (engine.ContainerState, error) {
			return engine.ContainerState{Status: engine.StatusExited, ExitCode: SupervisorAbortedExitCode}, nil
		},
		exec: func(string, []string) (int, string, error) {
			return -1, "", errors.New("container is not running")
		},
		logs: func(string, int) (string, error) {
			return "level=INFO msg=\"capsule starting\"\n" + denial + "\n", nil
		},
	}}

	err := m.awaitProtocol(t.Context(), "runner-1")
	if !errors.Is(err, ErrIncompatibleImage) {
		t.Fatalf("error = %v; want ErrIncompatibleImage", err)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error = %q; it has to carry what the capsule said, or the only "+
			"account of a derived image that cannot write its own control surface "+
			"is an exit code that names nothing", err)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("error = %q; it is recorded as the evidence beside a held attempt, "+
			"and a multi-line value there is one field that reads as several", err)
	}
}

// TestACapsuleWhoseLogsCannotBeReadStillReportsTheExit: the tail is an
// addition to a diagnosis and never the diagnosis. A daemon that will
// not answer for the logs must not cost the error that sent us here —
// the path is already failing, and this is the second thing to fail on
// it.
func TestACapsuleWhoseLogsCannotBeReadStillReportsTheExit(t *testing.T) {
	m := &Launcher{dock: &fakeDaemon{
		status: func(string) (engine.ContainerState, error) {
			return engine.ContainerState{Status: engine.StatusExited, ExitCode: 127}, nil
		},
		exec: func(string, []string) (int, string, error) {
			return -1, "", errors.New("container is not running")
		},
		logs: func(string, int) (string, error) {
			return "", errors.New("daemon is unreachable")
		},
	}}

	err := m.awaitProtocol(t.Context(), "runner-1")
	if !errors.Is(err, ErrIncompatibleImage) || !strings.Contains(err.Error(), "127") {
		t.Fatalf("error = %v; want the exit code and ErrIncompatibleImage, unchanged "+
			"by a log read that failed", err)
	}
	if strings.Contains(err.Error(), "last said") {
		t.Errorf("error = %q; a log that could not be read has nothing to add, and "+
			"an empty quotation reads as a capsule that said nothing", err)
	}
}

// TestACapsuleThatSaidTooMuchIsCutOff.
//
// The daemon's tail bounds newline-delimited records, not bytes, so a
// capsule that wrote one unterminated line makes "the last five lines"
// the whole log it has written. That reaches this process's memory and
// one structured log record, and neither wants a megabyte.
//
// Cut from the end, because the last thing a dying process said is the
// reason it died.
func TestACapsuleThatSaidTooMuchIsCutOff(t *testing.T) {
	const reason = "mkdir /run/runpool-docker: permission denied"
	m := &Launcher{dock: &fakeDaemon{
		status: func(string) (engine.ContainerState, error) {
			return engine.ContainerState{Status: engine.StatusExited, ExitCode: SupervisorAbortedExitCode}, nil
		},
		exec: func(string, []string) (int, string, error) {
			return -1, "", errors.New("container is not running")
		},
		logs: func(string, int) (string, error) {
			// One line, no newline in it, far past the ceiling.
			return strings.Repeat("x", 64<<10) + " " + reason, nil
		},
	}}

	err := m.awaitProtocol(t.Context(), "runner-1")
	if err == nil {
		t.Fatal("a dead capsule was reported as still declaring a protocol")
	}
	if len(err.Error()) > logTailBytes+512 {
		t.Errorf("the error is %d bytes; one unterminated line put the whole log in it, "+
			"and this lands in memory and in a log record", len(err.Error()))
	}
	if !strings.Contains(err.Error(), reason) {
		t.Errorf("error = %q; cutting from the end is what keeps the last thing the "+
			"capsule said, which is the reason it died", err)
	}
}

// TestACapsuleCutOffStillReadsAsText: the cap is in bytes and a log is
// UTF-8, so a cut that lands mid-character leaves an invalid prefix.
// This string becomes a field in a structured log record, where that is
// either replacement characters or the encoder's problem -- and either
// way an operator reads mojibake instead of the reason the capsule died.
func TestACapsuleCutOffStillReadsAsText(t *testing.T) {
	const reason = "permiso denegado al crear /run/runpool-docker"
	m := &Launcher{dock: &fakeDaemon{
		status: func(string) (engine.ContainerState, error) {
			return engine.ContainerState{Status: engine.StatusExited, ExitCode: SupervisorAbortedExitCode}, nil
		},
		exec: func(string, []string) (int, string, error) {
			return -1, "", errors.New("container is not running")
		},
		logs: func(string, int) (string, error) {
			// Multi-byte throughout, so almost every byte offset is
			// inside a character rather than between two.
			return strings.Repeat("ñ", 4096) + " " + reason, nil
		},
	}}

	err := m.awaitProtocol(t.Context(), "runner-1")
	if err == nil {
		t.Fatal("a dead capsule was reported as still declaring a protocol")
	}
	if !utf8.ValidString(err.Error()) {
		t.Errorf("the error is not valid UTF-8; the cut landed inside a character: %q", err.Error())
	}
	if !strings.Contains(err.Error(), reason) {
		t.Errorf("error = %q; the reason the capsule died is the part that has to survive", err)
	}
}
