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
	"slices"
	"strings"
	"time"

	"github.com/rhobuild/runpool/internal/platform"
)

const recordSchemaVersion = 2

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
	Suites              []SuiteEvidence `json:"suites"`
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
	suiteEvidence, err := provenSuites(evidenceDir, build, observed)
	if err != nil {
		return Record{}, err
	}
	return Record{
		SchemaVersion:       recordSchemaVersion,
		Commit:              build.Commit,
		ControllerImage:     build.ControllerImage,
		CapsuleImage:        build.CapsuleImage,
		Run:                 build.Run,
		QualifiedAt:         at.UTC().Format(time.RFC3339),
		PlatformPolicy:      qualified.Policy,
		PlatformReference:   qualified.Platform,
		PlatformObserved:    observed,
		StandaloneArtifacts: checksums,
		Suites:              suiteEvidence,
	}, nil
}

// CoversBuild reports whether the record qualifies the build about to be
// published. A record naming another commit or another digest is a
// record of a qualification that happened, of something else.
func (r Record) CoversBuild(build Build) error {
	if err := r.validate(); err != nil {
		return err
	}
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

func (r Record) validate() error {
	if r.SchemaVersion != recordSchemaVersion {
		return fmt.Errorf("release-qualification record schema is %d; require %d", r.SchemaVersion, recordSchemaVersion)
	}
	if r.QualifiedAt == "" {
		return fmt.Errorf("release-qualification record states no qualification time")
	}
	if _, err := time.Parse(time.RFC3339, r.QualifiedAt); err != nil {
		return fmt.Errorf("release-qualification record has invalid qualification time %q: %w", r.QualifiedAt, err)
	}
	if len(r.StandaloneArtifacts) == 0 {
		return fmt.Errorf("release-qualification record lists no standalone artifact")
	}
	if len(r.Suites) != len(suiteDefinitions) {
		return fmt.Errorf("release-qualification record contains %d suites; require %d", len(r.Suites), len(suiteDefinitions))
	}
	recordedBuild := Build{
		Commit: r.Commit, ControllerImage: r.ControllerImage, CapsuleImage: r.CapsuleImage, Run: r.Run,
	}
	seen := make([]SuiteID, 0, len(r.Suites))
	for _, evidence := range r.Suites {
		if slices.Contains(seen, evidence.SuiteID) {
			return fmt.Errorf("release-qualification record contains suite %s more than once", evidence.SuiteID)
		}
		seen = append(seen, evidence.SuiteID)
		if err := ValidateSuiteEvidence(evidence, recordedBuild, r.PlatformObserved); err != nil {
			return fmt.Errorf("release-qualification record: %w", err)
		}
	}
	for _, definition := range suiteDefinitions {
		if !slices.Contains(seen, definition.id) {
			return fmt.Errorf("release-qualification record contains no suite %s", definition.id)
		}
	}
	return nil
}

// readControllerE2E requires exactly one end-to-end record, covering the
// images under qualification. A second record is a second run, and which
// one the release is authorized against would be whichever the directory
// happened to list first.
func readControllerE2E(evidenceDir string, build Build) error {
	pattern := filepath.Join(evidenceDir, "controller-e2e", "reference", "controller-e2e-*.json")
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

// provenSuites decodes and validates every suite manifest. A log is retained
// for diagnosis, but it is not evidence by itself: only the common manifest
// states which cases executed and binds them to this build and host.
func provenSuites(evidenceDir string, build Build, observed platform.Facts) ([]SuiteEvidence, error) {
	evidence := make([]SuiteEvidence, 0, len(suiteDefinitions))
	for _, definition := range suiteDefinitions {
		path := filepath.Join(evidenceDir, definition.evidencePath)
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("suite %s: open evidence %s: %w", definition.id, definition.evidencePath, err)
		}
		manifest, decodeErr := DecodeSuiteEvidence(file)
		closeErr := file.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("suite %s: decode evidence %s: %w", definition.id, definition.evidencePath, decodeErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("suite %s: close evidence %s: %w", definition.id, definition.evidencePath, closeErr)
		}
		if err := validateSuiteEvidence(manifest, definition, build, observed); err != nil {
			return nil, err
		}
		evidence = append(evidence, manifest)
	}
	return evidence, nil
}
