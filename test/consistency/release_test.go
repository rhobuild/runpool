package consistency

import (
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestTheReleaseBuildsEveryPlatformTheLockDeclares: the lock says what a
// release builds for, and the release workflow is what builds it.
//
// Nothing else relates the two. `build/images.lock.json` declares the
// platforms, `internal/app` embeds that declaration, and a Go test holds
// it equal to the platforms the pinned base images publish — but the
// workflow that produces the artifacts reads none of it. A lock
// declaring two platforms beside a release that pushes one is a claim
// with nothing behind it, and the first reader to notice is an operator
// pulling an image for an architecture that was never published.
func TestTheReleaseBuildsEveryPlatformTheLockDeclares(t *testing.T) {
	var lock struct {
		Platforms []string `json:"platforms"`
	}
	if err := readRepoJSON(t, "build/images.lock.json", &lock); err != nil {
		t.Fatal(err)
	}
	if len(lock.Platforms) == 0 {
		t.Fatal("the image lock declares no platform, so this proves nothing")
	}

	body, err := os.ReadFile(repoPath(".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow yaml.Node
	if err := yaml.Unmarshal(body, &workflow); err != nil {
		t.Fatal(err)
	}
	built := valuesOf(&workflow, "platform")
	slices.Sort(built)
	built = slices.Compact(built)
	declared := slices.Clone(lock.Platforms)
	slices.Sort(declared)

	if !slices.Equal(built, declared) {
		t.Errorf("the release builds for %s; the lock declares %s. A platform in one list "+
			"and not the other is one a release claims and does not produce, or one it "+
			"produces and does not offer.",
			strings.Join(built, ", "), strings.Join(declared, ", "))
	}
}
