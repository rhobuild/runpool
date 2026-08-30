package command

import (
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/rhobuild/runpool/internal/store"
	"github.com/spf13/cobra"
)

// Each constructor declares its positional-argument contract with Cobra.
// Command bodies receive typed flag values and return errors; Cobra remains
// the single parser and the source for generated CLI documentation.

func newVersionCommand(build BuildInfo, streams IO) *cobra.Command {
	var asJSON, checkRelease bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the build version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			operational(cmd)
			if checkRelease {
				// The release pipeline asks the binary whether its own
				// stamped version may be published. Asking the artifact
				// is the point: a check on a string somewhere else can
				// pass while the binary carries something different.
				if err := ValidReleaseVersion(build.Version); err != nil {
					return err
				}
				fmt.Fprintln(streams.Out, "release version accepted:", build.Version)
				return nil
			}
			if !asJSON {
				fmt.Fprintln(streams.Out, "runpool", build.Version)
				return nil
			}
			// Structured build facts for support and release evidence.
			enc := json.NewEncoder(streams.Out)
			enc.SetIndent("", "  ")
			return enc.Encode(map[string]any{
				"version":                         build.Version,
				"commit":                          build.Commit,
				"built":                           build.Built,
				"dirty":                           build.Dirty,
				"go":                              runtime.Version(),
				"platform":                        runtime.GOOS + "/" + runtime.GOARCH,
				"capsule_image":                   build.CapsuleImage,
				"release_qualification_reference": releaseQualificationReference(),
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the build facts as JSON")
	cmd.Flags().BoolVar(&checkRelease, "check-release", false,
		"fail unless this binary's version is a publishable SemVer release")
	return cmd
}

func newServeCommand(build BuildInfo) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the controller (configuration from the environment)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			operational(cmd)
			return runServe(build)
		},
	}
}

func newDoctorCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the host, storage and credentials",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			operational(cmd)
			return runDoctor(streamsOf(cmd), asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the report as JSON")
	return cmd
}

func newHealthcheckCommand() *cobra.Command {
	var mode string
	cmd := &cobra.Command{
		Use:   "healthcheck",
		Short: "Container health probe",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			operational(cmd)
			return runHealthcheck(streamsOf(cmd), mode)
		},
	}
	cmd.Flags().StringVar(&mode, "mode", "liveness", "liveness or readiness")
	return cmd
}

func newStatusCommand(build BuildInfo) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "What this instance owns, and whether the books agree with the daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			operational(cmd)
			return runStatus(streamsOf(cmd), asJSON, build.CapsuleImage)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the status as JSON")
	return cmd
}

func newAttemptsCommand() *cobra.Command {
	attempts := &cobra.Command{
		Use:   "attempts",
		Short: "The work held for a person, and how to decide it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	var (
		listJSON   bool
		listState  string
		listLimit  int
		listCursor string
	)
	list := &cobra.Command{
		Use:   "list",
		Short: "List attempts waiting for a decision",
		Long: "Lists attempts in deterministic FIFO order. Results are bounded by --limit; " +
			"pass the returned opaque cursor to continue without offset scans.\n\n" +
			"JSON output is an object with state, attempts, total and, when more rows exist, next_cursor.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			operational(cmd)
			return runAttemptsList(streamsOf(cmd), listState, listJSON, listLimit, listCursor)
		},
	}
	list.Flags().BoolVar(&listJSON, "json", false, "emit the list as JSON")
	list.Flags().IntVar(&listLimit, "limit", defaultAttemptPageSize,
		fmt.Sprintf("maximum attempts to return (1-%d)", store.MaxAttemptPageSize))
	list.Flags().StringVar(&listCursor, "cursor", "",
		"opaque cursor returned by the previous page")
	// The body has always served this flag; the tree never declared it,
	// so `--state` was an unknown flag while the reference generated
	// from the tree said nothing about it either way. Declaring it is
	// what makes the two agree.
	list.Flags().StringVar(&listState, "state", "manual-review",
		"attempt state to list: manual-review or ready")

	var inspectJSON bool
	inspect := &cobra.Command{
		Use:   "inspect <attempt-id>",
		Short: "Show one attempt and its lifecycle events",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			operational(cmd)
			return runAttemptsInspect(streamsOf(cmd), args[0], inspectJSON)
		},
	}
	inspect.Flags().BoolVar(&inspectJSON, "json", false, "emit the attempt as JSON")

	var (
		retry  bool
		settle bool
		reason string
		actor  string
		apply  bool
	)
	resolve := &cobra.Command{
		Use:   "resolve <attempt-id>",
		Short: "Decide a held attempt (preview by default)",
		Long: "Decides work Runpool could not decide alone.\n\n" +
			"--retry returns the attempt to the queue and it will run. Use it only\n" +
			"after verifying outside Runpool — in the provider's own UI or API —\n" +
			"that the workload never executed.\n\n" +
			"--settle-may-have-run closes the attempt as possibly executed. It\n" +
			"never runs again. Use it when execution cannot be ruled out.\n\n" +
			"Without --apply this is a preview.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			operational(cmd)
			return runAttemptsResolve(streamsOf(cmd), args[0], retry, settle, reason, actor, apply)
		},
	}
	resolve.Flags().BoolVar(&retry, "retry", false,
		"return the attempt to the queue (verify externally that it never executed first)")
	resolve.Flags().BoolVar(&settle, "settle-may-have-run", false,
		"close the attempt as possibly executed; it never runs again")
	resolve.Flags().StringVar(&reason, "reason", "", "why this decision is correct (required)")
	// No default here: the flag's default is baked into the generated
	// reference, and $USER at generation time would ship whoever ran
	// the generator. The command resolves it at run time instead.
	resolve.Flags().StringVar(&actor, "actor", "", "who is deciding (default: $USER)")
	resolve.Flags().BoolVar(&apply, "apply", false, "perform the resolution (default is a preview)")

	attempts.AddCommand(list, inspect, resolve)
	return attempts
}

