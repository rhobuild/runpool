package command

import "github.com/rhobuild/runpool/internal/platform"

// releaseQualificationReference summarizes the exact evidence host reported by
// `version --json`. Runtime compatibility remains a separate doctor check.
func releaseQualificationReference() map[string]string {
	ref, err := platform.Load()
	if err != nil {
		return map[string]string{"error": err.Error()}
	}
	if ref.Status != platform.ReferenceStatusFrozen {
		return map[string]string{
			"status":         ref.Status,
			"os":             ref.Policy.OS + " " + ref.Policy.OSVersion,
			"arch":           ref.Policy.Arch,
			"docker_channel": ref.Policy.DockerChannel,
			"selection":      ref.Policy.Selection,
			"target_engine":  ref.Policy.TargetEngine,
			"reviewed":       ref.Policy.Reviewed,
		}
	}
	return map[string]string{
		"status":   ref.Status,
		"os":       ref.Platform.OS + " " + ref.Platform.OSVersion,
		"arch":     ref.Platform.Arch,
		"engine":   ref.Platform.Engine,
		"cgroup":   "v" + ref.Platform.CgroupVersion + "/" + ref.Platform.CgroupDriver,
		"recorded": ref.Recorded,
	}
}
