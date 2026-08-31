package qualification

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

const (
	maxEvidenceLogLineBytes  = 1024 * 1024
	caseMarkerPrefix         = "RUNPOOL_CASE "
	qualificationPhasePrefix = "RUNPOOL_PHASE "
	goSuiteCompleteMarker    = qualificationPhasePrefix + "go-suite-complete"
)

var goTestResult = regexp.MustCompile(`^--- (PASS|FAIL|SKIP): ([^ ]+) \(([0-9.]+)s\)$`)

type goTestCaseCollector struct {
	results map[string]CaseEvidence
	order   []string
}

func newGoTestCaseCollector() *goTestCaseCollector {
	return &goTestCaseCollector{results: make(map[string]CaseEvidence)}
}

func (collector *goTestCaseCollector) observe(line string) error {
	match := goTestResult.FindStringSubmatch(strings.TrimSpace(line))
	if match == nil {
		return nil
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
			return fmt.Errorf("subtest %s %s", id, outcome)
		}
		return nil
	}
	if _, exists := collector.results[id]; exists {
		return fmt.Errorf("test %s reported more than one terminal result", id)
	}
	seconds, err := strconv.ParseFloat(match[3], 64)
	if err != nil {
		return fmt.Errorf("parse duration for %s: %w", id, err)
	}
	collector.results[id] = CaseEvidence{
		ID: id, Outcome: outcome, DurationMilliseconds: int64(seconds * 1000),
	}
	collector.order = append(collector.order, id)
	return nil
}

func (collector *goTestCaseCollector) cases() []CaseEvidence {
	cases := make([]CaseEvidence, 0, len(collector.order))
	for _, id := range collector.order {
		cases = append(cases, collector.results[id])
	}
	return cases
}

type caseMarkerCollector struct {
	cases []CaseEvidence
	seen  map[string]struct{}
}

func newCaseMarkerCollector() *caseMarkerCollector {
	return &caseMarkerCollector{seen: make(map[string]struct{})}
}

func (collector *caseMarkerCollector) observe(line string) (bool, error) {
	observed, ok, err := parseCaseMarker(line)
	if err != nil || !ok {
		return ok, err
	}
	if _, exists := collector.seen[observed.ID]; exists {
		return true, fmt.Errorf("case %s reported more than one terminal result", observed.ID)
	}
	collector.seen[observed.ID] = struct{}{}
	collector.cases = append(collector.cases, observed)
	return true, nil
}

func parseCaseMarker(line string) (CaseEvidence, bool, error) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, caseMarkerPrefix) {
		return CaseEvidence{}, false, nil
	}
	fields := strings.Fields(strings.TrimPrefix(line, caseMarkerPrefix))
	if len(fields) != 2 {
		return CaseEvidence{}, true, fmt.Errorf("invalid case marker %q", line)
	}
	outcome := CaseOutcome(fields[1])
	if outcome != CasePassed && outcome != CaseFailed && outcome != CaseSkipped {
		return CaseEvidence{}, true, fmt.Errorf("invalid case outcome %q", outcome)
	}
	return CaseEvidence{ID: fields[0], Outcome: outcome}, true, nil
}

func scanEvidenceLog(input io.Reader, observe func(string) error) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), maxEvidenceLogLineBytes)
	for scanner.Scan() {
		if err := observe(scanner.Text()); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan qualification transcript: %w", err)
	}
	return nil
}

// ParseGoTestLog extracts top-level results from Go's stable verbose test
// protocol. A skipped or failed subtest is rejected even when its parent later
// reports PASS, so a producer cannot flatten away an incomplete contract.
func ParseGoTestLog(input io.Reader) ([]CaseEvidence, error) {
	collector := newGoTestCaseCollector()
	if err := scanEvidenceLog(input, collector.observe); err != nil {
		return nil, err
	}
	return collector.cases(), nil
}

// ParseDrillLog extracts explicit property markers from a shell harness.
func ParseDrillLog(input io.Reader) ([]CaseEvidence, error) {
	collector := newCaseMarkerCollector()
	if err := scanEvidenceLog(input, func(line string) error {
		_, err := collector.observe(line)
		return err
	}); err != nil {
		return nil, err
	}
	return collector.cases, nil
}

// ParseGoSuiteThenCases reads the ordered transcript protocol used when a
// harness runs one authoritative Go suite before executing diagnostic rounds.
// The harness must emit RUNPOOL_PHASE go-suite-complete exactly once. Go test
// results are evidence only before that boundary; explicit RUNPOOL_CASE
// markers are evidence only after it.
func ParseGoSuiteThenCases(input io.Reader) ([]CaseEvidence, error) {
	goCases := newGoTestCaseCollector()
	explicitCases := newCaseMarkerCollector()
	goSuiteComplete := false

	err := scanEvidenceLog(input, func(line string) error {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, qualificationPhasePrefix) {
			if trimmed != goSuiteCompleteMarker {
				return fmt.Errorf("invalid qualification phase marker %q", trimmed)
			}
			if goSuiteComplete {
				return errors.New("go suite completion marker appears more than once")
			}
			goSuiteComplete = true
			return nil
		}

		if !goSuiteComplete {
			observed, isExplicitCase, err := parseCaseMarker(trimmed)
			if err != nil {
				return err
			}
			if isExplicitCase {
				return fmt.Errorf("case %s appears before the go suite completion marker", observed.ID)
			}
			return goCases.observe(trimmed)
		}
		_, err := explicitCases.observe(trimmed)
		return err
	})
	if err != nil {
		return nil, err
	}
	if !goSuiteComplete {
		return nil, errors.New("go suite completion marker is missing")
	}
	return append(goCases.cases(), explicitCases.cases...), nil
}
