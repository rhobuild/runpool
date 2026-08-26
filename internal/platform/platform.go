// Package platform owns the selection policy and exact host reference used for
// release qualification. The reference is frozen in build/platform.lock.json
// before a candidate and embedded here so every check reads the same reviewed
// answer.
//
// Runtime compatibility is enforced separately by doctor; this package
// records evidence and never constrains operators to the reference patch.
package platform

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// MinimumEngineMajor is the compatibility floor for ordinary runtime
// admission. Exact release evidence is compared separately against Reference.
const MinimumEngineMajor = 28

// ReferenceStatus is whether a platform entry's facts have been captured
// and reviewed. It gates the release: a candidate tag cannot be created
// while any entry is pending, and Compare refuses outright unless the
// entry is frozen -- so a status compared against the wrong vocabulary's
// word would report a reference nobody reviewed as reviewed.
type ReferenceStatus string

const (
	ReferenceStatusPending ReferenceStatus = "pending"
	ReferenceStatusFrozen  ReferenceStatus = "frozen"
)

//go:embed platform.lock.json
var manifestJSON []byte

// Buildable are the platforms a release can build for: the intersection
// of what the pinned upstream images publish. The runner image publishes
// linux/amd64 and linux/arm64 and nothing else, which is the ceiling;
// dind publishes more.
//
// The operating system is here because the images say so, not because
// anything in this package decided it. A capsule runs a Linux daemon and
// a Linux runner inside a container, so there is no non-Linux variant of
// these images to build against — and if one were ever published, this
// list is where that would show, rather than in a rule written beside it.
//
// It is not the list of qualified platforms. A release may build for a
// platform nobody has run the suites on, and keeping the two apart is
// what lets either be said without implying the other.
var Buildable = []string{"linux/amd64", "linux/arm64"}

// BuildableArches is Buildable's architectures, for the entries in the
// qualification record, which name a host rather than an image.
func BuildableArches() []string {
	out := make([]string, 0, len(Buildable))
	for _, p := range Buildable {
		if _, arch, ok := strings.Cut(p, "/"); ok {
			out = append(out, arch)
		}
	}
	return out
}

// Reference is the reviewed release-qualification record: one entry per
// platform that was qualified, each with its own selection policy and,
// once frozen, its own exact facts.
//
// A list rather than one entry because qualification is evidence that
// the release ran correctly on a host, and there is one host's worth of
// facts per platform. Shaped as one, the file could not record a second
// qualification without changing the format that is itself the proof of
// the gate -- and a gate that refuses a host for not being the one
// platform its file can express is measuring the file, not the host.
type Reference struct {
	SchemaVersion int         `json:"schema_version"`
	Platforms     []Qualified `json:"platforms"`
}

// Qualified is one platform's selection policy and the facts observed on
// it. A pending entry keeps release gates closed for that platform until
// its facts have been captured and reviewed before a candidate tag.
type Qualified struct {
	Status   ReferenceStatus `json:"status"`
	Policy   Policy          `json:"policy"`
	Recorded string          `json:"recorded,omitempty"`
	Platform Facts           `json:"platform,omitempty"`
}

// For returns the entry qualified for an architecture.
func (r Reference) For(arch string) (Qualified, bool) {
	for _, q := range r.Platforms {
		if q.Policy.Arch == arch {
			return q, true
		}
	}
	return Qualified{}, false
}

// Arches lists the architectures this release records a qualification
// for, in the order the file names them. It is what a failure on an
// unqualified platform says instead of "wrong architecture": the
// distinction between a host that failed and a host nobody has run.
func (r Reference) Arches() []string {
	out := make([]string, 0, len(r.Platforms))
	for _, q := range r.Platforms {
		out = append(out, q.Policy.Arch)
	}
	return out
}

// NotQualified is the error a platform with no entry gets.
func (r Reference) NotQualified(arch string) error {
	return fmt.Errorf("no release qualification is recorded for %s; this release records %s",
		arch, strings.Join(r.Arches(), ", "))
}

