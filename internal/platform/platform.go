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
	"strings"
)

// MinimumEngineMajor is the compatibility floor for ordinary runtime
// admission. Exact release evidence is compared separately against Reference.
const MinimumEngineMajor = 28

const (
	ReferenceStatusPending = "pending"
	ReferenceStatusFrozen  = "frozen"
)

//go:embed platform.lock.json
var manifestJSON []byte

// Reference is the reviewed release-qualification target. A pending reference
// keeps release gates closed until the operator facts have been captured and
// frozen in the repository before a candidate tag is created.
type Reference struct {
	SchemaVersion int    `json:"schema_version"`
	Status        string `json:"status"`
	Policy        Policy `json:"policy"`
	Recorded      string `json:"recorded,omitempty"`
	Platform      Facts  `json:"platform,omitempty"`
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
	if r.SchemaVersion != 1 {
		return fmt.Errorf("platform manifest schema_version %d is unsupported", r.SchemaVersion)
	}
	if r.Policy.OS != "debian" || r.Policy.OSVersion != "13" ||
		r.Policy.OSCodename != "trixie" || r.Policy.Arch != "amd64" ||
		r.Policy.DockerChannel != "stable" || r.Policy.DockerSource == "" ||
		r.Policy.Selection != "latest-stable-at-policy-review" || r.Policy.TargetEngine == "" ||
		r.Policy.Reviewed == "" || r.Policy.FreezePoint != "before-release-candidate" {
		return fmt.Errorf("platform manifest has an invalid selection policy")
	}
	switch r.Status {
	case ReferenceStatusPending:
		if r.Recorded != "" || r.Platform != (Facts{}) {
			return fmt.Errorf("pending platform manifest must not contain frozen facts")
		}
		return nil
	case ReferenceStatusFrozen:
		// Continue below and require every exact fact.
	default:
		return fmt.Errorf("platform manifest status %q is unsupported", r.Status)
	}

	missing := []string{}
	for name, value := range map[string]string{
		"recorded":           r.Recorded,
		"os":                 r.Platform.OS,
		"os_version":         r.Platform.OSVersion,
		"os_codename":        r.Platform.OSCodename,
		"arch":               r.Platform.Arch,
		"kernel":             r.Platform.Kernel,
		"engine":             r.Platform.Engine,
		"api":                r.Platform.API,
		"cgroup_version":     r.Platform.CgroupVersion,
		"cgroup_driver":      r.Platform.CgroupDriver,
		"storage_driver":     r.Platform.StorageDriver,
		"backing_filesystem": r.Platform.BackingFilesystem,
		"containerd":         r.Platform.Containerd,
		"runc":               r.Platform.Runc,
		"buildx":             r.Platform.Buildx,
		"compose":            r.Platform.Compose,
		"iptables":           r.Platform.IPTables,
		"nftables":           r.Platform.NFTables,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("platform manifest is incomplete: %s", strings.Join(missing, ", "))
	}
	if r.Platform.Rootless == nil {
		return fmt.Errorf("platform manifest is incomplete: rootless")
	}
	return nil
}

// RequireFrozen rejects release qualification until an independently reviewed
// set of exact host facts has replaced the pending reference.
func (r Reference) RequireFrozen() error {
	if r.Status != ReferenceStatusFrozen {
		return fmt.Errorf("release-qualification reference is %s; capture and review the installed stable Docker platform before creating a candidate", r.Status)
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
func (r Reference) Compare(observed Facts) []Mismatch {
	if r.Status != ReferenceStatusFrozen {
		return []Mismatch{{Property: "reference_status", Want: ReferenceStatusFrozen, Got: r.Status}}
	}
	var out []Mismatch
	check := func(property, want, got string) {
		if got != want {
			out = append(out, Mismatch{Property: property, Want: want, Got: got})
		}
	}
	check("os", r.Platform.OS, observed.OS)
	check("os_version", r.Platform.OSVersion, observed.OSVersion)
	check("os_codename", r.Platform.OSCodename, observed.OSCodename)
	check("arch", r.Platform.Arch, observed.Arch)
	check("kernel", r.Platform.Kernel, observed.Kernel)
	check("engine", r.Platform.Engine, observed.Engine)
	check("api", r.Platform.API, observed.API)
	check("cgroup_version", r.Platform.CgroupVersion, observed.CgroupVersion)
	check("cgroup_driver", r.Platform.CgroupDriver, observed.CgroupDriver)
	check("storage_driver", r.Platform.StorageDriver, observed.StorageDriver)
	check("backing_filesystem", r.Platform.BackingFilesystem, observed.BackingFilesystem)
	check("containerd", r.Platform.Containerd, observed.Containerd)
	check("runc", r.Platform.Runc, observed.Runc)
	check("buildx", r.Platform.Buildx, observed.Buildx)
	check("compose", r.Platform.Compose, observed.Compose)
	check("iptables", r.Platform.IPTables, observed.IPTables)
	check("nftables", r.Platform.NFTables, observed.NFTables)
	if mismatch, ok := compareBoolFact("rootless", r.Platform.Rootless, observed.Rootless); ok {
		out = append(out, mismatch)
	}
	return out
}

// CompareDockerFacts is the subset of Compare observable through the Docker
// API. Qualification contracts use it when they need daemon-only evidence.
func (r Reference) CompareDockerFacts(observed Facts) []Mismatch {
	if r.Status != ReferenceStatusFrozen {
		return []Mismatch{{Property: "reference_status", Want: ReferenceStatusFrozen, Got: r.Status}}
	}
	var out []Mismatch
	check := func(property, want, got string) {
		if got != want {
			out = append(out, Mismatch{Property: property, Want: want, Got: got})
		}
	}
	check("engine", r.Platform.Engine, observed.Engine)
	check("api", r.Platform.API, observed.API)
	check("arch", r.Platform.Arch, observed.Arch)
	check("cgroup_version", r.Platform.CgroupVersion, observed.CgroupVersion)
	check("cgroup_driver", r.Platform.CgroupDriver, observed.CgroupDriver)
	if mismatch, ok := compareBoolFact("rootless", r.Platform.Rootless, observed.Rootless); ok {
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
