package githubcontract

import (
	"slices"
	"testing"

	"github.com/actions/scaleset"
)

// TestScaleSetSystemLabel verifies that an unlabeled scale set receives the
// system label used by runs-on routing.
func TestScaleSetSystemLabel(t *testing.T) {
	url, token := target(t, envOrgURL, envOrgToken)
	c := newClient(t, url, token)

	group, err := c.GetRunnerGroupByName(testCtx(t), scaleset.DefaultRunnerGroup)
	if err != nil {
		t.Fatalf("resolve Default runner group: %v", err)
	}
	name := uniqueName(t)
	created := createScaleSet(t, c, group, name)

	if len(created.Labels) != 1 || created.Labels[0].Type != "system" || created.Labels[0].Name != name {
		t.Errorf("labels = %+v; want exactly one system label named %q", created.Labels, name)
	}
}

// The tests below settle what GitHub's own documentation does not say
// about scale sets carrying more than one label, and they exist because
// the answers decide a design rather than describe one.
//
// Runpool assigns one label per scale set: its name. A tier is a
// resource ceiling, an egress policy and a capsule image, and today the
// `runs-on` that reaches it is a unique name, so which tier serves a job
// is decided by the configuration. Serving several labels instead would
// move that decision into GitHub's matcher, and the matcher's rules are
// published in fragments: the workflow syntax says a job runs on "any
// runner that matches all of the specified runs-on values", and the ARC
// 0.14.0 announcement says a scale set may carry several labels. What
// none of them states is whether a scale set is matched by that rule,
// what happens when two sets both match, or whether a set keeps
// answering to its own name once it has been given labels.
//
// Each test states the behaviour observed, so a failure is upstream
// having changed and is worth a maintainer's attention either way. The
// message says what the new answer would mean here.

// TestCustomLabelsReplaceTheNameLabel: whether a scale set given labels
// still answers to its own name.
//
// The SDK's ensureLabels adds the name-equal label only when the caller
// supplies none, and ARC's controller prepends the name itself before
// sending any custom set -- which it would not bother doing if the
// service kept it. If that reading is right, then configuring labels on
// a tier silently stops every existing `runs-on: <scale-set-name>`
// workflow from reaching it, and runpool would have to prepend the name
// the same way.
func TestCustomLabelsReplaceTheNameLabel(t *testing.T) {
	url, token := target(t, envOrgURL, envOrgToken)
	c := newClient(t, url, token)

	group, err := c.GetRunnerGroupByName(testCtx(t), scaleset.DefaultRunnerGroup)
	if err != nil {
		t.Fatalf("resolve Default runner group: %v", err)
	}
	name := uniqueName(t)
	first, second := name+"-a", name+"-b"
	created := createScaleSetWith(t, c, &scaleset.RunnerScaleSet{
		Name:          name,
		RunnerGroupID: group.ID,
		Labels:        []scaleset.Label{{Name: first}, {Name: second}},
	})

	got := labelNames(created)
	if slices.Contains(got, name) {
		t.Errorf("labels = %v and include the scale set's own name; a tier could carry "+
			"labels without breaking `runs-on: %s`, and runpool would not have to "+
			"prepend the name", got, name)
	}
	if want := []string{first, second}; !slices.Equal(got, want) {
		t.Errorf("labels = %v; want exactly %v — the service returned a set the caller "+
			"did not ask for, so what a scale set answers to is not what was configured", got, want)
	}
}

// TestScaleSetLabelsAreFixedAtCreation: whether a tier's labels could be
// changed, or only replaced along with the scale set.
//
// This is the question that decides whether labels are configuration at
// all. `EnsureScaleSet` adopts a set it created before and refuses to
// adopt a different one; if labels cannot be updated, then changing them
// means deleting the set and creating another, which changes the id the
// store recorded and turns an edit to a configuration file into a
// destructive provider operation.
func TestScaleSetLabelsAreFixedAtCreation(t *testing.T) {
	url, token := target(t, envOrgURL, envOrgToken)
	c := newClient(t, url, token)

	group, err := c.GetRunnerGroupByName(testCtx(t), scaleset.DefaultRunnerGroup)
	if err != nil {
		t.Fatalf("resolve Default runner group: %v", err)
	}
	name := uniqueName(t)
	created := createScaleSetWith(t, c, &scaleset.RunnerScaleSet{
		Name:          name,
		RunnerGroupID: group.ID,
		Labels:        []scaleset.Label{{Name: name + "-a"}},
	})

	// The update is expected to be accepted and to change nothing, which
	// is the shape that makes this worth pinning: a caller that only
	// checked the error would record a label set the service never took.
	updated, err := c.UpdateRunnerScaleSet(testCtx(t), created.ID, &scaleset.RunnerScaleSet{
		Labels: []scaleset.Label{{Name: name + "-a"}, {Name: name + "-b"}},
	})
	if err != nil {
		t.Logf("the service refused the label update outright: %v", err)
	} else if got := labelNames(updated); len(got) != 1 {
		t.Errorf("the update answered with labels %v; if labels can be changed in "+
			"place, a tier can be relabelled without deleting the scale set and "+
			"losing the id this instance recorded", got)
	}

	// The answer to a write is not proof the write landed. This is the
	// same reason EnsureScaleSet re-reads DisableUpdate rather than
	// trusting the 200 that accepted it.
	after, err := c.GetRunnerScaleSetByID(testCtx(t), created.ID)
	if err != nil {
		t.Fatalf("re-read scale set %d: %v", created.ID, err)
	}
	if got := labelNames(after); !slices.Equal(got, []string{name + "-a"}) {
		t.Errorf("labels after the update = %v; want the created set unchanged. "+
			"Labels being mutable would make them ordinary configuration, which "+
			"is not how this was designed around", got)
	}
}

