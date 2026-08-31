package consistency

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestEveryShellScriptDeclaresItsExecutionContract(t *testing.T) {
	for _, file := range tracked(t, "*.sh") {
		path := repoPath(file)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		if !strings.HasPrefix(text, "#!/usr/bin/env bash\nset -euo pipefail\n") {
			t.Errorf("%s must start with the repository Bash shebang and strict mode", file)
		}
		if !strings.Contains(text, "# Usage:") {
			t.Errorf("%s states no usage contract", file)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s is not executable (mode %v)", file, info.Mode().Perm())
		}
	}
}

func TestCoverageHelperRejectsAnInvalidFloor(t *testing.T) {
	helper := repoPath("scripts", "verify", "coverage.sh")
	for _, value := range []string{"", "none", "-1", "101", "55%", "1.2.3"} {
		value := value
		t.Run(value, func(t *testing.T) {
			cmd := exec.Command(helper, "profile-that-must-not-be-read")
			cmd.Env = append(os.Environ(), "RUNPOOL_COVERAGE_MIN="+value)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 2 {
				t.Fatalf("exit = %v, output = %q; want usage exit 2", err, output)
			}
			if !strings.Contains(string(output), "between 0 and 100") {
				t.Errorf("output = %q; want the accepted range", output)
			}
		})
	}
}