// Policy records how the exact target is selected. It is distinct from the
// frozen facts: selection happens once, then the candidate is qualified
// against reviewed bytes rather than expectations derived from the host under
// test.
type Policy struct {
	OS            string `json:"os"`
	OSVersion     string `json:"os_version"`
	OSCodename    string `json:"os_codename"`
	Arch          string `json:"arch"`
	DockerChannel string `json:"docker_channel"`
	DockerSource  string `json:"docker_source"`
	Selection     string `json:"selection"`
	TargetEngine  string `json:"target_engine"`
	Reviewed      string `json:"reviewed"`
	FreezePoint   string `json:"freeze_point"`
}

// Facts are the platform's properties. Every one of them can change a
// container's behaviour, which is why they are recorded rather than
// summarised: engine patches, cgroup drivers, storage drivers, and backing
// filesystems can all change container behaviour materially.
type Facts struct {
	OS                string `json:"os"`
	OSVersion         string `json:"os_version"`
	OSCodename        string `json:"os_codename"`
	Arch              string `json:"arch"`
	Kernel            string `json:"kernel"`
	Engine            string `json:"engine"`
	API               string `json:"api"`
	CgroupVersion     string `json:"cgroup_version"`
	CgroupDriver      string `json:"cgroup_driver"`
	StorageDriver     string `json:"storage_driver"`
	BackingFilesystem string `json:"backing_filesystem"`
	Rootless          *bool  `json:"rootless"`
	Containerd        string `json:"containerd"`
	Runc              string `json:"runc"`
	Buildx            string `json:"buildx"`
	Compose           string `json:"compose"`
	IPTables          string `json:"iptables"`
	NFTables          string `json:"nftables"`
}

// Load returns the embedded manifest.
func Load() (Reference, error) {
	var ref Reference
	if err := json.Unmarshal(manifestJSON, &ref); err != nil {
		return ref, fmt.Errorf("platform manifest: %w", err)
	}
	if err := ref.validate(); err != nil {
		return ref, err
	}
	return ref, nil
}

// MustLoad is Load for callers that cannot proceed without it. The
// manifest is embedded and reviewed, so a failure is a build defect.
func MustLoad() Reference {
	ref, err := Load()
	if err != nil {
		panic(err)
	}
	return ref
}

