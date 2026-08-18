package githubcontract

import (
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