func newGCCommand() *cobra.Command {
	var apply, aggressive bool
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Collect cache lanes and finished lease records (dry run by default)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			operational(cmd)
			return runGC(streamsOf(cmd), apply, aggressive)
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "perform the evictions (default is a dry run)")
	cmd.Flags().BoolVar(&aggressive, "aggressive", false, "plan every free lane, not just expired and over-budget ones")
	return cmd
}

func newCleanupCommand() *cobra.Command {
	var apply bool
	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Remove resources no live lease needs (dry run by default)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			operational(cmd)
			return runCleanup(streamsOf(cmd), apply)
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "perform the removals (default is a dry run)")
	return cmd
}

func newUninstallCommand() *cobra.Command {
	var (
		confirm         string
		deleteScaleSets bool
	)
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove everything this instance owns",
		Long: "Removes every container, network and volume this instance owns,\n" +
			"including cache lanes. The state volume is left for you to remove.\n" +
			"Without --confirm it is a dry run and prints the exact command.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			operational(cmd)
			return runUninstall(streamsOf(cmd), confirm, deleteScaleSets)
		},
	}
	cmd.Flags().StringVar(&confirm, "confirm", "", "the instance id being uninstalled")
	cmd.Flags().BoolVar(&deleteScaleSets, "delete-scale-sets", false, "also delete this instance's scale sets from the provider")
	return cmd
}

func newConfigCommand() *cobra.Command {
	config := &cobra.Command{
		Use:   "config",
		Short: "Validate and inspect the configuration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	var validateFile string
	validate := &cobra.Command{
		Use:   "validate",
		Short: "Validate a configuration file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			operational(cmd)
			return runConfigValidate(streamsOf(cmd), validateFile)
		},
	}
	validate.Flags().StringVar(&validateFile, "file", "", "path to the configuration file")

	// The schema carries secret references — environment variable names,
	// file paths — and never secret values, so there is nothing to
	// redact. The flag is accepted and does nothing, which the help text
	// says outright rather than implying a redaction happened.
	var (
		effectiveFile string
		redacted      bool
	)
	effective := &cobra.Command{
		Use:   "effective",
		Short: "Print the defaulted, validated configuration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			operational(cmd)
			return runConfigEffective(streamsOf(cmd), effectiveFile)
		},
	}
	effective.Flags().StringVar(&effectiveFile, "file", "", "path to the configuration file (default: the environment)")
	effective.Flags().BoolVar(&redacted, "redacted", false,
		"accepted for forward compatibility; the schema holds secret references, never secret values, so nothing is redacted")
	_ = redacted

	config.AddCommand(validate, effective)
	return config
}