// TestTwoScaleSetsMayShareALabel: whether GitHub refuses the overlap
// that makes a tier ambiguous, or whether runpool would have to.
//
// Scale-set names are unique within a runner group and runpool's
// configuration validation refuses a duplicate. If labels carry no such
// constraint, then two tiers can both answer to one `runs-on`, and which
// of them serves a job -- with its resource ceiling, its capsule image
// and its egress policy -- is a tie GitHub has not documented breaking.
func TestTwoScaleSetsMayShareALabel(t *testing.T) {
	url, token := target(t, envOrgURL, envOrgToken)
	c := newClient(t, url, token)

	group, err := c.GetRunnerGroupByName(testCtx(t), scaleset.DefaultRunnerGroup)
	if err != nil {
		t.Fatalf("resolve Default runner group: %v", err)
	}
	shared := uniqueName(t) + "-shared"
	for _, name := range []string{uniqueName(t), uniqueName(t)} {
		createScaleSetWith(t, c, &scaleset.RunnerScaleSet{
			Name:          name,
			RunnerGroupID: group.ID,
			Labels:        []scaleset.Label{{Name: shared}, {Name: name}},
		})
	}
	t.Logf("two scale sets in %q both carry the label %q; nothing upstream refuses it",
		scaleset.DefaultRunnerGroup, shared)
}

// TestAScaleSetMayClaimTheSelfHostedLabel: whether the label vocabulary
// a workflow already uses is reserved.
//
// `runs-on: [self-hosted, linux, x64]` is what a fleet migrating from
// classic self-hosted runners has written across its workflows, and
// serving those unedited is the strongest argument for labels at all.
// Scale-set runners receive no default labels, so the word is free
// unless the service holds it back.
func TestAScaleSetMayClaimTheSelfHostedLabel(t *testing.T) {
	url, token := target(t, envOrgURL, envOrgToken)
	c := newClient(t, url, token)

	group, err := c.GetRunnerGroupByName(testCtx(t), scaleset.DefaultRunnerGroup)
	if err != nil {
		t.Fatalf("resolve Default runner group: %v", err)
	}
	name := uniqueName(t)
	created := createScaleSetWith(t, c, &scaleset.RunnerScaleSet{
		Name:          name,
		RunnerGroupID: group.ID,
		Labels:        []scaleset.Label{{Name: "self-hosted"}, {Name: name}},
	})

	// Deleted here rather than left to the cleanup, and this is the
	// whole of why: every other set this suite creates carries only
	// random names nothing else could ask for, and this one deliberately
	// carries the word a fleet migrating from classic runners has
	// written across its workflows. It has no session, so a job assigned
	// to it is a job that waits and is never served -- which the
	// provider's own stranded-grant handling calls unrecoverable from
	// this side. The answer is already in `created`; the set does not
	// have to outlive the assertion to give it.
	deleteNow(t, c, created)

	if got := labelNames(created); !slices.Contains(got, "self-hosted") {
		t.Errorf("labels = %v; the service dropped or refused %q, which it is entitled "+
			"to reserve — and runpool would not have to refuse it itself",
			got, "self-hosted")
	}
}

// labelNames is the label set as the service reports it, sorted, so a
// comparison is about which labels exist rather than the order a
// response happened to list them in.
func labelNames(s *scaleset.RunnerScaleSet) []string {
	out := make([]string, 0, len(s.Labels))
	for _, l := range s.Labels {
		out = append(out, l.Name)
	}
	slices.Sort(out)
	return out
}
