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

func TestPinnedStripsTagKeepsRegistryPort(t *testing.T) {
	lock := imageLock{Images: map[string]struct {
		Ref    string `json:"ref"`
		Digest string `json:"digest"`
	}{
		"a": {Ref: "registry.example:5000/team/img:1.2.3", Digest: "sha256:abc"},
		"b": {Ref: "img:tag", Digest: "sha256:def"},
		"c": {Ref: "img", Digest: ""},
	}}
	got, err := pinned(lock, "a")
	if err != nil || got != "registry.example:5000/team/img@sha256:abc" {
		t.Errorf("registry port case = %q, %v", got, err)
	}
	if got, err := pinned(lock, "b"); err != nil || got != "img@sha256:def" {
		t.Errorf("plain tag case = %q, %v", got, err)
	}
	if _, err := pinned(lock, "c"); err == nil {
		t.Error("an entry without a digest must be rejected, not run unpinned")
	}
	if _, err := pinned(lock, "missing"); err == nil {
		t.Error("a missing entry must be rejected")
	}
}
