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

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	verifyOrganizationProbeFixtures(t, ctx, rest, repo)

	group, err := c.GetRunnerGroupByName(ctx, scaleset.DefaultRunnerGroup)
	if err != nil {
		t.Fatal(err)
	}
	label := uniqueName(t)
	set := createScaleSet(t, c, group, label)

	session := openContractSession(t, ctx, c, set.ID, "runpool-capacity-contract")

	last := 0
	prime := observeBroker(t, ctx, session, &last, 0, "prime")
	if prime.Assigned {
		t.Fatal("the new scale set received an assignment before the probe was dispatched")
	}
	run := dispatchOrganizationProbe(t, ctx, rest, repo, label)
	t.Cleanup(func() { cancelProbeRun(t, rest, repo, run.ID) })

	zero := observeBroker(t, ctx, session, &last, 0, "zero")
	raised := observeBroker(t, ctx, session, &last, 1, "raised")
	t.Logf("capacity observations: zero={assigned:%t statistics:%t} raised={assigned:%t statistics:%t}",
		zero.Assigned, zero.Statistics, raised.Assigned, raised.Statistics)

	// The contract this test pins: announcing zero is a trapdoor, not a
	// throttle. A job that queues under capacity 0 is neither delivered
	// then nor after capacity rises within the session, and a session
	// announcing 0 receives no statistics at all — it is blind to its
	// own demand. The allocator therefore rotates one bounded discovery
	// credit among otherwise silent bindings.
	if zero.Assigned {
		t.Error("job was assigned while capacity was 0; the announcement is not an admission gate")
	}
	if zero.Statistics {
		t.Error("a session announcing capacity 0 received demand statistics; revisit the zero-capacity blindness contract")
	}
	if raised.Assigned {
		t.Error("broker behavior changed: a job queued under capacity 0 was delivered after raising capacity; revisit discovery-credit semantics")
	}
	t.Log("zero-capacity blindness confirmed; eligible bindings require rotating discovery")
}

type brokerPollObservation struct {
	Assigned   bool
	Statistics bool
	Empty      bool
}

func observeBroker(
	t *testing.T,
	ctx context.Context,
	session *scaleset.MessageSessionClient,
	last *int,
	capacity int,
	phase string,
) brokerPollObservation {
	t.Helper()
	observation := brokerPollObservation{}
	for poll := 1; poll <= 3; poll++ {
		current := receiveBrokerPoll(t, ctx, session, last, capacity, phase)
		observation.Assigned = observation.Assigned || current.Assigned
		observation.Statistics = observation.Statistics || current.Statistics
		observation.Empty = current.Empty
		if current.Assigned || current.Empty {
			return observation
		}
	}
	t.Fatalf("%s did not reach an assignment or an empty broker response after three polls", phase)
	return observation
}

// receiveBrokerPoll waits for the broker's own long-poll response. A local
// deadline is a failed measurement, not an empty broker response.
func receiveBrokerPoll(
	t *testing.T,
	ctx context.Context,
	session *scaleset.MessageSessionClient,
	last *int,
	capacity int,
	phase string,
) brokerPollObservation {
	t.Helper()
	pollCtx, cancel := context.WithTimeout(ctx, 75*time.Second)
	defer cancel()
	msg, err := session.GetMessage(pollCtx, *last, capacity)
	if err != nil {
		t.Fatalf("%s poll at capacity %d did not complete at the broker: %v", phase, capacity, err)
	}
	if msg == nil {
		t.Logf("[%s capacity=%d] broker completed an empty poll", phase, capacity)
		return brokerPollObservation{Empty: true}
	}

	observation := brokerPollObservation{Statistics: msg.Statistics != nil}
	if statistics := msg.Statistics; statistics != nil {
		t.Logf("[%s capacity=%d] available=%d assigned=%d acquired=%d", phase, capacity,
			statistics.TotalAvailableJobs, statistics.TotalAssignedJobs, statistics.TotalAcquiredJobs)
	}
	for _, job := range msg.JobAssignedMessages {
		t.Logf("[%s] JobAssigned: %s/%s run=%d", phase, job.OwnerName, job.RepositoryName, job.WorkflowRunID)
		observation.Assigned = true
	}
	if err := session.DeleteMessage(ctx, msg.MessageID); err != nil {
		t.Fatalf("acknowledge message %d during %s: %v", msg.MessageID, phase, err)
	}
	*last = msg.MessageID
	return observation
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