func (r Reference) validate() error {
	if r.SchemaVersion != 2 {
		return fmt.Errorf("platform manifest schema_version %d is unsupported", r.SchemaVersion)
	}
	if len(r.Platforms) == 0 {
		return fmt.Errorf("platform manifest records no platform at all")
	}
	seen := map[string]bool{}
	for _, q := range r.Platforms {
		if seen[q.Policy.Arch] {
			return fmt.Errorf("platform manifest records %s twice; which entry qualifies a host "+
				"would be whichever came first", q.Policy.Arch)
		}
		seen[q.Policy.Arch] = true
		if err := q.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (q Qualified) validate() error {
	if !slices.Contains(BuildableArches(), q.Policy.Arch) {
		return fmt.Errorf("platform manifest qualifies %q, which no release builds for; "+
			"the pinned images publish %s", q.Policy.Arch, strings.Join(Buildable, ", "))
	}
	// Which distribution was selected is a reviewed choice, not a rule.
	// Naming one here would do to the operating system what naming a
	// single architecture did to the machine: a host that ran the suites
	// and passed would be refused for not being the one this file can
	// express, and the refusal would read as a broken manifest rather
	// than as an unqualified platform.
	//
	// What is required is that the choice is stated. A policy missing a
	// field records a selection nobody can check the host against.
	// Two of these are rules rather than choices, and stay values. The
	// channel is what the guide and the lock's own comment require --
	// evidence from a nightly build is evidence about something no
	// operator installs -- and the selection names how the target was
	// picked, which is the sentence the reviewer signed off.
	if q.Policy.DockerChannel != "stable" {
		return fmt.Errorf("the %s selection policy takes Docker from the %q channel; release "+
			"evidence comes from the stable channel an operator installs from", q.Policy.Arch,
			q.Policy.DockerChannel)
	}
	if q.Policy.Selection != "latest-stable-at-policy-review" {
		return fmt.Errorf("the %s selection policy selects by %q; that string is what a "+
			"reviewer signed off on, not free text", q.Policy.Arch, q.Policy.Selection)
	}
	missing := []string{}
	for name, value := range map[string]string{
		"os":            q.Policy.OS,
		"os_version":    q.Policy.OSVersion,
		"os_codename":   q.Policy.OSCodename,
		"docker_source": q.Policy.DockerSource,
		"target_engine": q.Policy.TargetEngine,
		"reviewed":      q.Policy.Reviewed,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return fmt.Errorf("the %s selection policy states no %s", q.Policy.Arch,
			strings.Join(missing, ", "))
	}
	// The freeze point is a rule rather than a choice: a lock reviewed
	// after the candidate exists is a lock the candidate could have been
	// built against.
	if q.Policy.FreezePoint != "before-release-candidate" {
		return fmt.Errorf("the %s selection policy freezes at %q; a reference reviewed after "+
			"the candidate exists is not evidence about it", q.Policy.Arch, q.Policy.FreezePoint)
	}
	switch q.Status {
	case ReferenceStatusPending:
		if q.Recorded != "" || q.Platform != (Facts{}) {
			return fmt.Errorf("pending platform manifest must not contain frozen facts for %s",
				q.Policy.Arch)
		}
		return nil
	case ReferenceStatusFrozen:
		// Continue below and require every exact fact.
	default:
		return fmt.Errorf("platform manifest status %q is unsupported for %s",
			q.Status, q.Policy.Arch)
	}

	// The facts have to be the platform the entry claims. Nothing else
	// relates the two: selection reads the policy and comparison reads
	// the facts, so an entry labelled one platform and frozen from
	// another qualifies neither -- the host that ran the suites is told
	// nobody qualified it, and a host of the claimed platform is told the
	// reference is something else. That is a wrong qualification rather
	// than a differently shaped one, and it is what naming a single
	// architecture used to make unrepresentable.
	for name, pair := range map[string][2]string{
		"arch":        {q.Policy.Arch, q.Platform.Arch},
		"os":          {q.Policy.OS, q.Platform.OS},
		"os_version":  {q.Policy.OSVersion, q.Platform.OSVersion},
		"os_codename": {q.Policy.OSCodename, q.Platform.OSCodename},
		// The engine belongs in this list for the reason above and was
		// missing from it. A test held the rule for one architecture by
		// reading that architecture out of the file; an entry added for
		// another, frozen from an engine its own policy did not select,
		// passed validation and qualified hosts against it.
		"engine": {q.Policy.TargetEngine, q.Platform.Engine},
	} {
		if want, got := pair[0], pair[1]; got != "" && got != want {
			return fmt.Errorf("the %s entry selects %s %q and froze %q; the facts are not from "+
				"the platform the entry claims", q.Policy.Arch, name, want, got)
		}
	}

	missing = nil
	for name, value := range map[string]string{
		"recorded":           q.Recorded,
		"os":                 q.Platform.OS,
		"os_version":         q.Platform.OSVersion,
		"os_codename":        q.Platform.OSCodename,
		"arch":               q.Platform.Arch,
		"kernel":             q.Platform.Kernel,
		"engine":             q.Platform.Engine,
		"api":                q.Platform.API,
		"cgroup_version":     q.Platform.CgroupVersion,
		"cgroup_driver":      q.Platform.CgroupDriver,
		"storage_driver":     q.Platform.StorageDriver,
		"backing_filesystem": q.Platform.BackingFilesystem,
		"containerd":         q.Platform.Containerd,
		"runc":               q.Platform.Runc,
		"buildx":             q.Platform.Buildx,
		"compose":            q.Platform.Compose,
		"iptables":           q.Platform.IPTables,
		"nftables":           q.Platform.NFTables,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("platform manifest is incomplete: %s", strings.Join(missing, ", "))
	}
	if q.Platform.Rootless == nil {
		return fmt.Errorf("platform manifest is incomplete: rootless")
	}
	return nil
}

// RequireFrozen rejects release qualification until an independently reviewed
// set of exact host facts has replaced the pending reference.
func (q Qualified) RequireFrozen() error {
	if q.Status != ReferenceStatusFrozen {
		return fmt.Errorf("the %s release-qualification reference is %s; capture and review the "+
			"installed stable Docker platform before creating a candidate", q.Policy.Arch, q.Status)
	}
	return nil
}

// Mismatch is one property where an observed host differs from the reference.
type Mismatch struct {
	Property string
	Want     string
	Got      string
}

func (m Mismatch) String() string {
	return fmt.Sprintf("%s: reference %q, host has %q", m.Property, m.Want, m.Got)
}

// Compare reports every observed fact that differs from the qualification
// reference. Missing observations are mismatches because absent evidence
// cannot qualify a release.
func (q Qualified) Compare(observed Facts) []Mismatch {
	if q.Status != ReferenceStatusFrozen {
		return []Mismatch{{Property: "reference_status", Want: string(ReferenceStatusFrozen), Got: string(q.Status)}}
	}
	var out []Mismatch
	check := func(property, want, got string) {
		if got != want {
			out = append(out, Mismatch{Property: property, Want: want, Got: got})
		}
	}
	check("os", q.Platform.OS, observed.OS)
	check("os_version", q.Platform.OSVersion, observed.OSVersion)
	check("os_codename", q.Platform.OSCodename, observed.OSCodename)
	check("arch", q.Platform.Arch, observed.Arch)
	check("kernel", q.Platform.Kernel, observed.Kernel)
	check("engine", q.Platform.Engine, observed.Engine)
	check("api", q.Platform.API, observed.API)
	check("cgroup_version", q.Platform.CgroupVersion, observed.CgroupVersion)
	check("cgroup_driver", q.Platform.CgroupDriver, observed.CgroupDriver)
	check("storage_driver", q.Platform.StorageDriver, observed.StorageDriver)
	check("backing_filesystem", q.Platform.BackingFilesystem, observed.BackingFilesystem)
	check("containerd", q.Platform.Containerd, observed.Containerd)
	check("runc", q.Platform.Runc, observed.Runc)
	check("buildx", q.Platform.Buildx, observed.Buildx)
	check("compose", q.Platform.Compose, observed.Compose)
	check("iptables", q.Platform.IPTables, observed.IPTables)
	check("nftables", q.Platform.NFTables, observed.NFTables)
	if mismatch, ok := compareBoolFact("rootless", q.Platform.Rootless, observed.Rootless); ok {
		out = append(out, mismatch)
	}
	return out
}

// CompareDockerFacts is the subset of Compare observable through the Docker
// API. Qualification contracts use it when they need daemon-only evidence.
func (q Qualified) CompareDockerFacts(observed Facts) []Mismatch {
	if q.Status != ReferenceStatusFrozen {
		return []Mismatch{{Property: "reference_status", Want: string(ReferenceStatusFrozen), Got: string(q.Status)}}
	}
	var out []Mismatch
	check := func(property, want, got string) {
		if got != want {
			out = append(out, Mismatch{Property: property, Want: want, Got: got})
		}
	}
	check("engine", q.Platform.Engine, observed.Engine)
	check("api", q.Platform.API, observed.API)
	check("arch", q.Platform.Arch, observed.Arch)
	check("cgroup_version", q.Platform.CgroupVersion, observed.CgroupVersion)
	check("cgroup_driver", q.Platform.CgroupDriver, observed.CgroupDriver)
	if mismatch, ok := compareBoolFact("rootless", q.Platform.Rootless, observed.Rootless); ok {
		out = append(out, mismatch)
	}
	return out
}

func compareBoolFact(property string, want, got *bool) (Mismatch, bool) {
	if want != nil && got != nil && *want == *got {
		return Mismatch{}, false
	}
	format := func(value *bool) string {
		if value == nil {
			return "<missing>"
		}
		return fmt.Sprint(*value)
	}
	return Mismatch{Property: property, Want: format(want), Got: format(got)}, true
}
