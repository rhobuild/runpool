package githubcontract

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

const (
	labelRoutingWorkflow    = "label-routing.yml"
	labelRoutingFixturePath = ".github/workflows/label-routing.yml"

	// A dispatched job that is going to be offered is offered within
	// seconds; this is long enough that a slow queue is not read as a
	// refusal, and short enough that a refusal is not a five-minute
	// test. Both outcomes are answers, so the bound decides how long an
	// answer takes rather than whether one arrives.
	routingWindow = 90 * time.Second
)

// TestAJobReachesAScaleSetCarryingMoreLabelsThanItAsksFor is the
// experiment that decides whether runpool can ever serve more than one
// label per tier.
//
// A tier is a resource ceiling, a capsule image and an egress policy.
// Today the `runs-on` that selects one is a unique scale-set name, so
// the configuration decides which tier serves a job. Labels would move
// that decision into GitHub's matcher -- and the rule is published for
// runners in general ("any runner that matches all of the specified
// runs-on values") and nowhere for scale sets, whose concepts page still
// says they carry one label at all.
//
// If the rule is the general one, then a tier labelled {a, b, c} answers
// a job asking for {a, b}, two tiers whose labels overlap both answer,
// and which of them serves the job -- with its network policy -- is a
// tie nothing documents breaking. If instead a scale set is matched only
// on the exact set it carries, overlap is impossible to write by
// accident and labels are ordinary configuration.
//
// One scale set, deliberately: with two, a job could land on either and
// a single dispatch would measure a tie-break rather than the rule. Here
// the only set that could possibly answer carries a superset, so an
// assignment means superset matching and silence means exact matching.
func TestAJobReachesAScaleSetCarryingMoreLabelsThanItAsksFor(t *testing.T) {
	url, token := target(t, envOrgURL, envOrgToken)
	repo := requireEnv(t, envRepoA)
	c := newClient(t, url, token)
	rest := newRESTClient(token)
	ctx := testCtx(t)

	verifyLabelRoutingFixture(t, ctx, rest, repo)

	group, err := c.GetRunnerGroupByName(ctx, scaleset.DefaultRunnerGroup)
	if err != nil {
		t.Fatalf("resolve Default runner group: %v", err)
	}

	// Asked for: the first two. Carried: those two and a third, so the
	// set is a strict superset of the request and matches under the
	// general rule and not under an exact one.
	name := uniqueName(t)
	asked := []string{name + "-a", name + "-b"}
	carried := append(append([]string{}, asked...), name+"-c")
	set := createScaleSetWith(t, c, &scaleset.RunnerScaleSet{
		Name:          name,
		RunnerGroupID: group.ID,
		Labels:        labelsOf(carried),
	})

	session, err := c.MessageSessionClient(ctx, set.ID, "runpool-label-routing-contract")
	if err != nil {
		t.Fatalf("open a message session on scale set %d: %v", set.ID, err)
	}
	t.Cleanup(func() {
		closing, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := session.Close(closing); err != nil {
			t.Errorf("cleanup: close session: %v", err)
		}
	})

	run := dispatchLabelRouting(t, ctx, rest, repo, asked)
	t.Cleanup(func() {
		closing, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// The job is never served: no runner is started for it. Leaving
		// it queued would hold the fixture's concurrency group against
		// the next run of this suite.
		if err := rest.cancelRun(closing, repo, run.ID); err != nil {
			t.Errorf("cleanup: cancel run %d: %v", run.ID, err)
		}
	})

	if offered := waitForAnyJob(t, ctx, session, routingWindow); offered {
		t.Errorf("a job asking for %v was offered to a scale set carrying %v. "+
			"A scale set is matched on holding all of the requested labels, not on "+
			"carrying exactly them -- so two tiers whose labels overlap both answer "+
			"one `runs-on`, and which of them serves a job, with its resource "+
			"ceiling and its egress policy, is a tie GitHub does not document "+
			"breaking. Labels cannot select a tier.", asked, carried)
		return
	}
	t.Logf("no job reached a scale set carrying %v for a request of %v within %s: "+
		"a scale set answers the exact label set it carries, and overlap cannot be "+
		"written by accident", carried, asked, routingWindow)
}

