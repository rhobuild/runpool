//go:build github_observation

package githubcontract

import (
	"slices"
	"testing"

	"github.com/actions/scaleset"
)

// deleteNow removes a scale set the moment an observation is complete.
// The removal is recorded so shared cleanup can distinguish it from a
// scale set that disappeared unexpectedly.
func deleteNow(t *testing.T, c *scaleset.Client, set *scaleset.RunnerScaleSet) {
	t.Helper()
	earlyMu.Lock()
	early[set.ID] = true
	earlyMu.Unlock()
	if err := c.DeleteRunnerScaleSet(testCtx(t), set.ID); err != nil {
		t.Errorf("delete scale set %d: %v", set.ID, err)
	}
}

// TestObservationCustomLabelsAreReturnedWithoutName records the labels
// returned when the caller supplies the complete custom set.
func TestObservationCustomLabelsAreReturnedWithoutName(t *testing.T) {
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

	if got, want := labelNames(created), []string{first, second}; !slices.Equal(got, want) {
		t.Errorf("labels = %v; want exactly %v", got, want)
	}
}

// TestObservationScaleSetLabelsAreFixedAtCreation records whether an update
// changes the labels persisted by the service.
func TestObservationScaleSetLabelsAreFixedAtCreation(t *testing.T) {
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

	updated, err := c.UpdateRunnerScaleSet(testCtx(t), created.ID, &scaleset.RunnerScaleSet{
		Labels: []scaleset.Label{{Name: name + "-a"}, {Name: name + "-b"}},
	})
	if err != nil {
		t.Logf("the service refused the label update: %v", err)
	} else if got := labelNames(updated); len(got) != 1 {
		t.Errorf("labels returned by update = %v; want the original label only", got)
	}

	after, err := c.GetRunnerScaleSetByID(testCtx(t), created.ID)
	if err != nil {
		t.Fatalf("re-read scale set %d: %v", created.ID, err)
	}
	if got, want := labelNames(after), []string{name + "-a"}; !slices.Equal(got, want) {
		t.Errorf("persisted labels = %v; want %v", got, want)
	}
}

// TestObservationScaleSetsMayShareALabel records whether the service accepts
// overlapping custom labels within one runner group.
func TestObservationScaleSetsMayShareALabel(t *testing.T) {
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
	t.Logf("two scale sets in %q accepted label %q", scaleset.DefaultRunnerGroup, shared)
}

// TestObservationScaleSetMayClaimSelfHosted records whether the service
// accepts a label used by classic self-hosted runners.
func TestObservationScaleSetMayClaimSelfHosted(t *testing.T) {
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
	deleteNow(t, c, created)

	if got := labelNames(created); !slices.Contains(got, "self-hosted") {
		t.Errorf("labels = %v; want %q", got, "self-hosted")
	}
}

func labelNames(s *scaleset.RunnerScaleSet) []string {
	out := make([]string, 0, len(s.Labels))
	for _, label := range s.Labels {
		out = append(out, label.Name)
	}
	slices.Sort(out)
	return out
}
