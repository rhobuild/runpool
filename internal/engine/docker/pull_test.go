package docker

import (
	"context"
	"errors"
	"fmt"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
)

// TestAPullIsTriedAgainOnlyWhileTheAnswerMightChange: a pull is the one
// step of a launch that depends on a third party being reachable, and a
// failed one spends the attempt's retry budget. An attempt that spends
// all of it is held for a person, so a registry that was briefly
// unreachable became work for an operator.
//
// Trying again is only right where the answer might differ. A reference
// nobody published answers the same every time, and pausing between
// three identical refusals delays the error the operator needs.
func TestAPullIsTriedAgainOnlyWhileTheAnswerMightChange(t *testing.T) {
	unreachable := errors.New("dial tcp: connection reset by peer")

	for _, c := range []struct {
		name  string
		fail  error
		tries int
	}{
		{"a transport failure arrives untyped and is retried", unreachable, pullAttempts},
		{"an unpublished reference answers the same every time",
			fmt.Errorf("manifest unknown: %w", cerrdefs.ErrNotFound), 1},
		{"a credential that does not carry is not a blip",
			fmt.Errorf("denied: %w", cerrdefs.ErrPermissionDenied), 1},
		{"a malformed reference is refused the same way every time",
			fmt.Errorf("invalid reference: %w", cerrdefs.ErrInvalidArgument), 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			tries := 0
			err := pullWithRetry(t.Context(), "img", 0, func(context.Context, string) error {
				tries++
				return c.fail
			})
			if !errors.Is(err, c.fail) {
				t.Errorf("returned %v; want the failure it was given", err)
			}
			if tries != c.tries {
				t.Errorf("pulled %d times; want %d. A permanent refusal retried delays the "+
					"error, and a blip not retried holds an attempt", tries, c.tries)
			}
		})
	}
}

// TestAPullThatSucceedsOnASecondTryIsNotAFailure holds the case this
// exists for: the launch continues rather than spending budget.
func TestAPullThatSucceedsOnASecondTryIsNotAFailure(t *testing.T) {
	tries := 0
	err := pullWithRetry(t.Context(), "img", 0, func(context.Context, string) error {
		tries++
		if tries == 1 {
			return errors.New("unexpected EOF")
		}
		return nil
	})
	if err != nil {
		t.Errorf("pull failed with %v; the second answer was success", err)
	}
	if tries != 2 {
		t.Errorf("pulled %d times; want 2", tries)
	}
}

// TestACancelledLaunchStopsPulling: the pause must not outlive the job.
func TestACancelledLaunchStopsPulling(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	tries := 0
	err := pullWithRetry(ctx, "img", 0, func(context.Context, string) error {
		tries++
		cancel()
		return errors.New("unexpected EOF")
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("returned %v; want the cancellation", err)
	}
	if tries != 1 {
		t.Errorf("pulled %d times after cancellation; want 1", tries)
	}
}
