package qualification

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rhobuild/runpool/internal/platform"
)

// SuiteID identifies one independently executed qualification boundary.
// The value is persisted in release evidence; changing it creates a new
// boundary rather than renaming an implementation detail.
type SuiteID string

const (
	SuiteHermetic              SuiteID = "hermetic"
	SuiteDockerContract        SuiteID = "docker-contract"
	SuiteCapsuleContract       SuiteID = "capsule-contract"
	SuiteSQLiteDurability      SuiteID = "sqlite-durability"
	SuiteLifecycleDrills       SuiteID = "lifecycle-drills"
	SuiteProviderContracts     SuiteID = "contracts-github-actions"
	SuiteControllerPortable    SuiteID = "controller-e2e-portable"
	SuiteControllerEndToEnd    SuiteID = "controller-e2e"
	suiteEvidenceSchemaVersion         = 1
)

// EnvironmentKind states whether a suite ran on a reproducible hosted
// runner image or on the exact host whose platform facts are qualified.
type EnvironmentKind string

const (
	EnvironmentHosted          EnvironmentKind = "github-hosted"
	EnvironmentReleasePlatform EnvironmentKind = "release-platform"
)

// ExecutionEnvironment identifies the machine class a suite actually used.
// Facts are required for release-platform evidence and deliberately absent
// for GitHub-hosted evidence, whose maintained runner image is the contract.
type ExecutionEnvironment struct {
	Kind   EnvironmentKind `json:"kind"`
	Runner string          `json:"runner"`
	Facts  *platform.Facts `json:"facts,omitempty"`
}

// CaseOutcome is the terminal result of one expected qualification case.
type CaseOutcome string

const (
	CasePassed  CaseOutcome = "passed"
	CaseFailed  CaseOutcome = "failed"
	CaseSkipped CaseOutcome = "skipped"
)

// CaseEvidence is one observed test or drill. DurationMilliseconds is zero
// only for boundaries, such as workflow jobs, that expose no useful duration.
type CaseEvidence struct {
	ID                   string      `json:"id"`
	Outcome              CaseOutcome `json:"outcome"`
	DurationMilliseconds int64       `json:"duration_ms"`
}

// CaseSummary is redundant by design. Producers make it convenient for a
// human to read; the assembler recomputes it and refuses any disagreement.
type CaseSummary struct {
	Expected int `json:"expected"`
	Executed int `json:"executed"`
	Passed   int `json:"passed"`
	Failed   int `json:"failed"`
	Skipped  int `json:"skipped"`
}

// SuiteEvidence is the common, machine-checked evidence contract emitted by
// every qualification suite.
type SuiteEvidence struct {
	SchemaVersion   int                  `json:"schema_version"`
	SuiteID         SuiteID              `json:"suite_id"`
	Commit          string               `json:"commit"`
	Run             string               `json:"run"`
	StartedAt       time.Time            `json:"started_at"`
	CompletedAt     time.Time            `json:"completed_at"`
	Environment     ExecutionEnvironment `json:"environment"`
	ControllerImage string               `json:"controller_image,omitempty"`
	CapsuleImage    string               `json:"capsule_image,omitempty"`
	ExpectedCases   []string             `json:"expected_cases"`
	Cases           []CaseEvidence       `json:"cases"`
	Summary         CaseSummary          `json:"summary"`
	Outcome         CaseOutcome          `json:"outcome"`
}

type suiteDefinition struct {
	id              SuiteID
	evidencePath    string
	environment     ExecutionEnvironment
	requiresFacts   bool
	controllerImage bool
	capsuleImage    bool
	expectedCases   []string
}

