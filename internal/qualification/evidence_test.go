package qualification

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestGoTestEvidenceCannotHideIncompleteCases(t *testing.T) {
	for name, log := range map[string]string{
		"skipped top-level test": "--- SKIP: TestOne (0.01s)\n",
		"skipped subtest":        "--- SKIP: TestOne/required-case (0.01s)\n--- PASS: TestOne (0.02s)\n",
		"failed subtest":         "--- FAIL: TestOne/required-case (0.01s)\n--- FAIL: TestOne (0.02s)\n",
		"duplicate result":       "--- PASS: TestOne (0.01s)\n--- PASS: TestOne (0.02s)\n",
	} {
		t.Run(name, func(t *testing.T) {
			cases, err := ParseGoTestLog(strings.NewReader(log))
			if name == "skipped top-level test" {
				if err != nil {
					t.Fatal(err)
				}
				if len(cases) != 1 || cases[0].Outcome != CaseSkipped {
					t.Fatalf("cases = %+v", cases)
				}
				return
			}
			if err == nil {
				t.Fatalf("parsed evidence that conceals %s: %+v", name, cases)
			}
		})
	}
}

func TestGoTestEvidenceRecordsOnlyObservedTerminalResults(t *testing.T) {
	log := strings.NewReader("=== RUN   TestOne\n--- PASS: TestOne (1.25s)\n" +
		"=== RUN   TestTwo\n--- FAIL: TestTwo (0.50s)\n")
	cases, err := ParseGoTestLog(log)
	if err != nil {
		t.Fatal(err)
	}
	want := []CaseEvidence{
		{ID: "TestOne", Outcome: CasePassed, DurationMilliseconds: 1250},
		{ID: "TestTwo", Outcome: CaseFailed, DurationMilliseconds: 500},
	}
	if len(cases) != len(want) {
		t.Fatalf("cases = %+v", cases)
	}
	for i := range want {
		if cases[i] != want[i] {
			t.Errorf("case %d = %+v; want %+v", i, cases[i], want[i])
		}
	}
}

func TestDrillEvidenceRejectsDuplicateCaseResults(t *testing.T) {
	transcript := strings.NewReader(
		caseMarkerPrefix + "round-1 passed\n" +
			caseMarkerPrefix + "round-1 failed\n",
	)
	if cases, err := ParseDrillLog(transcript); err == nil {
		t.Fatalf("accepted contradictory case results: %+v", cases)
	}
}

