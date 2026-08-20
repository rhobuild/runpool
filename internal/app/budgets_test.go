package app

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rhobuild/runpool/internal/capsule"
	"github.com/rhobuild/runpool/internal/store"

	"github.com/rhobuild/runpool/internal/config"
)

// TestPreparationIsBoundedSeparatelyFromTheJob: a tier's ceiling is the
// wait for a job the provider owns. Bounding preparation with it too made
// the validator's own floor unreachable — the capsule's readiness budget
// alone outlasts the lowest ceiling a tier may configure — and any expiry
// surfaced as whichever preparation step it happened to land in.
func TestPreparationIsBoundedSeparatelyFromTheJob(t *testing.T) {
	floor := time.Duration(config.MinJobTimeout)
	if capsulePrepTimeout <= floor {
		t.Errorf("preparation is bounded at %s and a tier may configure a ceiling of %s; "+
			"the two must not be the same budget", capsulePrepTimeout, floor)
	}
	// The readiness wait is one step inside preparation, so a budget that
	// does not clear it cannot be the budget for the whole of it.
	if capsulePrepTimeout <= 90*time.Second {
		t.Errorf("preparation is bounded at %s, which does not clear the capsule's own readiness budget",
			capsulePrepTimeout)
	}
}

// TestRemainingCeilingIsNotRestartedByAdoption: the ceiling bounds a
// capsule that stopped reporting. Handing an adopted capsule a fresh full
// budget on every restart is how that bound stops bounding anything.
func TestRemainingCeilingIsNotRestartedByAdoption(t *testing.T) {
	ceiling := config.Duration(4 * time.Hour)
	tier := config.Tier{ID: "standard", JobTimeout: &ceiling}

	if got := remainingCeiling(tier, time.Time{}); got != 4*time.Hour {
		t.Errorf("a lease with no recorded start waits %s, want the whole ceiling", got)
	}
	if got := remainingCeiling(tier, time.Now().Add(-time.Hour)); got > 3*time.Hour {
		t.Errorf("a lease an hour old waits %s, want at most the three hours it has left", got)
	}
	// Past the ceiling, a short grace rather than zero: the wait has to
	// resolve through the ordinary path, not expire before it begins.
	got := remainingCeiling(tier, time.Now().Add(-5*time.Hour))
	if got <= 0 {
		t.Errorf("a lease past its ceiling waits %s; it would expire before observing anything", got)
	}
	if got > time.Minute {
		t.Errorf("a lease past its ceiling waits %s, want only a grace", got)
	}
}

// TestTheTierImageReachesTheCapsuleOnly: tiers[].capsuleImage lets a
// deployment add tools to the container its jobs run in. The per-lease
// egress gateway is not that container — it is the one that applies the
// policy confining them — and it once read the same field, so extending a
// tier's jobs silently replaced the enforcement with the operator's own
// build.
//
// The gateway's image is no longer something a launch can name: it
// belongs to the launcher, which the controller builds from the image
// this build ships. What is left to assert here is the other half — that
// the tier's image reaches the capsule and stops there.
func TestTheTierImageReachesTheCapsuleOnly(t *testing.T) {
	const operators = "ghcr.io/acme/capsule@sha256:" +
		"2222222222222222222222222222222222222222222222222222222222222222"

	h := newHarness(t, 1)
	h.bind.capsuleImage = operators
	h.bind.tier = config.Tier{ID: "standard", Parallelism: 1, CapsuleImage: operators}

	caps := &fakeCapsule{}
	runFaulted(t, h, caps, &fakeRegistry{}, &fakeWaiter{}, "job-1")

	spec := caps.spec
	if spec.CapsuleImage != operators {
		t.Errorf("the job runs %q, want the tier's image", spec.CapsuleImage)
	}

}

// TestAnIncompatibleCapsuleIsHeldNotRetried: a capsule that does not
// speak this controller's protocol will not start speaking it on the next
// attempt. Retrying it spends one of the attempt's three servings
// discovering the same fact, three times per job, and then holds it for a
// reason that names the budget rather than the image.
func TestAnIncompatibleCapsuleIsHeldNotRetried(t *testing.T) {
	h := newHarness(t, 1)
	caps := &fakeCapsule{prepareErr: fmt.Errorf("%w: it speaks control protocol \"0\"",
		capsule.ErrIncompatibleImage)}
	lease, _ := runFaulted(t, h, caps, &fakeRegistry{}, &fakeWaiter{}, "job-1")

	attempt := h.attemptByLease(lease.ID)
	if attempt.State != store.AttemptManualReview {
		t.Fatalf("attempt state = %q, want it held rather than requeued", attempt.State)
	}
	if attempt.ReviewReason != store.ReviewReasonIncompatibleCapsule {
		t.Errorf("held as %q, want the image named", attempt.ReviewReason)
	}
}

// And an ordinary preparation failure is still a retry: the budget is
// there for transients, and holding every one of them would turn a failed
// image pull into a person's problem.
func TestAnOrdinaryPreparationFailureStillRetries(t *testing.T) {
	h := newHarness(t, 1)
	caps := &fakeCapsule{prepareErr: errors.New("image pull failed")}
	lease, _ := runFaulted(t, h, caps, &fakeRegistry{}, &fakeWaiter{}, "job-1")

	attempt := h.attemptByLease(lease.ID)
	if attempt.State == store.AttemptManualReview {
		t.Errorf("a transient preparation failure was held as %q", attempt.ReviewReason)
	}
}