var suiteDefinitions = []suiteDefinition{
	{
		id: SuiteHermetic, evidencePath: "hermetic/suite.json",
		environment:   ExecutionEnvironment{Kind: EnvironmentHosted, Runner: "ubuntu-24.04"},
		expectedCases: []string{"build", "lint", "vulnerabilities", "image"},
	},
	{
		id: SuiteDockerContract, evidencePath: "live/docker-contract.json",
		environment:   ExecutionEnvironment{Kind: EnvironmentReleasePlatform, Runner: "self-hosted"},
		requiresFacts: true,
		expectedCases: []string{
			"TestContainerLifecycle", "TestRemoveIsIdempotent", "TestPullOnMissingImage",
			"TestExec", "TestExecHonoursItsContext", "TestExecWithInput", "TestOwnershipQueries",
			"TestResourceLimits", "TestOOMState", "TestOwnedIDByName", "TestOwnedRemovalRefusesForeignNetworkAndVolume",
			"TestTheDaemonSpellingSelectsTheEntry", "TestHostInfo", "TestEnsureOwnedVolume",
			"TestCreateVolumeRefusesATakenName", "TestCacheLaneVolumes", "TestFilesystemProbe",
			"TestOwnedVolumeUsage", "TestContainerDiskFull", "TestCleanupSurvivesCancellation",
			"TestContainerRemovalSparesNamedVolumes",
		},
	},
	{
		id: SuiteCapsuleContract, evidencePath: "live/capsule-contract.json",
		environment:   ExecutionEnvironment{Kind: EnvironmentReleasePlatform, Runner: "self-hosted"},
		requiresFacts: true, capsuleImage: true,
		expectedCases: []string{
			"TestLeaseResourceBudget", "TestNetworkSandboxBypass", "TestCapsuleLifecycle",
			"TestTheCapsuleDeclaresTheProtocolThisBuildSpeaks", "TestPrepareWaitsForAProvenDaemon",
			"TestAbortBeforeStartExitsWithTheReservedCode",
		},
	},
	{
		id: SuiteSQLiteDurability, evidencePath: "live/sqlite-durability.json",
		environment:   ExecutionEnvironment{Kind: EnvironmentReleasePlatform, Runner: "self-hosted"},
		requiresFacts: true,
		expectedCases: []string{
			"TestPragmas", "TestCrashRecovery", "TestContention", "TestSingletonLock",
			"TestBackupRestore", "TestDiskFull", "TestMigrationMechanics",
			"container-kill-round-1", "container-kill-round-2", "container-kill-round-3",
		},
	},
	{
		id: SuiteLifecycleDrills, evidencePath: "live/lifecycle-drills.json",
		environment:   ExecutionEnvironment{Kind: EnvironmentReleasePlatform, Runner: "self-hosted"},
		requiresFacts: true,
		expectedCases: []string{"install", "backup", "restore", "upgrade", "uninstall"},
	},
	{
		id: SuiteProviderContracts, evidencePath: "upstream/suite.json",
		environment: ExecutionEnvironment{Kind: EnvironmentHosted, Runner: "ubuntu-24.04"},
		expectedCases: []string{
			"TestOrganizationJitAssignmentNotBound", "TestInvalidCredentialFailsClosed",
			"TestZeroCapacityDoesNotRevealQueuedDemand", "TestEveryContractFixtureIsInstalled",
			"TestDeliveryIdentityIsStable", "TestLapsedAssignmentIsCancelledAndRequeued",
			"TestScaleSetSystemLabel", "TestRepositoryScaleSetAndJitRunner",
			"TestASecondSessionIsRefusedRecognisably", "TestOrganizationDefaultGroupScaleSet",
			"TestTheScaleSetForbidsRunnerSelfUpdate", "TestAnAdoptedScaleSetForbidsRunnerSelfUpdate",
			"TestAdoptionLeavesACorrectScaleSetAlone",
		},
	},
	{
		id: SuiteControllerPortable, evidencePath: "controller-e2e/portable/suite.json",
		environment:     ExecutionEnvironment{Kind: EnvironmentHosted, Runner: "ubuntu-24.04"},
		controllerImage: true, capsuleImage: true,
		expectedCases: []string{"TestControllerEndToEnd"},
	},
	{
		id: SuiteControllerEndToEnd, evidencePath: "controller-e2e/reference/suite.json",
		environment:   ExecutionEnvironment{Kind: EnvironmentReleasePlatform, Runner: "self-hosted"},
		requiresFacts: true, controllerImage: true, capsuleImage: true,
		expectedCases: []string{"TestControllerEndToEnd"},
	},
}

func definitionFor(id SuiteID) (suiteDefinition, bool) {
	for _, definition := range suiteDefinitions {
		if definition.id == id {
			return definition, true
		}
	}
	return suiteDefinition{}, false
}

// ExpectedCases returns a copy of the cases that constitute a suite. The
// producer and assembler call the same registry, while the manifest persists
// the set so an archived record remains independently inspectable.
func ExpectedCases(id SuiteID) ([]string, error) {
	definition, ok := definitionFor(id)
	if !ok {
		return nil, fmt.Errorf("unknown qualification suite %q", id)
	}
	return slices.Clone(definition.expectedCases), nil
}

// NewSuiteEvidence constructs a manifest and derives all redundant fields.
func NewSuiteEvidence(id SuiteID, build Build, environment ExecutionEnvironment, startedAt, completedAt time.Time, cases []CaseEvidence) (SuiteEvidence, error) {
	definition, ok := definitionFor(id)
	if !ok {
		return SuiteEvidence{}, fmt.Errorf("unknown qualification suite %q", id)
	}
	expected := slices.Clone(definition.expectedCases)
	evidence := SuiteEvidence{
		SchemaVersion: suiteEvidenceSchemaVersion,
		SuiteID:       id, Commit: build.Commit, Run: build.Run,
		StartedAt: startedAt.UTC(), CompletedAt: completedAt.UTC(),
		Environment:   environment,
		ExpectedCases: expected, Cases: slices.Clone(cases),
	}
	if definition.controllerImage {
		evidence.ControllerImage = build.ControllerImage
	}
	if definition.capsuleImage {
		evidence.CapsuleImage = build.CapsuleImage
	}
	evidence.Summary, evidence.Outcome = summarize(expected, cases)
	return evidence, nil
}

