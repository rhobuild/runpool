package platform

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"
)

// The embedded manifest is what the checks execute; the copy under build/ is
// what humans review. Byte equality matters because structurally equivalent
// JSON can still differ from the reviewed artifact.
func TestEmbeddedManifestMatchesTheReviewedCopy(t *testing.T) {
	reviewed, err := os.ReadFile("../../build/platform.lock.json")
	if err != nil {
		t.Fatalf("read the reviewed manifest: %v", err)
	}
	if !bytes.Equal(reviewed, manifestJSON) {
		t.Error("internal/platform/platform.lock.json differs from build/platform.lock.json; " +
			"regenerate it with `go generate ./internal/platform/`")
	}
}

func TestManifestSelectsTheLatestReviewedStableEngine(t *testing.T) {
	ref, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	amd64, ok := ref.For("amd64")
	if !ok {
		t.Fatalf("no entry for amd64; the release records %v", ref.Arches())
	}
	if amd64.Status != ReferenceStatusPending {
		t.Errorf("status = %q; want pending until the server facts are frozen", amd64.Status)
	}
	if amd64.Policy.TargetEngine != "29.7.2" || amd64.Policy.DockerChannel != "stable" {
		t.Errorf("Docker policy = %+v; want stable Engine 29.7.2", amd64.Policy)
	}
	if err := amd64.RequireFrozen(); err == nil {
		t.Fatal("a pending reference was accepted for release qualification")
	}
	if got := amd64.Compare(Facts{}); len(got) != 1 || got[0].Property != "reference_status" {
		t.Fatalf("pending comparison = %v; want a reference_status mismatch", got)
	}
}

// TestAPlatformWithNoEntryIsNotAFailedOne: a host nobody has run the
// suites on and a host that ran them and differed are different answers,
// and the file's shape used to make only the second expressible.
func TestAPlatformWithNoEntryIsNotAFailedOne(t *testing.T) {
	ref, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ref.For("arm64"); ok {
		t.Skip("arm64 is qualified now, so this says nothing")
	}
	err = ref.NotQualified("arm64")
	if err == nil {
		t.Fatal("an unqualified platform produced no error")
	}
	if !strings.Contains(err.Error(), "arm64") || !strings.Contains(err.Error(), "amd64") {
		t.Errorf("the error is %q; it has to name the platform asked about and the ones that "+
			"are qualified, or it reads as a broken release rather than an unqualified host", err)
	}
	// And the shape can hold one without changing: adding a platform is
	// adding an entry.
	if !slices.Contains(BuildableArches(), "arm64") {
		t.Error("arm64 is not buildable, so a qualification for it could never be recorded")
	}
}

func TestManifestRequiresEveryFrozenFact(t *testing.T) {
	bad := Qualified{
		Status:   ReferenceStatusFrozen,
		Policy:   testPolicy(),
		Recorded: "2026-08-14",
		Platform: Facts{
			OS: "debian", OSVersion: "13", Arch: "amd64",
			Engine: "29.7.2", CgroupVersion: "2", CgroupDriver: "systemd",
			StorageDriver: "overlay2",
		},
	}
	if err := bad.validate(); err == nil {
		t.Fatal("an incomplete frozen reference validated")
	}

	missingBoolean := frozenReference()
	missingBoolean.Platform.Rootless = nil
	if err := missingBoolean.validate(); err == nil {
		t.Fatal("a frozen reference without the rootless observation validated")
	}
}

// TestCompareNamesEveryDifference keeps qualification evidence fail-closed.
func TestCompareNamesEveryDifference(t *testing.T) {
	ref := frozenReference()

	if got := ref.Compare(ref.Platform); len(got) != 0 {
		t.Errorf("the reference platform mismatched itself: %v", got)
	}

	drifted := ref.Platform
	drifted.Engine = "29.6.3"
	drifted.CgroupDriver = "cgroupfs"
	drifted.Rootless = boolFact(true)
	got := ref.Compare(drifted)
	if len(got) != 3 {
		t.Fatalf("three drifted properties produced %d mismatches: %v", len(got), got)
	}
	for _, mismatch := range got {
		if mismatch.Want == "" || mismatch.Got == "" {
			t.Errorf("mismatch %v does not say both sides", mismatch)
		}
	}

	partial := Facts{Engine: ref.Platform.Engine, Rootless: ref.Platform.Rootless}
	if got := ref.Compare(partial); len(got) == 0 {
		t.Error("an incomplete observation matched the reference platform")
	}
}

func TestRuntimeComparisonUsesOnlyDockerFacts(t *testing.T) {
	ref := frozenReference()
	observed := Facts{
		Engine:        ref.Platform.Engine,
		API:           ref.Platform.API,
		Arch:          ref.Platform.Arch,
		CgroupVersion: ref.Platform.CgroupVersion,
		CgroupDriver:  ref.Platform.CgroupDriver,
		Rootless:      ref.Platform.Rootless,
	}
	if got := ref.CompareDockerFacts(observed); len(got) != 0 {
		t.Errorf("matching Docker runtime facts produced mismatches: %v", got)
	}
}

