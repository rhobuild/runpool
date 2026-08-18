// Package command is the runpool CLI surface.
//
// Cobra owns parsing, help, completion, and generated reference data. Every
// command declares its positional-argument contract. Usage is printed for
// parse failures and omitted for operational failures.
package command

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

// Exit codes: 0 success, 1 operational failure, 2 usage error.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// usageError marks a failure the caller could have avoided by typing
// something else, which is the only kind that earns the usage text.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

func usagef(format string, args ...any) error {
	return usageError{fmt.Errorf(format, args...)}
}

// IO is where a command reads and writes. Tests supply buffers; main
// supplies the process streams. Commands never reach for os.Stdout
// directly, or their output could not be asserted on.
type IO struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

// streamsOf is where a command body gets its writers. Execute points the
// root at the caller's IO and Cobra carries them down the tree, so a
// test asserts on buffers and main writes to the process streams without
// any command body knowing which of the two it got.
func streamsOf(cmd *cobra.Command) IO {
	return IO{In: cmd.InOrStdin(), Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr()}
}

// BuildInfo is what `version` reports: stamped at build time, never
// guessed at run time.
type BuildInfo struct {
	Version      string
	Commit       string
	Built        string
	Dirty        bool
	CapsuleImage string
}

// Execute runs one CLI invocation and returns its exit code.
func Execute(args []string, build BuildInfo, streams IO) int {
	root := NewRootCommand(build, streams)
	root.SetArgs(args)
	err := root.Execute()
	return exitCodeFor(err, root, streams)
}

// Run is Execute over the process streams, with the build facts the
// toolchain recorded filled in around whatever the linker stamped.
func Run(args []string, build BuildInfo) int {
	return Execute(args, BuildInfoFromDebug(build), IO{
		In: os.Stdin, Out: os.Stdout, Err: os.Stderr,
	})
}

// exitCodeFor maps one error to one exit code, in one place. Scattering
// this decision is how a command ends up reporting a usage problem as
// an operational failure, which tells a script the wrong thing.
func exitCodeFor(err error, root *cobra.Command, streams IO) int {
	if err == nil {
		return exitOK
	}
	var usage usageError
	if errors.As(err, &usage) {
		fmt.Fprintln(streams.Err, "runpool:", usage.Error())
		return exitUsage
	}
	// Cobra's own parse failures — unknown command, unknown flag, wrong
	// argument count. SilenceErrors keeps Cobra from printing them, so
	// they are printed here: an unknown command that produced no output
	// at all leaves a person with an exit code and no idea why.
	if isCobraUsageError(err) {
		fmt.Fprintln(streams.Err, "runpool:", err)
		fmt.Fprintf(streams.Err, "run 'runpool --help' for usage.\n")
		return exitUsage
	}
	// A command that already reported its own failure says so by
	// returning errSilent; printing again would double the message.
	if !errors.Is(err, errSilent) {
		fmt.Fprintln(streams.Err, "runpool:", err)
	}
	return exitError
}

// isCobraUsageError distinguishes a parse failure from an operational
// one. Cobra has no error type for this, so the check is on the text it
// produces; a miss costs an exit code of 1 instead of 2, never a
// success reported for a failure.
func isCobraUsageError(err error) bool {
	for _, marker := range []string{
		"unknown command",
		"unknown flag",
		"unknown shorthand flag",
		"invalid argument",
		"accepts ",
		"requires at least",
		"requires exactly",
		"flag needs an argument",
	} {
		if containsFold(err.Error(), marker) {
			return true
		}
	}
	return false
}

func containsFold(s, sub string) bool {
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if equalFold(s[i:i+len(sub)], sub) {
			return true
		}
	}
	return false
}

func equalFold(a, b string) bool {
	for i := range a {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// NewRootCommand builds the whole tree. It takes its dependencies so a
// test can drive every command without a process.
func NewRootCommand(build BuildInfo, streams IO) *cobra.Command {
	root := &cobra.Command{
		Use:   "runpool",
		Short: "Docker-native autoscaling for ephemeral GitHub Actions runners",
		Long: "Runpool coordinates GitHub Actions scale-set demand against a finite\n" +
			"pool of per-job execution capsules on one Docker host.",
		SilenceErrors: true,
		// Usage is printed for parse failures, which Cobra decides
		// before RunE is reached; each command silences it once it is
		// running, so an operational failure shows the error alone.
		SilenceUsage: false,
		// A bare `runpool` is not an error: it is someone asking what
		// this is. Print help and succeed.
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	root.SetIn(streams.In)
	root.SetOut(streams.Out)
	root.SetErr(streams.Err)
	root.CompletionOptions.HiddenDefaultCmd = false

	root.AddCommand(
		newVersionCommand(build, streams),
		newServeCommand(build),
		newDoctorCommand(),
		newHealthcheckCommand(),
		newStatusCommand(build),
		newAttemptsCommand(),
		newGCCommand(),
		newCleanupCommand(),
		newUninstallCommand(),
		newConfigCommand(),
	)
	return root
}

// operational marks a command as past parsing: from here a failure is
// the world's, not the caller's, so the usage text would be noise.
func operational(cmd *cobra.Command) { cmd.SilenceUsage = true }

// errSilent is a failure whose detail a command already wrote to its
// own output — the doctor's per-check lines, a gc pass's per-eviction
// errors. Returning it sets the exit code without printing a second,
// emptier version of the same news.
var errSilent = errors.New("")