func summarize(expected []string, cases []CaseEvidence) (CaseSummary, CaseOutcome) {
	summary := CaseSummary{Expected: len(expected), Executed: len(cases)}
	for _, result := range cases {
		switch result.Outcome {
		case CasePassed:
			summary.Passed++
		case CaseFailed:
			summary.Failed++
		case CaseSkipped:
			summary.Skipped++
		}
	}
	if summary.Executed == summary.Expected && summary.Passed == summary.Expected && summary.Failed == 0 && summary.Skipped == 0 {
		return summary, CasePassed
	}
	return summary, CaseFailed
}

func validateSuiteEvidence(evidence SuiteEvidence, definition suiteDefinition, build Build, observed platform.Facts) error {
	if evidence.SchemaVersion != suiteEvidenceSchemaVersion {
		return fmt.Errorf("suite %s uses evidence schema %d; require %d", definition.id, evidence.SchemaVersion, suiteEvidenceSchemaVersion)
	}
	if evidence.SuiteID != definition.id {
		return fmt.Errorf("expected suite %s at %s, found %s", definition.id, definition.evidencePath, evidence.SuiteID)
	}
	if evidence.Commit == "" || evidence.Commit != build.Commit {
		return fmt.Errorf("suite %s covered commit %q, not %q", definition.id, evidence.Commit, build.Commit)
	}
	if evidence.Run == "" || evidence.Run != build.Run {
		return fmt.Errorf("suite %s belongs to run %q, not %q", definition.id, evidence.Run, build.Run)
	}
	if evidence.StartedAt.IsZero() || evidence.CompletedAt.IsZero() || evidence.CompletedAt.Before(evidence.StartedAt) {
		return fmt.Errorf("suite %s has an invalid execution interval", definition.id)
	}
	if evidence.Environment.Kind != definition.environment.Kind || evidence.Environment.Runner != definition.environment.Runner {
		return fmt.Errorf("suite %s ran in environment %s/%s, require %s/%s", definition.id,
			evidence.Environment.Kind, evidence.Environment.Runner,
			definition.environment.Kind, definition.environment.Runner)
	}
	if definition.requiresFacts {
		if evidence.Environment.Facts == nil {
			return fmt.Errorf("suite %s states no release-platform facts", definition.id)
		}
		if !reflect.DeepEqual(*evidence.Environment.Facts, observed) {
			return fmt.Errorf("suite %s ran on platform facts that differ from the qualification host", definition.id)
		}
	} else if evidence.Environment.Facts != nil {
		return fmt.Errorf("suite %s attached release-platform facts to a hosted run", definition.id)
	}
	if definition.controllerImage && evidence.ControllerImage != build.ControllerImage {
		return fmt.Errorf("suite %s covered controller_image %q, not %q", definition.id, evidence.ControllerImage, build.ControllerImage)
	}
	if definition.capsuleImage && evidence.CapsuleImage != build.CapsuleImage {
		return fmt.Errorf("suite %s covered capsule_image %q, not %q", definition.id, evidence.CapsuleImage, build.CapsuleImage)
	}
	if err := sameSet("expected cases", evidence.ExpectedCases, definition.expectedCases); err != nil {
		return fmt.Errorf("suite %s: %w", definition.id, err)
	}

	caseIDs := make([]string, 0, len(evidence.Cases))
	for _, result := range evidence.Cases {
		if result.DurationMilliseconds < 0 {
			return fmt.Errorf("suite %s case %s has a negative duration", definition.id, result.ID)
		}
		switch result.Outcome {
		case CasePassed, CaseFailed, CaseSkipped:
		default:
			return fmt.Errorf("suite %s case %s has unknown outcome %q", definition.id, result.ID, result.Outcome)
		}
		caseIDs = append(caseIDs, result.ID)
	}
	if err := sameSet("executed cases", caseIDs, definition.expectedCases); err != nil {
		return fmt.Errorf("suite %s: %w", definition.id, err)
	}
	summary, outcome := summarize(definition.expectedCases, evidence.Cases)
	if evidence.Summary != summary {
		return fmt.Errorf("suite %s summary %+v disagrees with observed cases %+v", definition.id, evidence.Summary, summary)
	}
	if evidence.Outcome != outcome {
		return fmt.Errorf("suite %s outcome %q disagrees with observed outcome %q", definition.id, evidence.Outcome, outcome)
	}
	if outcome != CasePassed {
		return fmt.Errorf("suite %s did not pass: %d failed, %d skipped", definition.id, summary.Failed, summary.Skipped)
	}
	return nil
}

