package qualification

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/platform"
)

// update rewrites the golden record rather than comparing against it,
// for when the document's shape changes on purpose.
var update = flag.Bool("update", false, "rewrite the golden release-qualification record")

const (
	controllerImage = "ghcr.io/rhobuild/runpool@sha256:" +
		"1111111111111111111111111111111111111111111111111111111111111111"
	capsuleImage = "ghcr.io/rhobuild/runpool/capsule@sha256:" +
		"2222222222222222222222222222222222222222222222222222222222222222"
)

func testBuild() Build {
	return Build{
		Commit:          "0123456789abcdef0123456789abcdef01234567",
		ControllerImage: controllerImage,
		CapsuleImage:    capsuleImage,
		Run:             "https://github.com/rhobuild/runpool/actions/runs/1",
	}
}

func qualifiedAt() time.Time {
	return time.Date(2026, 8, 18, 9, 14, 7, 0, time.UTC)
}

func boolFact(value bool) *bool { return &value }

// frozenReference records two platforms, and the one the fixture facts
// name is second: a reader that takes the first entry rather than the
// entry for the architecture the host reported would pair one platform's
// policy with another's facts, and a single-entry reference cannot tell
// the two apart.
func frozenReference() platform.Reference {
	policy := func(arch string) platform.Policy {
		return platform.Policy{
			OS: "debian", OSVersion: "13", OSCodename: "trixie", Arch: arch,
			DockerChannel: "stable",
			DockerSource:  "https://download.docker.com/linux/debian",
			Selection:     "latest-stable-at-policy-review",
			TargetEngine:  "29.7.2",
			Reviewed:      "2026-08-16",
			FreezePoint:   "before-release-candidate",
		}
	}
	facts := func(arch, kernel string) platform.Facts {
		return platform.Facts{
			OS: "debian", OSVersion: "13", OSCodename: "trixie", Arch: arch,
			Kernel: kernel, Engine: "29.7.2", API: "1.53",
			CgroupVersion: "2", CgroupDriver: "systemd",
			StorageDriver: "overlayfs", BackingFilesystem: "extfs",
			Rootless: boolFact(false), Containerd: "2.2.0", Runc: "1.4.2",
			Buildx: "v0.32.0", Compose: "2.42.0",
			IPTables: "iptables v1.8.11 (nf_tables)",
			NFTables: "nftables v1.1.1 (Commodore Bullmoose)",
		}
	}
	return platform.Reference{
		SchemaVersion: 2,
		Platforms: []platform.Qualified{
			{
				Status: platform.ReferenceStatusFrozen, Policy: policy("arm64"),
				Recorded: "2026-08-17", Platform: facts("arm64", "6.12.48+deb13-arm64"),
			},
			{
				Status: platform.ReferenceStatusFrozen, Policy: policy("amd64"),
				Recorded: "2026-08-18", Platform: facts("amd64", "6.12.48+deb13-amd64"),
			},
		},
	}
}

func evidence() string { return filepath.Join("testdata", "evidence") }

// evidenceCopy is a writable copy of the fixture tree, for the cases
// that need one piece of evidence to be wrong.
func evidenceCopy(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "evidence")
	if err := os.CopyFS(dir, os.DirFS(evidence())); err != nil {
		t.Fatalf("copy the fixture evidence: %v", err)
	}
	return dir
}

func rewriteJSON(t *testing.T, path string, edit func(map[string]any)) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	edit(document)
	if body, err = json.Marshal(document); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestTheRecordIsTheDocumentItPromises: the record's JSON is the
