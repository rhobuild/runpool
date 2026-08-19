package app

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/rhobuild/runpool/internal/config"
)

// The reviewed image lock travels inside the binary, so a controller
// cannot be pointed at images it was not tested against by editing a file
// next to it. A digest-qualified reference is what actually runs: a tag
// can move, and the dind payload runs privileged.
//
//go:embed images.lock.json
var imageLockJSON []byte

type imageLock struct {
	Images map[string]struct {
		Ref    string `json:"ref"`
		Digest string `json:"digest"`
	} `json:"images"`
}

// defaultCapsuleImage is what a development build runs when nothing
// overrides it, and the reason an unstamped build cannot fail to
// resolve: it is substituted before any check, and it passes all of
// them.
const defaultCapsuleImage = "runpool-capsule:dev"

// CapsuleImage resolves the outer capsule image the controller creates.
// Exported because two surfaces must give the same answer: serve, which
// launches it, and status, which reports it — a report resolved by any
// other rule describes an image the controller does not run.
// A release binary carries the exact digest that passed release qualification;
// an environment value cannot replace it. Development builds may use an
// explicit local image through RUNPOOL_CAPSULE_IMAGE.
//
// The runner and dind lock entries remain the build inputs of that
// image — build/capsule/Dockerfile copies its halves from exactly those
// digests — so the lock still reviews everything privileged that runs.
func CapsuleImage(environ func(string) string, buildDefault string) (string, error) {
	if buildDefault == "" {
		buildDefault = defaultCapsuleImage
	}
	override := environ("RUNPOOL_CAPSULE_IMAGE")
	if config.IsDigestQualifiedImage(buildDefault) {
		if override != "" && override != buildDefault {
			return "", fmt.Errorf("RUNPOOL_CAPSULE_IMAGE cannot override the release capsule %q", buildDefault)
		}
		return buildDefault, nil
	}
	if buildDefault != defaultCapsuleImage {
		return "", fmt.Errorf("release capsule image %q is not digest-qualified", buildDefault)
	}
	if override != "" {
		return override, nil
	}
	return buildDefault, nil
}

// buildInputImages returns the digest-qualified runner and dind
// references the capsule image is built from. A malformed or incomplete
// lock is fatal: building from unverified bytes that later run
// privileged is worse than not building.
func buildInputImages() (runner, dind string, err error) {
	var lock imageLock
	if err := json.Unmarshal(imageLockJSON, &lock); err != nil {
		return "", "", fmt.Errorf("image lock: %w", err)
	}
	runner, err = pinned(lock, "runner")
	if err != nil {
		return "", "", err
	}
	dind, err = pinned(lock, "dind")
	return runner, dind, err
}

// pinned turns "repo:tag" plus a digest into "repo@digest": the tag is
// discarded, so nothing resolves through a mutable name.
func pinned(lock imageLock, name string) (string, error) {
	entry, ok := lock.Images[name]
	if !ok {
		return "", fmt.Errorf("image lock has no %q entry", name)
	}
	if entry.Digest == "" || entry.Ref == "" {
		return "", fmt.Errorf("image lock entry %q is incomplete", name)
	}
	repo := entry.Ref
	// Strip the tag, keeping any registry port (host:5000/image:tag).
	for i := len(repo) - 1; i >= 0; i-- {
		if repo[i] == ':' {
			repo = repo[:i]
			break
		}
		if repo[i] == '/' {
			break
		}
	}
	return repo + "@" + entry.Digest, nil
}
