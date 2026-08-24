package imagelock

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/rhobuild/runpool/internal/platform"
)

// The embedded lock is what the controller executes; the reviewed lock
// under build/ is what humans audit. If they diverge, the review applied
// to bytes that do not run. Byte equality, not structural equality: two
// JSON documents can be structurally equal while only one of them was
// reviewed.
func TestEmbeddedImageLockMatchesTheReviewedCopy(t *testing.T) {
	reviewed, err := os.ReadFile("../../build/images.lock.json")
	if err != nil {
		t.Fatalf("read the reviewed lock: %v", err)
	}
	if !bytes.Equal(reviewed, reviewedJSON) {
		t.Error("internal/imagelock/images.lock.json differs from build/images.lock.json; " +
			"the embedded copy is what runs, so the reviewed copy must be identical")
	}
}

// The lock records the privileged payload a capsule runs, but the capsule is
// built from the FROM lines in its Dockerfile. Nothing else compares the two:
// the release gate re-resolves the lock against its registry and never opens
// a Dockerfile, so a dependency update that moves one and not the other keeps
// every check green while the reviewed document stops describing what runs.
func TestLockedImagesAreWhatTheCapsuleIsBuiltFrom(t *testing.T) {
	dockerfile, err := os.ReadFile("../../build/capsule/Dockerfile")
	if err != nil {
		t.Fatalf("read the capsule Dockerfile: %v", err)
	}
	lock, err := Reviewed()
	if err != nil {
		t.Fatalf("parse the image lock: %v", err)
	}
	if len(lock.Images) == 0 {
		t.Fatal("the image lock is empty, so this proves nothing")
	}
	for name, entry := range lock.Images {
		want := entry.Ref + "@" + entry.Digest
		if !strings.Contains(string(dockerfile), want) {
			t.Errorf("build/capsule/Dockerfile does not build from the locked %s image %q; "+
				"the lock is the reviewed record of what runs privileged, so a Dockerfile "+
				"that names other bytes makes that record false", name, want)
		}
	}
}

// TestTheLockBuildsForEveryPlatformAReleaseCan: what a release builds
// for is bounded by what the pinned images publish.
//
// The two locks answer different questions — this one what is built,
// build/platform.lock.json what was qualified — and neither promises the
// other. What they cannot disagree about is the ceiling: an image the
// upstream does not publish for a platform cannot be built for it, so a
// release declaring one would fail at the registry rather than here.
func TestTheLockBuildsForEveryPlatformAReleaseCan(t *testing.T) {
	lock, err := Reviewed()
	if err != nil {
		t.Fatalf("parse the image lock: %v", err)
	}
	if len(lock.Platforms) == 0 {
		t.Fatal("the lock declares no platform, so a release builds for nothing it states")
	}
	declared := slices.Clone(lock.Platforms)
	want := slices.Clone(platform.Buildable)
	slices.Sort(declared)
	slices.Sort(want)
	if !slices.Equal(declared, want) {
		t.Errorf("the lock builds for %v; the pinned images publish %v. A platform in neither "+
			"list is one a release claims and cannot produce, or one it could produce and "+
			"does not offer.", declared, want)
	}
	// Whole platform strings, operating system included, and compared
	// against what the images publish rather than against a rule written
	// here. A capsule runs a Linux daemon and a Linux runner, so there is
	// no non-Linux variant to build against — and if one were ever
	// published, that list is where it would show.
}

// TestPinnedStripsTagKeepsRegistryPort holds the rule the whole package
// exists to have stated once: a reference is cut at the tag, and the
// colon in a registry's port is not one. Cutting at the leftmost colon
// turns "registry.example:5000/team/img:1.2.3" into "registry.example",
// which resolves to nothing or to somebody else's bytes.
const sha = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestPinnedStripsTagKeepsRegistryPort(t *testing.T) {
	lock := Lock{Images: map[string]Image{
		"a": {Ref: "registry.example:5000/team/img:1.2.3", Digest: sha},
		"b": {Ref: "img:tag", Digest: sha},
	}}
	if got, err := lock.Pinned("a"); err != nil || got != "registry.example:5000/team/img@"+sha {
		t.Errorf("registry port case = %q, %v", got, err)
	}
	if got, err := lock.Pinned("b"); err != nil || got != "img@"+sha {
		t.Errorf("plain tag case = %q, %v", got, err)
	}
	if _, err := lock.Pinned("missing"); err == nil {
		t.Error("a missing entry must be rejected")
	}
}

// TestPinnedRefusesAnEntryARegistryCouldNotResolve: the lock names what
// runs privileged, so an entry that is merely string-shaped has to be
// refused here rather than at a pull. A digest of the right shape and
// the wrong length is the case a concatenation cannot see.
func TestPinnedRefusesAnEntryARegistryCouldNotResolve(t *testing.T) {
	for name, entry := range map[string]Image{
		"digest too short":  {Ref: "img:tag", Digest: "sha256:abc"},
		"digest missing":    {Ref: "img:tag", Digest: ""},
		"digest unprefixed": {Ref: "img:tag", Digest: "0123456789abcdef"},
		"reference missing": {Ref: "", Digest: sha},
		"reference upper":   {Ref: "IMG:tag", Digest: sha},
		"reference spaced":  {Ref: "img tag", Digest: sha},
		"digest algorithm":  {Ref: "img:tag", Digest: "md5:0123456789abcdef0123456789abcdef"},
	} {
		lock := Lock{Images: map[string]Image{"x": entry}}
		if got, err := lock.Pinned("x"); err == nil {
			t.Errorf("%s: Pinned = %q with no error; the lock would name bytes no registry serves", name, got)
		}
	}
}