// artifact a release is authorized against, so its shape is a contract
// between the job that writes it and the job that reads it back — and
// with the golden file, between this build and the one that reads an
// older record.
func TestTheRecordIsTheDocumentItPromises(t *testing.T) {
	document, err := Assemble(frozenReference(), evidence(), testBuild(), qualifiedAt())
	if err != nil {
		t.Fatalf("assembling from complete evidence failed: %v", err)
	}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "record.golden.json")
	if *update {
		if err := os.WriteFile(golden, append(encoded, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded)+"\n" != string(want) {
		t.Errorf("the record differs from %s:\n%s", golden, encoded)
	}
}

// TestTheEntryComesFromTheArchitectureTheHostReported: selection reads
// the observed facts, not the reference's order and not the architecture
// this test process runs on. A record pairing one platform's policy with
// another's facts is a wrong qualification rather than a missing one, and
// nothing downstream re-derives it.
func TestTheEntryComesFromTheArchitectureTheHostReported(t *testing.T) {
	document, err := Assemble(frozenReference(), evidence(), testBuild(), qualifiedAt())
	if err != nil {
		t.Fatal(err)
	}
	if document.PlatformPolicy.Arch != "amd64" {
		t.Errorf("the record carries the %s policy; the facts report amd64",
			document.PlatformPolicy.Arch)
	}
	if document.PlatformReference.Arch != "amd64" {
		t.Errorf("the record carries %s reference facts; the facts report amd64",
			document.PlatformReference.Arch)
	}
	if document.PlatformObserved.Kernel != "6.12.48+deb13-amd64" {
		t.Errorf("observed facts = %+v; they are the file's, verbatim", document.PlatformObserved)
	}
}

// TestTheReferenceFactsAreTheReviewedOnesNotTheHostsOwn: the record
// states two sets of facts, and confusing them is how a qualification
// comes to compare a host with itself.
//
// The reference is what somebody reviewed and froze; the observed is
// what the host reported. In a passing run they agree, which is exactly
// why a test whose fixture has them equal cannot tell one from the
// other. Here they differ, so the record has to name the source of each.
func TestTheReferenceFactsAreTheReviewedOnesNotTheHostsOwn(t *testing.T) {
	const reviewedKernel = "6.12.47+deb13-amd64"
	reference := frozenReference()
	reference.Platforms[1].Platform.Kernel = reviewedKernel

	document, err := Assemble(reference, evidence(), testBuild(), qualifiedAt())
	if err != nil {
		t.Fatal(err)
	}
	if document.PlatformReference.Kernel != reviewedKernel {
		t.Errorf("the reference kernel is %q; it is the reviewed entry's, never the host's",
			document.PlatformReference.Kernel)
	}
	if document.PlatformObserved.Kernel != "6.12.48+deb13-amd64" {
		t.Errorf("the observed kernel is %q; it is the host's, verbatim",
			document.PlatformObserved.Kernel)
	}
}

// TestAssembleRefusesWhatItCannotStandBehind: a record that exists is a
// record of something that happened, so each case below is evidence that
// does not support the claim the record would make.
func TestAssembleRefusesWhatItCannotStandBehind(t *testing.T) {
	e2ePath := func(dir string) string {
		return filepath.Join(dir, "controller-e2e", "controller-e2e-1.json")
	}
	for name, testCase := range map[string]struct {
		reference platform.Reference
		breakIt   func(t *testing.T, dir string)
		want      string
	}{
		"a platform nobody qualified": {
			breakIt: func(t *testing.T, dir string) {
				rewriteJSON(t, filepath.Join(dir, "live", "platform-facts.json"),
					func(facts map[string]any) { facts["arch"] = "riscv64" })
			},
			want: "no release qualification is recorded for riscv64",
		},
		"an entry that is still pending": {
			reference: platform.Reference{
				SchemaVersion: 2,
				Platforms: []platform.Qualified{{
					Status: platform.ReferenceStatusPending,
					Policy: frozenReference().Platforms[1].Policy,
				}},
			},
			want: "is pending",
		},
		"an end-to-end run that did not pass": {
			breakIt: func(t *testing.T, dir string) {
				rewriteJSON(t, e2ePath(dir), func(e2e map[string]any) { e2e["outcome"] = "failed" })
			},
			want: "not a pass",
		},
		"an end-to-end run of another controller": {
			breakIt: func(t *testing.T, dir string) {
				rewriteJSON(t, e2ePath(dir),
					func(e2e map[string]any) { e2e["controller_image"] = controllerImage + "0" })
			},
			want: "covered controller",
		},
		"an end-to-end run of another capsule": {
			breakIt: func(t *testing.T, dir string) {
				rewriteJSON(t, e2ePath(dir),
					func(e2e map[string]any) { e2e["capsule_image"] = capsuleImage + "0" })
			},
			want: "covered capsule",
		},
		"two end-to-end records": {
			breakIt: func(t *testing.T, dir string) {
				body, err := os.ReadFile(e2ePath(dir))
				if err != nil {
					t.Fatal(err)
				}
				second := filepath.Join(dir, "controller-e2e", "controller-e2e-2.json")
				if err := os.WriteFile(second, body, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "found 2",
		},
		"no end-to-end record at all": {
			breakIt: func(t *testing.T, dir string) {
				if err := os.Remove(e2ePath(dir)); err != nil {
					t.Fatal(err)
				}
			},
			want: "found 0",
		},
		"no artifact checksums": {
			breakIt: func(t *testing.T, dir string) {
				path := filepath.Join(dir, "release-artifacts", "checksums.txt")
				if err := os.WriteFile(path, []byte("\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "lists no artifact",
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := evidence()
			if testCase.breakIt != nil {
				dir = evidenceCopy(t)
				testCase.breakIt(t, dir)
			}
			reference := testCase.reference
			if len(reference.Platforms) == 0 {
				reference = frozenReference()
			}
			document, err := Assemble(reference, dir, testBuild(), qualifiedAt())
			if err == nil {
				t.Fatalf("a record was assembled from evidence that does not support it: %+v", document)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Errorf("refused with %q; the reason has to name what was wrong (%q)",
					err, testCase.want)
			}
		})
	}
}

// TestTheRecordItAssemblesIsTheRecordItVerifies: the two ends run in
// different jobs and meet only through the artifact, so the round trip
// through JSON is the part no single end can prove.
func TestTheRecordItAssemblesIsTheRecordItVerifies(t *testing.T) {
	document, err := Assemble(frozenReference(), evidence(), testBuild(), qualifiedAt())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	var readBack Record
	if err := json.Unmarshal(encoded, &readBack); err != nil {
		t.Fatalf("the record the publication side reads is not the record written: %v", err)
	}
	if err := readBack.CoversBuild(testBuild()); err != nil {
		t.Fatalf("the publication side refused the record the qualification side wrote: %v", err)
	}

	for field, publishing := range map[string]Build{
		"commit":           {Commit: testBuild().Commit + "0", ControllerImage: controllerImage, CapsuleImage: capsuleImage},
		"controller_image": {Commit: testBuild().Commit, ControllerImage: controllerImage + "0", CapsuleImage: capsuleImage},
		"capsule_image":    {Commit: testBuild().Commit, ControllerImage: controllerImage, CapsuleImage: capsuleImage + "0"},
	} {
		err := readBack.CoversBuild(publishing)
		if err == nil {
			t.Errorf("a publication for another %s was authorized by this record", field)
			continue
		}
		if !strings.Contains(err.Error(), field) {
			t.Errorf("refused with %q; the reason has to name the field that differs", err)
		}
	}
}

// TestAnUnstatedIdentityIsNotAMatch: two empty strings compare equal.
//
// The publication side reads its identity from the environment, so a
// variable that never reaches the job arrives as an empty string. Against
// a record that also names nothing there — one written by a build whose
// own variables were unset — a plain equality check returns success
// having compared nothing. It is the only arrangement where this gate
// passes while doing its job backwards, and it needs both sides blank,
// which is why a case that blanks one of them proves nothing.
func TestAnUnstatedIdentityIsNotAMatch(t *testing.T) {
	covering := Record{
		Commit:          testBuild().Commit,
		ControllerImage: controllerImage,
		CapsuleImage:    capsuleImage,
	}
	for field, blank := range map[string]func(*Record, *Build){
		"commit": func(r *Record, b *Build) { r.Commit, b.Commit = "", "" },
		"controller_image": func(r *Record, b *Build) {
			r.ControllerImage, b.ControllerImage = "", ""
		},
		"capsule_image": func(r *Record, b *Build) { r.CapsuleImage, b.CapsuleImage = "", "" },
	} {
		record, publishing := covering, testBuild()
		blank(&record, &publishing)
		err := record.CoversBuild(publishing)
		if err == nil {
			t.Errorf("a publication stating no %s was authorized by a record naming none either",
				field)
			continue
		}
		if !strings.Contains(err.Error(), field) {
			t.Errorf("refused with %q; the reason has to name the identity that is missing", err)
		}
	}
}

// TestTheEmbeddedReferenceIsTheOneARecordIsBuiltFrom: the reference this
// reads is the one the controller ships with, so the record cannot be
// assembled against a lock the product does not have.
//
// Either verdict is a pass — a record, or a refusal that names the
// platform — because the reference is pending today and frozen later,
// and both are answers about it.
func TestTheEmbeddedReferenceIsTheOneARecordIsBuiltFrom(t *testing.T) {
	reference, err := platform.Load()
	if err != nil {
		t.Fatalf("the embedded reference does not load: %v", err)
	}
	document, err := Assemble(reference, evidence(), testBuild(), qualifiedAt())
	if err == nil {
		if document.PlatformPolicy.Arch != document.PlatformObserved.Arch {
			t.Errorf("the record pairs the %s policy with %s facts",
				document.PlatformPolicy.Arch, document.PlatformObserved.Arch)
		}
		return
	}
	if !strings.Contains(err.Error(), "amd64") {
		t.Errorf("refused with %q; a refusal about the shipped reference has to name the "+
			"platform it is about", err)
	}
}
