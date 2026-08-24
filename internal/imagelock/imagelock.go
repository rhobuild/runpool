// Package imagelock is the reviewed image lock: the digest-qualified
// references a release is built from and runs.
//
// It is one package rather than a reader in each place that needs one,
// because the same reference has to be produced identically by the
// controller that runs it, by the gate that re-resolves it against its
// registry, and by the contract suites that start a container from it.
// Separate readers of one document are separate chances to disagree
// about what it says, and the disagreement is invisible until a
// privileged payload runs from bytes nobody reviewed.
package imagelock

import (
	// Registers sha256, without which go-digest rejects every digest in
	// the lock as an unsupported algorithm.
	_ "crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
)

// The reviewed lock travels inside the binary, so a controller cannot be
// pointed at images it was not tested against by editing a file next to
// it. A digest-qualified reference is what actually runs: a tag can
// move, and the dind payload runs privileged.
//
//go:embed images.lock.json
var reviewedJSON []byte

// Lock is the reviewed document.
type Lock struct {
	// Platforms are what a release builds for. They are not what it
	// qualifies: build/platform.lock.json records the platforms the
	// suites were run on, and a release may publish for one nobody has
	// run. Keeping the two lists apart is what lets either sentence be
	// said without implying the other.
	Platforms []string         `json:"platforms"`
	Images    map[string]Image `json:"images"`
}

// Image is one locked entry: the reference it was published under and
// the digest that was reviewed.
type Image struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
}

// Reviewed parses the lock this build carries. A malformed lock is an
// error rather than an empty document: building or running from
// unverified bytes that later run privileged is worse than not doing it.
func Reviewed() (Lock, error) { return Parse(reviewedJSON) }

// Parse reads a lock document. The release gate reads build/images.lock.json
// through this rather than the embedded copy, because what it re-resolves
// against the registry has to be the file a human audited; that the two are
// the same bytes is a separate assertion, and it stays separate.
func Parse(b []byte) (Lock, error) {
	var lock Lock
	if err := json.Unmarshal(b, &lock); err != nil {
		return Lock{}, fmt.Errorf("image lock: %w", err)
	}
	return lock, nil
}

// Pinned turns "repo:tag" plus a digest into "repo@digest": the tag is
// discarded, so nothing resolves through a mutable name.
//
// The reference grammar and the digest format belong to the registry,
// not to this project, so they are read by the libraries that define
// them. That is not only tidiness. Cutting a reference at the leftmost
// colon renames "host:5000/img:tag" to "host", and concatenating a
// digest accepts "sha256:abc" — well formed to a string, meaningless to
// a registry, and only discovered when a privileged payload fails to
// pull.
func (l Lock) Pinned(name string) (string, error) {
	entry, ok := l.Images[name]
	if !ok {
		return "", fmt.Errorf("image lock has no %q entry", name)
	}
	ref, err := reference.Parse(entry.Ref)
	if err != nil {
		return "", fmt.Errorf("image lock entry %q: %w", name, err)
	}
	named, ok := ref.(reference.Named)
	if !ok {
		return "", fmt.Errorf("image lock entry %q names no repository: %q", name, entry.Ref)
	}
	dgst, err := digest.Parse(entry.Digest)
	if err != nil {
		return "", fmt.Errorf("image lock entry %q: %w", name, err)
	}
	pinned, err := reference.WithDigest(reference.TrimNamed(named), dgst)
	if err != nil {
		return "", fmt.Errorf("image lock entry %q: %w", name, err)
	}
	return pinned.String(), nil
}