// TestAScaleSetGivenLabelsStillAnswersToItsName settles whether
// configuring labels on a tier would break the workflows already
// pointing at it.
//
// The SDK gives a scale set a name-equal label only when the caller
// supplies none, and ARC prepends the name itself before sending custom
// labels -- which suggests the service does not keep it, and that every
// existing `runs-on: <scale-set-name>` would stop resolving the moment a
// tier was given labels. Suggests is not knows.
func TestAScaleSetGivenLabelsStillAnswersToItsName(t *testing.T) {
	url, token := target(t, envOrgURL, envOrgToken)
	repo := requireEnv(t, envRepoA)
	c := newClient(t, url, token)
	rest := newRESTClient(token)
	ctx := testCtx(t)

	verifyLabelRoutingFixture(t, ctx, rest, repo)

	group, err := c.GetRunnerGroupByName(ctx, scaleset.DefaultRunnerGroup)
	if err != nil {
		t.Fatalf("resolve Default runner group: %v", err)
	}
	name := uniqueName(t)
	set := createScaleSetWith(t, c, &scaleset.RunnerScaleSet{
		Name:          name,
		RunnerGroupID: group.ID,
		Labels:        labelsOf([]string{name + "-only"}),
	})

	session, err := c.MessageSessionClient(ctx, set.ID, "runpool-label-routing-contract")
	if err != nil {
		t.Fatalf("open a message session on scale set %d: %v", set.ID, err)
	}
	t.Cleanup(func() {
		closing, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := session.Close(closing); err != nil {
			t.Errorf("cleanup: close session: %v", err)
		}
	})

	// The set's own name, which is not among its labels.
	run := dispatchLabelRouting(t, ctx, rest, repo, []string{name})
	t.Cleanup(func() {
		closing, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := rest.cancelRun(closing, repo, run.ID); err != nil {
			t.Errorf("cleanup: cancel run %d: %v", run.ID, err)
		}
	})

	if waitForAnyJob(t, ctx, session, routingWindow) {
		t.Logf("a scale set given labels still answers to its own name: existing "+
			"`runs-on: %s` workflows would survive a tier being given labels", name)
		return
	}
	t.Errorf("no job reached scale set %q dispatched by its own name within %s. "+
		"Giving a tier labels silently stops every workflow already pointing at it "+
		"by name, so runpool would have to carry the name as a label itself",
		name, routingWindow)
}

// waitForAnyJob reports whether the session was offered any job before
// the window closed. Both answers are the measurement, so a window that
// closes is not a failure here -- unlike messagePoller, which is used
// where an assignment is the contract and its absence is a broken one.
func waitForAnyJob(t *testing.T, ctx context.Context, session *scaleset.MessageSessionClient, within time.Duration) bool {
	t.Helper()
	deadline, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	for {
		msg, err := session.GetMessage(deadline, 0, 2)
		if err != nil {
			if deadline.Err() != nil {
				return false
			}
			t.Fatalf("get message: %v", err)
		}
		if msg == nil {
			if deadline.Err() != nil {
				return false
			}
			continue
		}
		offered := len(msg.JobAvailableMessages) > 0 || len(msg.JobAssignedMessages) > 0
		if err := session.DeleteMessage(deadline, msg.MessageID); err != nil && deadline.Err() == nil {
			t.Fatalf("delete message %d: %v", msg.MessageID, err)
		}
		if offered {
			return true
		}
	}
}

func labelsOf(names []string) []scaleset.Label {
	out := make([]scaleset.Label, 0, len(names))
	for _, n := range names {
		out = append(out, scaleset.Label{Name: n})
	}
	return out
}

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s not set; the label-routing experiment needs a fixture repository", name)
	}
	return v
}

func verifyLabelRoutingFixture(t *testing.T, ctx context.Context, rest *restClient, repo string) {
	t.Helper()
	fixture, err := os.ReadFile(filepath.Join("testdata", labelRoutingWorkflow))
	if err != nil {
		t.Fatalf("read the label-routing fixture: %v", err)
	}
	want := sha256.Sum256(fixture)
	got, err := rest.installedFixtureDigest(ctx, repo, labelRoutingFixturePath)
	if err != nil {
		t.Fatalf("read the label-routing workflow installed on %s: %v", repo, err)
	}
	if got != hex.EncodeToString(want[:]) {
		t.Fatalf("the installed %s on %s differs from testdata/%s; install the versioned fixture",
			labelRoutingFixturePath, repo, labelRoutingWorkflow)
	}
}

func dispatchLabelRouting(t *testing.T, ctx context.Context, rest *restClient, repo string, labels []string) workflowRun {
	t.Helper()
	encoded, err := json.Marshal(labels)
	if err != nil {
		t.Fatal(err)
	}
	correlation := uniqueName(t)
	if err := rest.dispatchWorkflow(ctx, repo, labelRoutingWorkflow, map[string]string{
		"runner_labels":  string(encoded),
		"correlation_id": correlation,
	}); err != nil {
		t.Fatalf("dispatch %s on %s: %v", labelRoutingWorkflow, repo, err)
	}
	run, err := rest.findRunByCorrelation(ctx, repo, labelRoutingWorkflow, correlation, 60*time.Second)
	if err != nil {
		t.Fatalf("find the dispatched run: %v", err)
	}
	return run
}
