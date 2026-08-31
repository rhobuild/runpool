package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/rhobuild/runpool/internal/qualification"
)

func TestReadCasesSupportsTheOrderedHarnessProtocol(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.log")
	transcript := "--- PASS: TestOne (0.01s)\n" +
		"RUNPOOL_PHASE go-suite-complete\n" +
		"--- PASS: TestDiagnostic (0.01s)\n" +
		"RUNPOOL_CASE round-1 passed\n"
	if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}

	cases, err := readCases("go-then-cases", path, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []qualification.CaseEvidence{
		{ID: "TestOne", Outcome: qualification.CasePassed, DurationMilliseconds: 10},
		{ID: "round-1", Outcome: qualification.CasePassed},
	}
	if !slices.Equal(cases, want) {
		t.Fatalf("cases = %+v; want %+v", cases, want)
	}
}

func TestReadCasesRejectsTheFormerAmbiguousFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if cases, err := readCases("mixed", path, nil); err == nil {
		t.Fatalf("accepted the retired mixed format: %+v", cases)
	}
}
