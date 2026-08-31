// Command suite-evidence converts one completed qualification boundary into
// the common manifest consumed by the release-record assembler.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rhobuild/runpool/internal/platform"
	"github.com/rhobuild/runpool/internal/qualification"
)

type caseFlags []string

func (values *caseFlags) String() string { return strings.Join(*values, ",") }
func (values *caseFlags) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func main() {
	var (
		suiteID       = flag.String("suite", "", "qualification suite identity")
		commit        = flag.String("commit", "", "qualified Git commit")
		run           = flag.String("run", "", "qualification run URL")
		started       = flag.String("started-at", "", "suite start time in RFC3339")
		completed     = flag.String("completed-at", "", "suite completion time in RFC3339")
		environment   = flag.String("environment", "", "github-hosted or release-platform")
		runner        = flag.String("runner", "", "runner selector")
		platformPath  = flag.String("platform-facts", "", "observed platform facts JSON")
		controller    = flag.String("controller-image", "", "digest-qualified controller image")
		capsule       = flag.String("capsule-image", "", "digest-qualified capsule image")
		logPath       = flag.String("log", "", "test or drill log")
		logFormat     = flag.String("log-format", "go", "go, drill, go-then-cases, or cases")
		explicitCases caseFlags
	)
	flag.Var(&explicitCases, "case", "case result as id=passed (repeatable; log-format=cases)")
	flag.Parse()

	startTime := parseTime("started-at", *started)
	completedTime := parseTime("completed-at", *completed)
	build := qualification.Build{
		Commit: *commit, Run: *run, ControllerImage: *controller, CapsuleImage: *capsule,
	}
	env := qualification.ExecutionEnvironment{
		Kind: qualification.EnvironmentKind(*environment), Runner: *runner,
	}
	var observed platform.Facts
	if *platformPath != "" {
		readJSON(*platformPath, &observed)
		env.Facts = &observed
	}

	cases, err := readCases(*logFormat, *logPath, explicitCases)
	if err != nil {
		fail(err)
	}
	manifest, err := qualification.NewSuiteEvidence(
		qualification.SuiteID(*suiteID), build, env, startTime, completedTime, cases,
	)
	if err != nil {
		fail(err)
	}
	if err := qualification.ValidateSuiteEvidence(manifest, build, observed); err != nil {
		fail(err)
	}
	if err := qualification.EncodeSuiteEvidence(os.Stdout, manifest); err != nil {
		fail(err)
	}
}

func parseTime(name, value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		fail(fmt.Errorf("parse -%s %q: %w", name, value, err))
	}
	return parsed
}

func readCases(format, path string, explicit caseFlags) ([]qualification.CaseEvidence, error) {
	if format == "cases" {
		if path != "" {
			return nil, fmt.Errorf("-log is not valid with -log-format=cases")
		}
		cases := make([]qualification.CaseEvidence, 0, len(explicit))
		for _, value := range explicit {
			id, outcome, ok := strings.Cut(value, "=")
			if !ok || id == "" || outcome == "" {
				return nil, fmt.Errorf("invalid -case %q; require id=outcome", value)
			}
			cases = append(cases, qualification.CaseEvidence{ID: id, Outcome: qualification.CaseOutcome(outcome)})
		}
		return cases, nil
	}
	if len(explicit) != 0 {
		return nil, fmt.Errorf("-case requires -log-format=cases")
	}
	if path == "" {
		return nil, fmt.Errorf("-log is required with -log-format=%s", format)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	switch format {
	case "go":
		return qualification.ParseGoTestLog(file)
	case "drill":
		return qualification.ParseDrillLog(file)
	case "go-then-cases":
		return qualification.ParseGoSuiteThenCases(file)
	default:
		return nil, fmt.Errorf("unknown -log-format %q", format)
	}
}

func readJSON(path string, into any) {
	file, err := os.Open(path)
	if err != nil {
		fail(err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(into); err != nil {
		fail(fmt.Errorf("decode %s: %w", path, err))
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
