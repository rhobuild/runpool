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
				marker := drillMarkerPrefix + expected + " " + string(CasePassed)
				if count := strings.Count(string(body), marker); count != 1 {
					t.Errorf("%s contains marker %q %d times; require exactly one", script, marker, count)
				}
			}
			if id == SuiteSQLiteDurability {
				for _, fragment := range []string{
					"for round in 1 2 3", `echo "RUNPOOL_CASE container-kill-round-$round passed"`,
				} {
					if !strings.Contains(string(body), fragment) {
						t.Errorf("%s does not contain %q", script, fragment)
					}
				}
			}
		})
	}
}
