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

	// The budget for everything before the measurement: reading the
	// installed fixture, resolving the runner group, creating the scale
	// set, opening a session, dispatching, and polling for the run the
	// dispatch created -- which is allowed a minute on its own. testCtx's
	// ninety seconds would leave that minute sharing the rest with four
	// network calls and no slack, and while a shortfall there is now a
	// loud failure rather than a wrong answer, it is still a red run that
	// measured nothing. The identity suite budgets the same one-minute
	// ceiling inside ten minutes, for the same reason.
	routingSetup = 5 * time.Minute
)

// routingCtx is the budget for setting an experiment up. It is separate
// from the measurement window, which takes its own clock, so nothing
// spent here can shorten the observation that decides the answer.
func routingCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), routingSetup)
	t.Cleanup(cancel)
	return ctx
}

// TestAJobReachesAScaleSetCarryingMoreLabelsThanItAsksFor is the
// experiment that decides whether runpool can ever serve more than one
// label per tier.
//
// A tier is a resource ceiling, a capsule image and an egress policy.
// Today the `runs-on` that selects one is a unique scale-set name, so
// the configuration decides which tier serves a job. Labels would move
// that decision into GitHub's matcher -- and the rule is published for
// runners in general ("any runner that matches all of the specified
// runs-on values") and nowhere for scale sets.
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
//
// Measured on 2026-08-29: the job arrived. So a scale set is matched on
// holding all of the labels a job asks for, and two tiers carrying
// overlapping labels both answer one `runs-on`.
//
// What that does and does not settle: an uncoordinated label cannot
// select a tier safely, because which of two matching tiers serves a
// job -- with its resource ceiling and its egress policy -- is a tie
// nothing documents breaking. It does not show a label could never
// select one. Labels proven disjoint across a target's tiers would make
// two matches impossible, since a request that matched both would have
// to be a subset of an empty intersection and `runs-on` is never empty.
// Nothing validates that today, which is why this is deferred rather
// than refused.
func TestAJobReachesAScaleSetCarryingMoreLabelsThanItAsksFor(t *testing.T) {
	url, token := target(t, envOrgURL, envOrgToken)
	repo := requireEnv(t, envRepoA)
	c := newClient(t, url, token)
	rest := newRESTClient(token)
	ctx := routingCtx(t)

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

	if !waitForAnyJob(t, session, routingWindow) {
		t.Errorf("no job asking for %v reached a scale set carrying %v within %s. "+
			"Matching has become exact rather than all-of, which would mean two tiers "+
			"can no longer answer one `runs-on` by carrying overlapping labels -- read "+
			"the decision record before acting on it, because it is the premise that "+
			"deferred serving a second label per tier", asked, carried, routingWindow)
	}
}

// TestAScaleSetKeepsAnsweringToItsNameBesideOtherLabels asks the one
// thing left open about serving more than one label per tier.
//
// The SDK adds the name-equal label only when the caller supplies none,
// so a caller that supplies labels builds the whole set itself --
// including the name, if it wants `runs-on: <name>` to keep working.
// That is what ARC's controller does: it puts the scale set's name in
// first, unconditionally, before appending anything an operator asked
// for. Whether the service then still routes a bare name against a set
// carrying the name and more is the question, and nothing published
// answers it.
//
// This replaces a test that claimed to answer it and did not. That one
// created a set carrying one label that was not its name, dispatched by
// the name, and concluded from the silence that labels break name
// routing. The label it dispatched by was never among the labels the set
// carried, so nothing could have arrived under any matching rule -- the
// experiment never varied the thing it named.
func TestAScaleSetKeepsAnsweringToItsNameBesideOtherLabels(t *testing.T) {
	url, token := target(t, envOrgURL, envOrgToken)
	repo := requireEnv(t, envRepoA)
	c := newClient(t, url, token)
	rest := newRESTClient(token)
	ctx := routingCtx(t)

	verifyLabelRoutingFixture(t, ctx, rest, repo)

	group, err := c.GetRunnerGroupByName(ctx, scaleset.DefaultRunnerGroup)
	if err != nil {
		t.Fatalf("resolve Default runner group: %v", err)
	}
	name := uniqueName(t)
	// The name first and something else after it, which is the shape a
	// caller that wants both has to build.
	set := createScaleSetWith(t, c, &scaleset.RunnerScaleSet{
		Name:          name,
		RunnerGroupID: group.ID,
		Labels:        labelsOf([]string{name, name + "-extra"}),
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

	// By the name alone, which is what every workflow already written
	// against this scale set says.
	run := dispatchLabelRouting(t, ctx, rest, repo, []string{name})
	t.Cleanup(func() {
		closing, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := rest.cancelRun(closing, repo, run.ID); err != nil {
			t.Errorf("cleanup: cancel run %d: %v", run.ID, err)
		}
	})

	if !waitForAnyJob(t, session, routingWindow) {
		t.Errorf("no job reached scale set %q dispatched by its own name within %s, "+
			"though the name is among its labels. Keeping the name is then not enough "+
			"to preserve `runs-on: %s`, and serving a second label would break every "+
			"workflow already pointing at a tier", name, routingWindow, name)
	}
}

// waitForAnyJob reports whether the session was offered any job before
// the window closed. Both answers are the measurement, so a window that
// closes is not a failure here -- unlike messagePoller, which is used
// where an assignment is the contract and its absence is a broken one.
func waitForAnyJob(t *testing.T, session *scaleset.MessageSessionClient, within time.Duration) bool {
	t.Helper()
	// Its own clock, deliberately not the caller's. Setting up costs
	// real time -- finding the dispatched run is allowed a minute of
	// polling on its own -- and a window carved out of an already-ticking
	// budget shrinks by however long that took. Here silence is the
	// answer that means "exact matching", so a window that quietly
	// closed early would not fail: it would report a measurement, and
	// the wrong one.
	deadline, cancel := context.WithTimeout(t.Context(), within)
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
		// The same refusal target() makes, and for the same reason: this
		// suite's contract is that it cannot be skipped while
		// qualifying. A gate that only skips would let a release attest
		// a routing rule nothing measured -- which is worse here than
		// elsewhere, because the answer these tests produce is what
		// decides whether a tier can be selected by label at all.
		if os.Getenv(envQualify) != "" {
			t.Fatalf("release qualification requires %s; the label-routing experiment cannot be skipped", name)
		}
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
