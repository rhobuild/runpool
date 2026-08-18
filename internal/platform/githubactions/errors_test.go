package githubactions

import (
	"errors"
	"fmt"
	"testing"

	"github.com/actions/scaleset"
)

// TestDeregistrationRefusalsStayDistinguishable. "Already gone" and "the
// provider says it is busy" are opposite outcomes of the same call: the
// first is cleanup succeeding, the second is a registration that outlives
// the capsule and counts against the scale set until the provider expires
// it. A caller that cannot tell them apart logs one line for both.
//
// The adapter wraps the SDK's error, so this pins that the wrap keeps the
// chain rather than flattening it to a string.
func TestDeregistrationRefusalsStayDistinguishable(t *testing.T) {
	for name, tc := range map[string]struct {
		err       error
		notFound  bool
		stillBusy bool
	}{
		"already removed": {
			err:      fmt.Errorf("remove runner 7: %w", scaleset.RunnerNotFoundError),
			notFound: true,
		},
		"the provider holds it busy": {
			err:       fmt.Errorf("remove runner 7: %w", scaleset.JobStillRunningError),
			stillBusy: true,
		},
		"some other failure": {
			err: fmt.Errorf("remove runner 7: %w", errors.New("503 from the broker")),
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := errors.Is(tc.err, ErrRunnerNotFound); got != tc.notFound {
				t.Errorf("errors.Is(err, ErrRunnerNotFound) = %v; want %v", got, tc.notFound)
			}
			if got := errors.Is(tc.err, ErrJobStillRunning); got != tc.stillBusy {
				t.Errorf("errors.Is(err, ErrJobStillRunning) = %v; want %v", got, tc.stillBusy)
			}
		})
	}
}
