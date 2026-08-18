package githubcontract

import (
	"context"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/rhobuild/runpool/internal/platform/githubactions"
)

// TestASecondSessionIsRefusedRecognisably pins the one error the restart
// path has to recognise.
//
// A controller that was killed leaves its session with the broker, which
// holds it until it expires by inactivity. The successor already owns
// the local state lock, so the only correct response is to wait the old
// session out — and it can only choose that response if it can tell this
// refusal from every other failure. The predicate matches the status the
// upstream client renders and the sentence GitHub writes beside it;
// this is what keeps saying that at least one of them still arrives.
//
// Recognising the refusal is the whole of it. Whether the wait succeeds
// is the deployment's business, not the protocol's.
func TestASecondSessionIsRefusedRecognisably(t *testing.T) {
	url, token := target(t, envRepoURL, envRepoToken)
	c := newClient(t, url, token)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	group, err := c.GetRunnerGroupByName(ctx, scaleset.DefaultRunnerGroup)
	if err != nil {
		t.Fatal(err)
	}
	set := createScaleSet(t, c, group, uniqueName(t))

	held, err := c.MessageSessionClient(ctx, set.ID, "runpool-conflict-contract")
	if err != nil {
		t.Fatalf("open the first message session: %v", err)
	}
	defer func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer ccancel()
		if err := held.Close(cctx); err != nil {
			t.Errorf("cleanup: close the held session: %v", err)
		}
	}()

	second, err := c.MessageSessionClient(ctx, set.ID, "runpool-conflict-contract-successor")
	if err == nil {
		cctx, ccancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer ccancel()
		second.Close(cctx)
		t.Fatal("the broker opened a second session for one scale set; the restart path " +
			"assumes it refuses, and a live duplicate would mean two controllers polling one queue")
	}

	t.Logf("second session refused: %v", err)
	if !githubactions.IsSessionConflict(err) {
		t.Fatalf("the refusal is not recognised as a session conflict: %v\n"+
			"a restart after a crash would fail on the first try instead of waiting the "+
			"dead session out, leaving the capsules of the previous run stranded", err)
	}
}
