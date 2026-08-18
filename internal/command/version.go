package command

import (
	"fmt"
	"regexp"
	"runtime/debug"
)

// Runpool versions are SemVer. The compatibility surface a release
// promises — the CLI, the configuration schema, the on-disk state and
// the operational contract — is exactly what SemVer describes, and a
// date-based scheme would say nothing about any of it.
var semverPattern = regexp.MustCompile(
	`^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)` +
		`(-((0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(\.(0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?` +
		`(\+([0-9a-zA-Z-]+(\.[0-9a-zA-Z-]+)*))?$`)

// ValidReleaseVersion reports whether a tag may be published.
//
// "dev" is what a local build carries and is deliberately not valid:
// the check exists so a release cannot be cut from an unversioned or
// hand-typed string, and a build that has not been tagged has not been
// released.
func ValidReleaseVersion(v string) error {
	if v == "" {
		return fmt.Errorf("no version: a release must be published from a tag")
	}
	if v == "dev" {
		return fmt.Errorf("version %q is a local build, not a release", v)
	}
	if !semverPattern.MatchString(v) {
		return fmt.Errorf("version %q is not SemVer; a release tag looks like v1.0.0 or v1.0.0-rc.1", v)
	}
	return nil
}

// BuildInfoFromDebug fills in what the Go toolchain already recorded —
// the commit, the build time and whether the tree was dirty — so a
// local build reports the truth without a release pipeline stamping it.
// Values the linker stamped explicitly win, because a release states
// its own facts rather than inferring them.
func BuildInfoFromDebug(build BuildInfo) BuildInfo {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return build
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			if build.Commit == "" {
				build.Commit = setting.Value
			}
		case "vcs.time":
			if build.Built == "" {
				build.Built = setting.Value
			}
		case "vcs.modified":
			if setting.Value == "true" {
				build.Dirty = true
			}
		}
	}
	return build
}