// ValidateSuiteEvidence applies the repository's contract for the manifest's
// suite. Hosted suites ignore observed; release-platform suites require it.
func ValidateSuiteEvidence(evidence SuiteEvidence, build Build, observed platform.Facts) error {
	definition, ok := definitionFor(evidence.SuiteID)
	if !ok {
		return fmt.Errorf("unknown qualification suite %q", evidence.SuiteID)
	}
	return validateSuiteEvidence(evidence, definition, build, observed)
}

func sameSet(name string, got, want []string) error {
	counts := make(map[string]int, len(got))
	for _, item := range got {
		if item == "" {
			return fmt.Errorf("%s contain an empty identity", name)
		}
		counts[item]++
		if counts[item] > 1 {
			return fmt.Errorf("%s contain duplicate %q", name, item)
		}
	}
	missing := make([]string, 0)
	unexpected := make([]string, 0)
	for _, item := range want {
		if counts[item] == 0 {
			missing = append(missing, item)
		}
		delete(counts, item)
	}
	for item := range counts {
		unexpected = append(unexpected, item)
	}
	slices.Sort(missing)
	slices.Sort(unexpected)
	if len(missing) != 0 || len(unexpected) != 0 {
		return fmt.Errorf("%s differ: missing=%v unexpected=%v", name, missing, unexpected)
	}
	return nil
}

var goTestResult = regexp.MustCompile(`^--- (PASS|FAIL|SKIP): ([^ ]+) \(([0-9.]+)s\)$`)

// ParseGoTestLog extracts top-level results from Go's stable verbose test
// protocol. A skipped or failed subtest is rejected even when its parent later
// reports PASS, so a producer cannot flatten away an incomplete contract.
func ParseGoTestLog(input io.Reader) ([]CaseEvidence, error) {
	results := make(map[string]CaseEvidence)
	order := make([]string, 0)
	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		match := goTestResult.FindStringSubmatch(strings.TrimSpace(scanner.Text()))
		if match == nil {
			continue
		}
		var outcome CaseOutcome
		switch match[1] {
		case "PASS":
			outcome = CasePassed
		case "FAIL":
			outcome = CaseFailed
		case "SKIP":
			outcome = CaseSkipped
		}
		id := match[2]
		if strings.Contains(id, "/") {
			if outcome != CasePassed {
				return nil, fmt.Errorf("subtest %s %s", id, outcome)
			}
			continue
		}
		if _, exists := results[id]; exists {
			return nil, fmt.Errorf("test %s reported more than one terminal result", id)
		}
		seconds, err := strconv.ParseFloat(match[3], 64)
		if err != nil {
			return nil, fmt.Errorf("parse duration for %s: %w", id, err)
		}
		results[id] = CaseEvidence{ID: id, Outcome: outcome, DurationMilliseconds: int64(seconds * 1000)}
		order = append(order, id)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	cases := make([]CaseEvidence, 0, len(order))
	for _, id := range order {
		cases = append(cases, results[id])
	}
	return cases, nil
}

const drillMarkerPrefix = "RUNPOOL_CASE "

// ParseDrillLog extracts explicit property markers from a shell harness.
func ParseDrillLog(input io.Reader) ([]CaseEvidence, error) {
	var cases []CaseEvidence
	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, drillMarkerPrefix) {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, drillMarkerPrefix))
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid drill marker %q", line)
		}
		outcome := CaseOutcome(fields[1])
		if outcome != CasePassed && outcome != CaseFailed && outcome != CaseSkipped {
			return nil, fmt.Errorf("invalid drill outcome %q", outcome)
		}
		cases = append(cases, CaseEvidence{ID: fields[0], Outcome: outcome})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return cases, nil
}

// EncodeSuiteEvidence writes the stable, indented artifact representation.
func EncodeSuiteEvidence(output io.Writer, evidence SuiteEvidence) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(evidence)
}

// DecodeSuiteEvidence rejects trailing documents so one evidence path cannot
// ambiguously contain two runs.
func DecodeSuiteEvidence(input io.Reader) (SuiteEvidence, error) {
	decoder := json.NewDecoder(input)
	var evidence SuiteEvidence
	if err := decoder.Decode(&evidence); err != nil {
		return SuiteEvidence{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return SuiteEvidence{}, errors.New("suite evidence contains more than one JSON document")
		}
		return SuiteEvidence{}, err
	}
	return evidence, nil
}
