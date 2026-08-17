package platform

import (
	"bytes"
	"os"
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
	if ref.Status != ReferenceStatusPending {
		t.Errorf("status = %q; want pending until the server facts are frozen", ref.Status)
	}
	if ref.Policy.TargetEngine != "29.7.2" || ref.Policy.DockerChannel != "stable" {
		t.Errorf("Docker policy = %+v; want stable Engine 29.7.2", ref.Policy)
	}
	if err := ref.RequireFrozen(); err == nil {
		t.Fatal("a pending reference was accepted for release qualification")
	}
	if got := ref.Compare(Facts{}); len(got) != 1 || got[0].Property != "reference_status" {
		t.Fatalf("pending comparison = %v; want a reference_status mismatch", got)
	}
}

func TestManifestRequiresEveryFrozenFact(t *testing.T) {
	bad := Reference{
		SchemaVersion: 1,
		Status:        ReferenceStatusFrozen,
		Policy:        testPolicy(),
		Recorded:      "2026-08-14",
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

func frozenReference() Reference {
	return Reference{
		SchemaVersion: 1,
		Status:        ReferenceStatusFrozen,
		Policy:        testPolicy(),
		Recorded:      "2026-08-14",
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
