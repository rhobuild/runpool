package app

import (
	"testing"

	"github.com/rhobuild/runpool/internal/config"
)

// TestTierCapsuleImage: a tier's own image replaces the shipped one for
// that tier alone, and a tier that names none runs what the build ships.
func TestTierCapsuleImage(t *testing.T) {
	const shipped = "ghcr.io/rhobuild/runpool/capsule@sha256:" +
		"1111111111111111111111111111111111111111111111111111111111111111"
	const operators = "ghcr.io/acme/capsule@sha256:" +
		"2222222222222222222222222222222222222222222222222222222222222222"

	if got := tierCapsuleImage(config.Tier{ID: "standard"}, shipped); got != shipped {
		t.Errorf("a tier naming no image runs %q, want the shipped capsule", got)
	}
	if got := tierCapsuleImage(config.Tier{ID: "heavy", CapsuleImage: operators}, shipped); got != operators {
		t.Errorf("a tier naming an image runs %q, want its own", got)
	}
}
