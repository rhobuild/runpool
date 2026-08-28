package consistency

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestTheRetryHelperNeverPassesWithoutRunning is the test that should
// have existed three regressions ago.
//
// scripts/ci/retry.sh has exactly one job an operator relies on: report
// the truth about a command it was given. It has failed that three times,
// each in a way that reported success for work that had not been done.
// `$?` after an `if` whose condition failed is the `if`'s own zero, so
// giving up exited 0. `seq 1 0` prints nothing, so the loop never ran and
// the script fell off the end at 0. `seq 1 00` does the same, because a
// check for the string "0" is not a check for the value zero.
//
// Every one was found by reading, after the fact, one at a time. Nothing
// ran the script. So this does: the invariant is not any of those three
// bugs, it is that no input makes the helper exit 0 without the command
// having run, and no input makes it exit 0 for a command that failed.
func TestTheRetryHelperNeverPassesWithoutRunning(t *testing.T) {
	helper := repoPath("scripts", "ci", "retry.sh")
	if _, err := os.Stat(helper); err != nil {
		t.Fatalf("%s is missing: %v", helper, err)
	}

	for name, tc := range map[string]struct {
		attempts string
		pause    string
		exit     int // the status the wrapped command exits with
		noArgs   bool
		wantExit int
		wantRuns int
	}{
		"a command that works runs once and passes": {exit: 0, wantExit: 0, wantRuns: 1},
		"a command that fails carries its own code": {exit: 7, wantExit: 7, wantRuns: 3},
		"one attempt is one attempt":                {attempts: "1", exit: 7, wantExit: 7, wantRuns: 1},
		"the bound is honoured":                     {attempts: "5", exit: 3, wantExit: 3, wantRuns: 5},
		"zero attempts refuses rather than passing": {attempts: "0", exit: 0, wantExit: 2, wantRuns: 0},
		"and so does zero spelled with two digits":  {attempts: "00", exit: 0, wantExit: 2, wantRuns: 0},
		"and three":                                 {attempts: "000", exit: 0, wantExit: 2, wantRuns: 0},
		"a count that is not a number refuses":      {attempts: "abc", exit: 0, wantExit: 2, wantRuns: 0},
		"a count with whitespace refuses":           {attempts: " 3", exit: 0, wantExit: 2, wantRuns: 0},
		"a count past the bound refuses":            {attempts: "11", exit: 0, wantExit: 2, wantRuns: 0},
		"a count that would hang refuses":           {attempts: "99999999999999999999", exit: 0, wantExit: 2, wantRuns: 0},
		"a pause that is not a number refuses":      {pause: "soon", exit: 0, wantExit: 2, wantRuns: 0},
		"no command at all refuses rather than 0":   {noArgs: true, wantExit: 2, wantRuns: 0},
		"a leading zero is read as base ten, not 8": {attempts: "08", exit: 0, wantExit: 0, wantRuns: 1},
	} {
		t.Run(name, func(t *testing.T) {
			// The wrapped command records every invocation, so "did it
			// run" is measured rather than inferred from the exit status
			// -- which is the exact inference each of the three bugs
			// made look right.
			ledger := filepath.Join(t.TempDir(), "runs")
			cmd := exec.Command(helper, "sh", "-c",
				"echo ran >> "+ledger+"; exit "+strconv.Itoa(tc.exit))
			if tc.noArgs {
				cmd = exec.Command(helper)
			}
			cmd.Env = append(os.Environ(), "RETRY_PAUSE=0")
			if tc.attempts != "" {
				cmd.Env = append(cmd.Env, "RETRY_ATTEMPTS="+tc.attempts)
			}
			if tc.pause != "" {
				cmd.Env = append(cmd.Env, "RETRY_PAUSE="+tc.pause)
			}
			out, err := cmd.CombinedOutput()

			got := 0
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				got = exitErr.ExitCode()
			} else if err != nil {
				t.Fatalf("running the helper: %v", err)
			}
			if got != tc.wantExit {
				t.Errorf("exit = %d, want %d\n%s", got, tc.wantExit, out)
			}

			runs := 0
			if body, err := os.ReadFile(ledger); err == nil {
				runs = strings.Count(string(body), "ran")
			}
			if runs != tc.wantRuns {
				t.Errorf("the command ran %d times, want %d\n%s", runs, tc.wantRuns, out)
			}
			// The invariant, stated once: a zero means the work was done.
			if got == 0 && runs == 0 {
				t.Errorf("the helper reported success for a command that never ran, " +
					"which is the failure it exists to prevent")
			}
		})
	}
}

// TestTheRetryHelperKeepsTryingUntilSomethingWorks: the other half. A
// helper that refuses bad input correctly but never actually retries
// would pass every case above.
func TestTheRetryHelperKeepsTryingUntilSomethingWorks(t *testing.T) {
	helper := repoPath("scripts", "ci", "retry.sh")
	ledger := filepath.Join(t.TempDir(), "runs")

	// Fails on the first two attempts and works on the third, by
	// counting its own invocations.
	cmd := exec.Command(helper, "sh", "-c",
		"echo ran >> "+ledger+"; [ $(wc -l < "+ledger+") -ge 3 ]")
	cmd.Env = append(os.Environ(), "RETRY_PAUSE=0", "RETRY_ATTEMPTS=3")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("a command that works on the third attempt was not retried into "+
			"success: %v\n%s", err, out)
	}
	body, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if runs := strings.Count(string(body), "ran"); runs != 3 {
		t.Errorf("the command ran %d times; want exactly 3 — fewer means it gave up "+
			"early, more means it kept going after success", runs)
	}
	if !strings.Contains(string(out), "attempt 3") {
		t.Errorf("output = %q; a retry that succeeded has to say which attempt it was, "+
			"or three identical errors are indistinguishable from one", out)
	}
}
