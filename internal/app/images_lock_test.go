package app

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
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
	if !bytes.Equal(reviewed, imageLockJSON) {
		t.Error("internal/app/images.lock.json differs from build/images.lock.json; " +
			"the embedded copy is what runs, so the reviewed copy must be identical")
	}
}

// The lock records the privileged payload a capsule runs, but the capsule is
// built from the FROM lines in its Dockerfile. Nothing else compares the two:
// the verify script re-resolves the lock against its registry and never opens
// a Dockerfile, so a dependency update that moves one and not the other keeps
// every check green while the reviewed document stops describing what runs.
func TestLockedImagesAreWhatTheCapsuleIsBuiltFrom(t *testing.T) {
	dockerfile, err := os.ReadFile("../../build/capsule/Dockerfile")
	if err != nil {
		t.Fatalf("read the capsule Dockerfile: %v", err)
	}
	var lock imageLock
	if err := json.Unmarshal(imageLockJSON, &lock); err != nil {
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

// Development may select a local capsule image. A release carries the
// qualified digest and refuses a runtime replacement.
func TestCapsuleImageResolution(t *testing.T) {
	fromEnv := func(k string) string {
		if k == "RUNPOOL_CAPSULE_IMAGE" {
			return "runpool-capsule:local-test"
		}
		return ""
	}
	img, err := capsuleImage(fromEnv, "runpool-capsule:dev")
	if err != nil {
		t.Fatal(err)
	}
	if img != "runpool-capsule:local-test" {
		t.Errorf("with the override set, image = %q; want the override", img)
	}
	img, err = capsuleImage(func(string) string { return "" }, "runpool-capsule:dev")
	if err != nil {
		t.Fatal(err)
	}
	if img != "runpool-capsule:dev" {
		t.Errorf("without override or lock entry, image = %q; want the dev tag", img)
	}

	release := "ghcr.io/rhobuild/runpool/capsule@sha256:" +
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	img, err = capsuleImage(func(string) string { return release }, release)
	if err != nil || img != release {
		t.Fatalf("release image = %q, %v; want stamped digest", img, err)
	}
	if _, err := capsuleImage(fromEnv, release); err == nil {
		t.Fatal("a runtime override replaced the release capsule image")
	}
	if _, err := capsuleImage(func(string) string { return "" }, "ghcr.io/rhobuild/runpool/capsule:v1"); err == nil {
		t.Fatal("a mutable release capsule reference was accepted")
	}
}