func testPolicy() Policy {
	return Policy{
		OS:            "debian",
		OSVersion:     "13",
		OSCodename:    "trixie",
		Arch:          "amd64",
		DockerChannel: "stable",
		DockerSource:  "https://download.docker.com/linux/debian",
		Selection:     "latest-stable-at-policy-review",
		TargetEngine:  "29.7.2",
		Reviewed:      "2026-08-14",
		FreezePoint:   "before-release-candidate",
	}
}

func frozenReference() Qualified {
	return Qualified{
		Status:   ReferenceStatusFrozen,
		Policy:   testPolicy(),
		Recorded: "2026-08-14",
		Platform: Facts{
			OS:                "debian",
			OSVersion:         "13",
			OSCodename:        "trixie",
			Arch:              "amd64",
			Kernel:            "6.12.0-amd64",
			Engine:            "29.7.2",
			API:               "1.52",
			CgroupVersion:     "2",
			CgroupDriver:      "systemd",
			StorageDriver:     "overlay2",
			BackingFilesystem: "ext4",
			Rootless:          boolFact(false),
			Containerd:        "2.2.6",
			Runc:              "1.4.0",
			Buildx:            "0.35.0",
			Compose:           "2.35.0",
			IPTables:          "1.8.11",
			NFTables:          "1.1.3",
		},
	}
}

func boolFact(value bool) *bool { return &value }

// TestTheSelectionPolicyIsAChoiceNotARule: which distribution was
// selected is reviewed, not validated.
//
// Naming one in the validator would do to the operating system what
// naming a single architecture did to the machine: a host that ran the
// suites and passed would be refused for not being the one the file can
// express, and the refusal would read as a broken manifest rather than
// as an unqualified platform. What has to be true is that the choice is
// stated, because a policy missing a field records a selection nobody
// can check a host against.
func TestTheSelectionPolicyIsAChoiceNotARule(t *testing.T) {
	elsewhere := testPolicy()
	elsewhere.OS = "ubuntu"
	elsewhere.OSVersion = "24.04"
	elsewhere.OSCodename = "noble"
	elsewhere.DockerSource = "https://download.docker.com/linux/ubuntu"
	if err := (Qualified{Status: ReferenceStatusPending, Policy: elsewhere}).validate(); err != nil {
		t.Errorf("a qualification on another distribution was refused: %v.\n"+
			"The gate is meant to measure the host, and a host it will not let anyone "+
			"record is a host it is not measuring.", err)
	}

	for _, blank := range []func(*Policy){
		func(p *Policy) { p.OS = "" },
		func(p *Policy) { p.OSVersion = "" },
		func(p *Policy) { p.OSCodename = "" },
		func(p *Policy) { p.DockerChannel = "" },
		func(p *Policy) { p.DockerSource = "" },
		func(p *Policy) { p.Selection = "" },
		func(p *Policy) { p.TargetEngine = "" },
		func(p *Policy) { p.Reviewed = "" },
	} {
		policy := testPolicy()
		blank(&policy)
		if err := (Qualified{Status: ReferenceStatusPending, Policy: policy}).validate(); err == nil {
			t.Errorf("a selection policy missing a field validated: %+v", policy)
		}
	}

	// The freeze point stays a rule. A reference reviewed after the
	// candidate exists is a reference the candidate could have been
	// built against.
	late := testPolicy()
	late.FreezePoint = "after-release-candidate"
	if err := (Qualified{Status: ReferenceStatusPending, Policy: late}).validate(); err == nil {
		t.Error("a reference frozen after the candidate validated, so the evidence could have " +
			"been written to fit what it was meant to judge")
	}
}

// TestAnyBuildablePlatformCanBeRecorded: the record has to be able to
// hold a qualification for every platform a release can produce.
//
// Only one is recorded today, so nothing else exercises the entries the
// file does not yet contain — and a check narrowed back to the one that
// is there would look correct against this file for as long as it stays
// the only one.
func TestAnyBuildablePlatformCanBeRecorded(t *testing.T) {
	for _, arch := range BuildableArches() {
		policy := testPolicy()
		policy.Arch = arch
		if err := (Qualified{Status: ReferenceStatusPending, Policy: policy}).validate(); err != nil {
			t.Errorf("a qualification for %s was refused: %v.\nA release builds for it, so a "+
				"host that ran the suites there has nowhere to be recorded.", arch, err)
		}
	}
	unbuildable := testPolicy()
	unbuildable.Arch = "riscv64"
	if err := (Qualified{Status: ReferenceStatusPending, Policy: unbuildable}).validate(); err == nil {
		t.Error("a qualification was accepted for a platform no release builds for, so the " +
			"record would claim evidence about something nobody can run")
	}
}
