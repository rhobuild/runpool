// Package qualification assembles and checks the release-qualification
// record: the document a release is authorized against.
//
// It reads the evidence the gates produced and states what was
// qualified — the commit, the two image digests, the platform entry the
// host was measured against, the facts it reported, and the artifact
// checksums. Nothing here re-derives a fact from an argument, because a
// record that exists has to be a record of something that happened.
//
// The two ends live together on purpose. Qualification writes the record
// and publication reads it back, one job and one artifact download
// apart, and the only thing tying them together is the document's shape.
// While each end was a reader of its own, that shape was an agreement
// nothing enforced; here it is one type.
package qualification

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rhobuild/runpool/internal/platform"
)

// suites are the gates a release-qualification run comprises. The record
// names them so that a reader of the document knows what "qualified"
// covered on the day it was written.
var suites = []string{
	"hermetic",
	"docker-contract",
	"capsule-contract",
	"sqlite-durability",
	"lifecycle-drills",
	"contracts-github-actions",
	"controller-e2e",
}

// Build identifies what is being qualified, and later what is being
// published. Both ends state it; a release happens where they agree.
type Build struct {
	Commit          string
	ControllerImage string
	CapsuleImage    string
	Run             string
}

// Record is the release-qualification document. Its JSON is the artifact
// the release workflow keeps, so the field names are a contract between
// the job that writes it and the job that reads it back.
type Record struct {
	SchemaVersion       int             `json:"schema_version"`
	Commit              string          `json:"commit"`
	ControllerImage     string          `json:"controller_image"`
	CapsuleImage        string          `json:"capsule_image"`
	Run                 string          `json:"run"`
	QualifiedAt         string          `json:"qualified_at"`
	PlatformPolicy      platform.Policy `json:"platform_policy"`
	PlatformReference   platform.Facts  `json:"platform_reference"`
	PlatformObserved    platform.Facts  `json:"platform_observed"`
	StandaloneArtifacts []string        `json:"standalone_artifacts"`
	Suites              []string        `json:"suites"`
}

// controllerE2E is the subset of the end-to-end record this reads: the
// outcome, and which images it covered.
type controllerE2E struct {
	Outcome         string `json:"outcome"`
	ControllerImage string `json:"controller_image"`
	CapsuleImage    string `json:"capsule_image"`
}

// Assemble builds the record from an evidence directory, at a moment.
//
// The platform entry is selected by the architecture the *facts* report,
// not by the one this process runs on: the facts came from a host, and
// it is that host being qualified. A platform with no entry is not a
// host that failed the reference — it is one nobody has run the suites
// on, and the two are reported differently.
func Assemble(ref platform.Reference, evidenceDir string, build Build, at time.Time) (Record, error) {
	var observed platform.Facts
	if err := readJSON(filepath.Join(evidenceDir, "live", "platform-facts.json"), &observed); err != nil {
		return Record{}, err
	}
	qualified, ok := ref.For(observed.Arch)
	if !ok {
		return Record{}, ref.NotQualified(observed.Arch)
	}
	if err := qualified.RequireFrozen(); err != nil {
		return Record{}, err
	}
	if err := readControllerE2E(evidenceDir, build); err != nil {
		return Record{}, err
	}
	checksums, err := readChecksums(filepath.Join(evidenceDir, "release-artifacts", "checksums.txt"))
	if err != nil {
		return Record{}, err
	}
	return Record{
		SchemaVersion:       1,
		Commit:              build.Commit,
		ControllerImage:     build.ControllerImage,
		CapsuleImage:        build.CapsuleImage,
		Run:                 build.Run,
		QualifiedAt:         at.UTC().Format(time.RFC3339),
		PlatformPolicy:      qualified.Policy,
		PlatformReference:   qualified.Platform,
		PlatformObserved:    observed,
		StandaloneArtifacts: checksums,
		Suites:              suites,
	}, nil
}

// CoversBuild reports whether the record qualifies the build about to be
// published. A record naming another commit or another digest is a
// record of a qualification that happened, of something else.
func (r Record) CoversBuild(build Build) error {
	for _, field := range []struct{ name, recorded, publishing string }{
		{"commit", r.Commit, build.Commit},
		{"controller_image", r.ControllerImage, build.ControllerImage},
		{"capsule_image", r.CapsuleImage, build.CapsuleImage},
	} {
		// An unstated identity is not a match. Two empty strings compare
		// equal, so a publication that names no commit would be
		// authorized by a record that names none either -- the one
		// arrangement where this returns success having compared
		// nothing.
		if field.publishing == "" {
			return fmt.Errorf("this publication states no %s, so no record can cover it", field.name)
		}
		if field.recorded != field.publishing {
			return fmt.Errorf("the release-qualification record names %s %q; this publication is for %q",
				field.name, field.recorded, field.publishing)
		}
	}
	return nil
}

// readControllerE2E requires exactly one end-to-end record, covering the
// images under qualification. A second record is a second run, and which
// one the release is authorized against would be whichever the directory
// happened to list first.
func readControllerE2E(evidenceDir string, build Build) error {
	pattern := filepath.Join(evidenceDir, "controller-e2e", "controller-e2e-*.json")
	found, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}
	if len(found) != 1 {
		return fmt.Errorf("expected one controller end-to-end record, found %d", len(found))
	}
	var e2e controllerE2E
	if err := readJSON(found[0], &e2e); err != nil {
		return err
	}
	if e2e.Outcome != "passed" {
		return fmt.Errorf("the controller end-to-end record reports %q, not a pass", e2e.Outcome)
	}
	if e2e.ControllerImage != build.ControllerImage {
		return fmt.Errorf("the controller end-to-end run covered controller %q, not %q",
			e2e.ControllerImage, build.ControllerImage)
	}
	if e2e.CapsuleImage != build.CapsuleImage {
		return fmt.Errorf("the controller end-to-end run covered capsule %q, not %q",
			e2e.CapsuleImage, build.CapsuleImage)
	}
	return nil
}

func readChecksums(path string) ([]string, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lines []string
	for line := range strings.SplitSeq(string(body), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("%s lists no artifact, so the record would claim a release "+
			"shipped nothing", path)
	}
	return lines, nil
}

func readJSON(path string, into any) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}
