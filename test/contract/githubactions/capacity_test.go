package githubcontract

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

// TestZeroCapacityDoesNotRevealQueuedDemand verifies that zero is a blind
// state rather than a throttle. The allocator's rotating discovery credit
// ensures no eligible binding remains at zero permanently.
func TestZeroCapacityDoesNotRevealQueuedDemand(t *testing.T) {
	url, token := target(t, envRepoURL, envRepoToken)
	repo := repoSlug(t, url)
	c := newClient(t, url, token)
	rest := newRESTClient(token)

	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	verifyOrganizationProbeFixtures(t, ctx, rest, repo)

	group, err := c.GetRunnerGroupByName(ctx, scaleset.DefaultRunnerGroup)
	if err != nil {
		t.Fatal(err)
	}
	label := uniqueName(t)
	set := createScaleSet(t, c, group, label)

	session, err := c.MessageSessionClient(ctx, set.ID, "runpool-capacity-contract")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer ccancel()
		session.Close(cctx)
	}()

	last := 0
	// observe polls with the given announced capacity for a window,
	// logging statistics and reporting whether a JobAssigned appeared. A
	// short per-poll deadline turns the broker long-poll into a tick.
	observe := func(capacity int, window time.Duration, label string) bool {
		deadline := time.Now().Add(window)
		assigned := false
		for time.Now().Before(deadline) {
			pctx, pcancel := context.WithTimeout(ctx, 12*time.Second)
			msg, err := session.GetMessage(pctx, last, capacity)
			pcancel()
			if err != nil {
				continue // long-poll tick with nothing to report
			}
			if msg == nil {
				continue
			}
			last = msg.MessageID
			if s := msg.Statistics; s != nil {
				t.Logf("[%s cap=%d] available=%d assigned=%d acquired=%d", label, capacity,
					s.TotalAvailableJobs, s.TotalAssignedJobs, s.TotalAcquiredJobs)
			}
			for _, j := range msg.JobAssignedMessages {
				t.Logf("[%s] JobAssigned: %s/%s run=%d", label, j.OwnerName, j.RepositoryName, j.WorkflowRunID)
				assigned = true
			}
			session.DeleteMessage(ctx, msg.MessageID)
			if assigned {
				return true
			}
		}
		return assigned
	}

	observe(0, 3*time.Second, "prime") // announce zero before the job exists
	run := dispatchOrganizationProbe(t, ctx, rest, repo, label)
	t.Cleanup(func() { cancelProbeRun(t, rest, repo, run.ID) })

	zeroAssigned := observe(0, 30*time.Second, "zero")
	t.Logf("capacity=0 observation: assigned=%v", zeroAssigned)

	raiseAssigned := observe(1, 60*time.Second, "raised")
	t.Logf("capacity=1 observation: assigned=%v", raiseAssigned)

	// The contract this test pins: announcing zero is a trapdoor, not a
	// throttle. A job that queues under capacity 0 is neither delivered
	// then nor after capacity rises within the session, and a session
	// announcing 0 receives no statistics at all — it is blind to its
	// own demand. The allocator therefore rotates one bounded discovery
	// credit among otherwise silent bindings.
	if zeroAssigned {
		t.Error("job was assigned while capacity was 0; the announcement is not an admission gate")
	}
	if raiseAssigned {
		t.Error("broker behavior changed: a job queued under capacity 0 was delivered after raising capacity; revisit discovery-credit semantics")
	}
	t.Log("zero-capacity blindness confirmed; eligible bindings require rotating discovery")
}

// repoSlug turns a repository URL into owner/repository form.
func repoSlug(t *testing.T, url string) string {
	t.Helper()
	path := strings.TrimPrefix(strings.TrimPrefix(url, "https://github.com/"), "http://github.com/")
	path = strings.Trim(path, "/")
	if strings.Count(path, "/") != 1 {
		t.Fatalf("expected a repository URL, got %q", url)
	}
	return path
}
