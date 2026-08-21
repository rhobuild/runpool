package command

import (
	"runtime"
	"strings"

	"github.com/rhobuild/runpool/internal/platform"
)

// releaseQualificationReference summarizes the exact evidence host
// reported by `version --json`. Runtime compatibility remains a separate
// doctor check.
//
// It reports the entry for the architecture this binary is running on,
// because that is the one an operator reading it is asking about. A
// build running where nothing was qualified says so and names what was,
// which is a different answer from a host that was measured and
// differed.
func releaseQualificationReference() map[string]string {
	ref, err := platform.Load()
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	qualified, ok := ref.For(runtime.GOARCH)
	if !ok {
		return map[string]string{
			"status":    "not-qualified-here",
			"arch":      runtime.GOARCH,
			"qualified": strings.Join(ref.Arches(), ", "),
		}
	}
	if qualified.Status != platform.ReferenceStatusFrozen {
		return map[string]string{
			"status":         qualified.Status,
			"os":             qualified.Policy.OS + " " + qualified.Policy.OSVersion,
			"arch":           qualified.Policy.Arch,
			"docker_channel": qualified.Policy.DockerChannel,
			"selection":      qualified.Policy.Selection,
			"target_engine":  qualified.Policy.TargetEngine,
			"reviewed":       qualified.Policy.Reviewed,
			"qualified":      strings.Join(ref.Arches(), ", "),
		}
	}
	return map[string]string{
		"status":    qualified.Status,
		"os":        qualified.Platform.OS + " " + qualified.Platform.OSVersion,
		"arch":      qualified.Platform.Arch,
		"engine":    qualified.Platform.Engine,
		"cgroup":    "v" + qualified.Platform.CgroupVersion + "/" + qualified.Platform.CgroupDriver,
		"recorded":  qualified.Recorded,
		"qualified": strings.Join(ref.Arches(), ", "),
	}
}
