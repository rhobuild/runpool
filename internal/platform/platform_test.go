package platform

import (
	"bytes"
	"os"
	"reflect"
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
	if amd64.Policy.TargetEngine != "29.7.2" || amd64.Policy.DockerChannel != "stable" {
		t.Errorf("Docker policy = %+v; want stable Engine 29.7.2", amd64.Policy)
	}
	if amd64.Status != ReferenceStatusFrozen {
		t.Fatalf("status = %q; the entry is frozen and qualification reads it", amd64.Status)
	}
	if err := amd64.RequireFrozen(); err != nil {
		t.Errorf("the frozen reference was refused for qualification: %v", err)
	}
	// The facts are the platform the policy selects, and the engine that
	// ran the suites is the one the policy named. An entry frozen from
	// somewhere else qualifies neither host.
	if amd64.Platform.Engine != amd64.Policy.TargetEngine {
		t.Errorf("froze engine %q against a policy naming %q",
			amd64.Platform.Engine, amd64.Policy.TargetEngine)
	}
	if got := amd64.Compare(amd64.Platform); len(got) != 0 {
		t.Errorf("the reference mismatched its own facts: %v", got)
	}
}

// TestAPendingReferenceQualifiesNothing keeps the other half of what the
// test above used to say. It read those refusals off the lock, which
// held a pending entry then and holds a frozen one now -- so the rules
// are stated here, over an entry this test owns, rather than over a file
// whose contents are the thing under review.
func TestAPendingReferenceQualifiesNothing(t *testing.T) {
	pending := Qualified{Status: ReferenceStatusPending, Policy: testPolicy()}
	if err := pending.RequireFrozen(); err == nil {
		t.Error("a pending reference was accepted for release qualification")
	}
	got := pending.Compare(Facts{})
	if len(got) != 1 || got[0].Property != "reference_status" {
		t.Fatalf("pending comparison = %v; want a reference_status mismatch", got)
	}
	if got := pending.CompareDockerFacts(Facts{}); len(got) != 1 ||
		got[0].Property != "reference_status" {
		t.Errorf("pending runtime comparison = %v; want the same refusal", got)
	}
}

// TestAPlatformWithNoEntryIsNotAFailedOne: a host nobody has run the
// suites on and a host that ran them and differed are different answers,
// and the file's shape used to make only the second expressible.
func TestAPlatformWithNoEntryIsNotAFailedOne(t *testing.T) {
	// Two entries this test owns, rather than whatever the lock holds. An
	// assertion written against the file cannot tell "arm64 was qualified
	// later" from "selection is broken", and the version of this that
	// skipped on the first was switched off by the second.
	amd := frozenQualified()
	arm := frozenQualified()
	arm.Policy.Arch, arm.Platform.Arch = "arm64", "arm64"
	arm.Platform.Kernel = "6.12.0-arm64"
	ref := Reference{SchemaVersion: 2, Platforms: []Qualified{amd, arm}}
	if err := ref.validate(); err != nil {
		t.Fatalf("two platforms did not validate: %v", err)
	}

	for _, arch := range []string{"amd64", "arm64"} {
		got, ok := ref.For(arch)
		if !ok {
			t.Fatalf("no entry for %s in a record holding %v", arch, ref.Arches())
		}
		if got.Policy.Arch != arch {
			t.Errorf("For(%q) returned the %s entry; a host is then qualified against another "+
				"platform's facts, which is the failure this record exists to stop",
				arch, got.Policy.Arch)
		}
	}
	if _, ok := ref.For("riscv64"); ok {
		t.Error("a platform with no entry was matched to one")
	}

	err := ref.NotQualified("riscv64")
	if err == nil {
		t.Fatal("an unqualified platform produced no error")
	}
	for _, want := range []string{"riscv64", "amd64", "arm64"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error is %q; it has to name the platform asked about and the ones "+
				"that are qualified, or it reads as a broken release rather than an "+
				"unqualified host", err)
		}
	}
}

// TestAnEntryFrozenFromAnotherPlatformIsRefused: selection reads the
// policy and comparison reads the facts, so an entry labelled one
// platform and frozen from another qualifies neither.
//
// The host that ran the suites is told nobody qualified it, and a host of
// the claimed platform is told the reference is something else. Naming a
// single architecture in the reader used to make this unrepresentable,
// and nothing replaced that when the name came out.
func TestAnEntryFrozenFromAnotherPlatformIsRefused(t *testing.T) {
	for name, break_ := range map[string]func(*Qualified){
		"arch":        func(q *Qualified) { q.Platform.Arch = "arm64" },
		"os":          func(q *Qualified) { q.Platform.OS = "ubuntu" },
		"os_version":  func(q *Qualified) { q.Platform.OSVersion = "24.04" },
		"os_codename": func(q *Qualified) { q.Platform.OSCodename = "noble" },
		// The engine is the same rule and was missing from it: an entry
		// frozen from an engine its own policy did not select qualifies
		// hosts against a version nobody reviewed.
		"engine": func(q *Qualified) { q.Platform.Engine = "29.7.1" },
	} {
		q := frozenQualified()
		break_(&q)
		if err := q.validate(); err == nil {
			t.Errorf("an entry whose frozen %s differs from the one it selects validated; "+
				"the release would claim a platform it never measured", name)
		}
	}
}

