package app

import (
	"strings"
	"testing"
)

// TestBuildInputImagesArePinned: what feeds the capsule image build
// must resolve through an immutable digest, never a tag that can move
// under a privileged payload.
func TestBuildInputImagesArePinned(t *testing.T) {
	runner, dind, err := buildInputImages()
	if err != nil {
		t.Fatal(err)
	}
	for name, ref := range map[string]string{"runner": runner, "dind": dind} {
		if !strings.Contains(ref, "@sha256:") {
			t.Errorf("%s image %q is not digest-qualified", name, ref)
		}
		if strings.Contains(strings.TrimPrefix(ref, "ghcr.io/"), ":") &&
			!strings.Contains(ref, "@") {
			t.Errorf("%s image %q still carries a tag", name, ref)
		}
	}
	if !strings.HasPrefix(runner, "ghcr.io/actions/actions-runner@") {
		t.Errorf("runner reference lost its repository: %q", runner)
	}
	if !strings.HasPrefix(dind, "docker@") {
		t.Errorf("dind reference lost its repository: %q", dind)
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
	img, err := CapsuleImage(fromEnv, "runpool-capsule:dev")
	if err != nil {
		t.Fatal(err)
	}
	if img != "runpool-capsule:local-test" {
		t.Errorf("with the override set, image = %q; want the override", img)
	}
	img, err = CapsuleImage(func(string) string { return "" }, "runpool-capsule:dev")
	if err != nil {
		t.Fatal(err)
	}
	if img != "runpool-capsule:dev" {
		t.Errorf("without override or lock entry, image = %q; want the dev tag", img)
	}

	release := "ghcr.io/rhobuild/runpool/capsule@sha256:" +
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	img, err = CapsuleImage(func(string) string { return release }, release)
	if err != nil || img != release {
		t.Fatalf("release image = %q, %v; want stamped digest", img, err)
	}
	if _, err := CapsuleImage(fromEnv, release); err == nil {
		t.Fatal("a runtime override replaced the release capsule image")
	}
	if _, err := CapsuleImage(func(string) string { return "" }, "ghcr.io/rhobuild/runpool/capsule:v1"); err == nil {
		t.Fatal("a mutable release capsule reference was accepted")
	}
}