func TestGoSuiteThenCasesValidatesTheDurabilityInventory(t *testing.T) {
	log, err := os.Open(filepath.Join("testdata", "sqlite-durability-transcript.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })

	cases, err := ParseGoSuiteThenCases(log)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(cases))
	for _, observed := range cases {
		ids = append(ids, observed.ID)
		if observed.Outcome != CasePassed {
			t.Errorf("case %s = %s; want passed", observed.ID, observed.Outcome)
		}
	}
	want, err := ExpectedCases(SuiteSQLiteDurability)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, want) {
		t.Fatalf("case ids = %v; want %v", ids, want)
	}
	if cases[1].DurationMilliseconds != 3040 {
		t.Errorf("crash recovery duration = %dms; want 3040ms", cases[1].DurationMilliseconds)
	}

	observed := frozenReference().Platforms[1].Platform
	environment := ExecutionEnvironment{
		Kind: EnvironmentReleasePlatform, Runner: "self-hosted", Facts: &observed,
	}
	manifest, err := NewSuiteEvidence(
		SuiteSQLiteDurability, testBuild(), environment,
		time.Unix(1, 0), time.Unix(2, 0), cases,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateSuiteEvidence(manifest, testBuild(), observed); err != nil {
		t.Fatalf("complete durability evidence was refused: %v", err)
	}
	if manifest.Summary != (CaseSummary{Expected: 10, Executed: 10, Passed: 10}) {
		t.Errorf("summary = %+v; want ten executed and passed cases", manifest.Summary)
	}
}

func TestGoSuiteThenCasesRequiresAnOrderedProtocol(t *testing.T) {
	for name, transcript := range map[string]string{
		"missing boundary":    "--- PASS: TestOne (0.01s)\n",
		"duplicate boundary":  goSuiteCompleteMarker + "\n" + goSuiteCompleteMarker + "\n",
		"case before suite":   caseMarkerPrefix + "round-1 passed\n" + goSuiteCompleteMarker + "\n",
		"unknown phase":       qualificationPhasePrefix + "another-phase\n",
		"duplicate go result": "--- PASS: TestOne (0.01s)\n--- PASS: TestOne (0.02s)\n" + goSuiteCompleteMarker + "\n",
		"duplicate case":      goSuiteCompleteMarker + "\n" + caseMarkerPrefix + "round-1 passed\n" + caseMarkerPrefix + "round-1 passed\n",
	} {
		t.Run(name, func(t *testing.T) {
			cases, err := ParseGoSuiteThenCases(strings.NewReader(transcript))
			if err == nil {
				t.Fatalf("parsed malformed protocol: %+v", cases)
			}
		})
	}
}

func TestGoSuiteThenCasesUsesOnlyTheExplicitBoundary(t *testing.T) {
	transcript := strings.NewReader(
		"--- PASS: TestOne (0.01s)\n" +
			"PASS\n" +
			"--- PASS: TestTwo (0.02s)\n" +
			goSuiteCompleteMarker + "\n" +
			"--- PASS: TestVerifyExisting (0.03s)\n" +
			caseMarkerPrefix + "round-1 passed\n",
	)
	cases, err := ParseGoSuiteThenCases(transcript)
	if err != nil {
		t.Fatal(err)
	}
	want := []CaseEvidence{
		{ID: "TestOne", Outcome: CasePassed, DurationMilliseconds: 10},
		{ID: "TestTwo", Outcome: CasePassed, DurationMilliseconds: 20},
		{ID: "round-1", Outcome: CasePassed},
	}
	if !slices.Equal(cases, want) {
		t.Fatalf("cases = %+v; want %+v", cases, want)
	}
}

func TestSuiteEvidenceIsBoundToCasesBuildAndEnvironment(t *testing.T) {
	definition, ok := definitionFor(SuiteHermetic)
	if !ok {
		t.Fatal("hermetic suite is not registered")
	}
	cases := make([]CaseEvidence, 0, len(definition.expectedCases))
	for _, id := range definition.expectedCases {
		cases = append(cases, CaseEvidence{ID: id, Outcome: CasePassed})
	}
	build := testBuild()
	manifest, err := NewSuiteEvidence(definition.id, build, definition.environment,
		time.Unix(1, 0), time.Unix(2, 0), cases)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateSuiteEvidence(manifest, build, frozenReference().Platforms[1].Platform); err != nil {
		t.Fatalf("complete manifest was refused: %v", err)
	}

	for name, mutate := range map[string]func(*SuiteEvidence){
		"wrong commit":     func(e *SuiteEvidence) { e.Commit += "0" },
		"wrong run":        func(e *SuiteEvidence) { e.Run += "/other" },
		"missing expected": func(e *SuiteEvidence) { e.ExpectedCases = e.ExpectedCases[1:] },
		"zero executed":    func(e *SuiteEvidence) { e.Cases = nil; e.Summary, e.Outcome = summarize(e.ExpectedCases, nil) },
		"skipped case": func(e *SuiteEvidence) {
			e.Cases[0].Outcome = CaseSkipped
			e.Summary, e.Outcome = summarize(e.ExpectedCases, e.Cases)
		},
		"forged summary":    func(e *SuiteEvidence) { e.Summary.Passed-- },
		"wrong environment": func(e *SuiteEvidence) { e.Environment.Runner = "ubuntu-latest" },
	} {
		t.Run(name, func(t *testing.T) {
			broken := manifest
			broken.ExpectedCases = append([]string(nil), manifest.ExpectedCases...)
			broken.Cases = append([]CaseEvidence(nil), manifest.Cases...)
			mutate(&broken)
			if err := ValidateSuiteEvidence(broken, build, frozenReference().Platforms[1].Platform); err == nil {
				t.Fatalf("accepted manifest with %s", name)
			}
		})
	}
}

func TestReleasePlatformEvidenceMustNameTheObservedHost(t *testing.T) {
	definition, ok := definitionFor(SuiteControllerEndToEnd)
	if !ok {
		t.Fatal("controller E2E suite is not registered")
	}
	observed := frozenReference().Platforms[1].Platform
	environment := definition.environment
	environment.Facts = &observed
	manifest, err := NewSuiteEvidence(definition.id, testBuild(), environment,
		time.Unix(1, 0), time.Unix(2, 0), []CaseEvidence{{ID: "TestControllerEndToEnd", Outcome: CasePassed}})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateSuiteEvidence(manifest, testBuild(), observed); err != nil {
		t.Fatal(err)
	}
	other := observed
	other.Kernel = "another-kernel"
	if err := ValidateSuiteEvidence(manifest, testBuild(), other); err == nil {
		t.Fatal("accepted evidence produced on another host")
	}
}

func TestQualificationInventoriesFollowTheirGoSuites(t *testing.T) {
	for id, relativeDir := range map[SuiteID]string{
		SuiteDockerContract:    filepath.Join("..", "..", "test", "contract", "docker"),
		SuiteCapsuleContract:   filepath.Join("..", "..", "test", "contract", "capsule"),
		SuiteSQLiteDurability:  filepath.Join("..", "..", "test", "contract", "sqlite"),
		SuiteProviderContracts: filepath.Join("..", "..", "test", "contract", "githubactions"),
	} {
		t.Run(string(id), func(t *testing.T) {
			definition, ok := definitionFor(id)
			if !ok {
				t.Fatalf("suite %s is not registered", id)
			}
			got := testFunctions(t, relativeDir)
			want := slices.Clone(definition.expectedCases)
			if id == SuiteSQLiteDurability {
				got = slices.DeleteFunc(got, func(name string) bool { return name == "TestVerifyExisting" })
				want = slices.DeleteFunc(want, func(name string) bool { return strings.HasPrefix(name, "container-kill-") })
			}
			if err := sameSet("registered tests", got, want); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func testFunctions(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		matches, err := build.Default.MatchFile(dir, entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if !matches {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, entry.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && function.Recv == nil && function.Name.Name != "TestMain" && strings.HasPrefix(function.Name.Name, "Test") {
				names = append(names, function.Name.Name)
			}
		}
	}
	slices.Sort(names)
	return names
}

func TestShellDrillInventoriesHaveOneSuccessMarkerPerCase(t *testing.T) {
	for id, script := range map[SuiteID]string{
		SuiteSQLiteDurability: filepath.Join("..", "..", "test", "contract", "sqlite", "testdata", "remote-harness.sh"),
		SuiteLifecycleDrills:  filepath.Join("..", "..", "test", "drills", "remote-harness.sh"),
	} {
		t.Run(string(id), func(t *testing.T) {
			body, err := os.ReadFile(script)
			if err != nil {
				t.Fatal(err)
			}
			definition, _ := definitionFor(id)
			for _, expected := range definition.expectedCases {
				if strings.HasPrefix(expected, "Test") {
					continue
				}
				if id == SuiteSQLiteDurability && strings.HasPrefix(expected, "container-kill-round-") {
					continue
				}
				marker := caseMarkerPrefix + expected + " " + string(CasePassed)
				if count := strings.Count(string(body), marker); count != 1 {
					t.Errorf("%s contains marker %q %d times; require exactly one", script, marker, count)
				}
			}
			if id == SuiteSQLiteDurability {
				for _, fragment := range []string{
					`echo "RUNPOOL_PHASE go-suite-complete"`,
					"for round in 1 2 3",
					`echo "RUNPOOL_CASE container-kill-round-$round passed"`,
				} {
					if !strings.Contains(string(body), fragment) {
						t.Errorf("%s does not contain %q", script, fragment)
					}
				}
			}
		})
	}
}