// TestTwoEntriesForOnePlatformAreRefused: which one qualified a host
// would be whichever came first.
func TestTwoEntriesForOnePlatformAreRefused(t *testing.T) {
	ref := Reference{SchemaVersion: 2, Platforms: []Qualified{frozenQualified(), frozenQualified()}}
	if err := ref.validate(); err == nil {
		t.Error("one platform recorded twice validated, so a host is qualified against " +
			"whichever entry the file happens to list first")
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

	missingBoolean := frozenQualified()
	missingBoolean.Platform.Rootless = nil
	if err := missingBoolean.validate(); err == nil {
		t.Fatal("a frozen reference without the rootless observation validated")
	}
}

// TestCompareNamesEveryDifference keeps qualification evidence fail-closed.
func TestCompareNamesEveryDifference(t *testing.T) {
	ref := frozenQualified()

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
	ref := frozenQualified()
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

func frozenQualified() Qualified {
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
// file does not contain — and a check narrowed back to the one that
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

// TestEveryFactIsCompared walks the Facts struct itself, so the domain
// is the type and not a list a reader keeps in step by hand. The
// previous shape drifted exactly that way: three properties were driven
// and fifteen were not, so deleting any of the fifteen checks — kernel,
// backing filesystem, nftables among them — failed nothing, and Compare
// is what stands between the host that ran the suites and the host
// somebody froze. Walking the struct also makes a field added to Facts
// without a check a failure here rather than a silent hole.
func TestEveryFactIsCompared(t *testing.T) {
	ref := frozenQualified()
	facts := reflect.TypeOf(Facts{})

	for i := 0; i < facts.NumField(); i++ {
		field := facts.Field(i)
		property := strings.TrimSuffix(field.Tag.Get("json"), ",omitempty")
		t.Run(property, func(t *testing.T) {
			drifted := ref.Platform
			v := reflect.ValueOf(&drifted).Elem().Field(i)
			switch field.Type.Kind() {
			case reflect.String:
				v.SetString(v.String() + "-drifted")
			case reflect.Pointer:
				flipped := !v.Elem().Bool()
				v.Set(reflect.ValueOf(&flipped))
			default:
				t.Fatalf("Facts grew a %s field; teach this walk to drift it", field.Type.Kind())
			}

			got := ref.Compare(drifted)
			if len(got) != 1 {
				t.Fatalf("drifting %s alone produced %d mismatches: %v — the fact is not compared",
					property, len(got), got)
			}
			if got[0].Property != property {
				t.Errorf("the mismatch names %q; want %q, which is what an operator greps the lock for",
					got[0].Property, property)
			}
		})
	}
}

// TestEveryDockerFactIsCompared is the runtime half, over the five facts
// a serving daemon can answer for. Its previous test built the observed
// facts from the reference's own fields and asserted zero mismatches, so
// deleting all five checks passed it — and its only other caller is
// double-gated behind two environment variables.
func TestEveryDockerFactIsCompared(t *testing.T) {
	ref := frozenQualified()
	base := Facts{
		Engine: ref.Platform.Engine, API: ref.Platform.API, Arch: ref.Platform.Arch,
		CgroupVersion: ref.Platform.CgroupVersion, CgroupDriver: ref.Platform.CgroupDriver,
		Rootless: ref.Platform.Rootless,
	}
	if got := ref.CompareDockerFacts(base); len(got) != 0 {
		t.Fatalf("the matching runtime facts mismatched: %v", got)
	}
	for property, drift := range map[string]func(*Facts){
		"engine":         func(f *Facts) { f.Engine += "-drifted" },
		"api":            func(f *Facts) { f.API += "-drifted" },
		"arch":           func(f *Facts) { f.Arch += "-drifted" },
		"cgroup_version": func(f *Facts) { f.CgroupVersion += "-drifted" },
		"cgroup_driver":  func(f *Facts) { f.CgroupDriver += "-drifted" },
		"rootless":       func(f *Facts) { flipped := !*f.Rootless; f.Rootless = &flipped },
	} {
		t.Run(property, func(t *testing.T) {
			observed := base
			drift(&observed)
			got := ref.CompareDockerFacts(observed)
			if len(got) != 1 || got[0].Property != property {
				t.Errorf("drifting %s produced %v; want exactly that one mismatch", property, got)
			}
		})
	}
}

// TestTheRefusalsAValidatorOwesAreItsOwn covers the two branches nothing
// reached. A pending entry carrying frozen facts is a record claiming
// review that never happened, and a status word outside the vocabulary
// is what this type's own doc warns about: compared against the wrong
// word, a reference nobody reviewed reports as reviewed.
func TestTheRefusalsAValidatorOwesAreItsOwn(t *testing.T) {
	facts := Facts{
		OS: "debian", OSVersion: "13", OSCodename: "trixie", Arch: "amd64",
		Kernel: "k", Engine: "29.7.2", API: "1.55", CgroupVersion: "2",
		CgroupDriver: "systemd", StorageDriver: "overlayfs", BackingFilesystem: "ext4",
		Rootless: boolFact(false), Containerd: "c", Runc: "r", Buildx: "b",
		Compose: "co", IPTables: "ipt", NFTables: "nft",
	}
	pending := Qualified{Status: ReferenceStatusPending, Policy: testPolicy(),
		Recorded: "2026-08-26", Platform: facts}
	if err := pending.validate(); err == nil {
		t.Error("a pending entry carrying frozen facts validated; it claims a review nobody did")
	}
	unknown := Qualified{Status: "reviewed", Policy: testPolicy()}
	if err := unknown.validate(); err == nil {
		t.Error("a status outside the vocabulary validated; the wrong word reports an " +
			"unreviewed reference as reviewed")
	}
}

func TestBuildablePlatformsReturnsASnapshot(t *testing.T) {
	platforms := BuildablePlatforms()
	want := platforms[0]
	platforms[0] = "caller/mutation"
	if got := BuildablePlatforms()[0]; got != want {
		t.Errorf("BuildablePlatforms()[0] = %q after caller mutation; want %q", got, want)
	}
}
