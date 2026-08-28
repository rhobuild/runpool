package command

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rhobuild/runpool/internal/assignment"
	"github.com/rhobuild/runpool/internal/store"
)

// TestAQuarantinedLeaseNamesWhatHoldsIt: the runbook sends an operator
// here to find the object a cleanup could not remove, and a count is not
// an object.
//
// A quarantined lease is stuck on a thing rather than on a decision —
// a volume still mounted, a network with an endpoint attached — and the
// report said only how many such things there were. Nor were they
// anywhere else: `discrepancies` lists objects belonging to no live
// lease, and a quarantined lease is live, so its own never appear there.
//
// Only that state. A busy host's report is mostly leases doing their job,
// and naming every object of every one of them buries the two lines that
// matter.
func TestAQuarantinedLeaseNamesWhatHoldsIt(t *testing.T) {
	stuck := assignment.LeaseID("lease-stuck")
	fine := assignment.LeaseID("lease-fine")
	snap := store.Snapshot{
		Attempts: map[assignment.LeaseID]store.Attempt{
			stuck: {TenantKey: "acme", ProjectKey: "app"},
			fine:  {TenantKey: "acme", ProjectKey: "web"},
		},
		Resources: map[assignment.LeaseID][]store.ResourceIntent{
			stuck: {
				{Kind: "volume", Name: "runpool-cache-9f2a", State: "cleanup_pending"},
				{Kind: "network", Name: "runpool-egress-9f2a", State: "present"},
			},
			fine: {{Kind: "container", Name: "runpool-capsule-11bb", State: "present"}},
		},
	}
	live := []store.Lease{
		{ID: stuck, State: store.LeaseQuarantined},
		{ID: fine, State: store.LeaseWorkloadRunning},
	}

	var out bytes.Buffer
	renderLeases(&out, live, snap)
	report := out.String()

	for _, want := range []string{"runpool-cache-9f2a", "cleanup_pending", "runpool-egress-9f2a"} {
		if !strings.Contains(report, want) {
			t.Errorf("the report does not name %q; an operator cannot clear an object the "+
				"report only counted:\n%s", want, report)
		}
	}
	if strings.Contains(report, "runpool-capsule-11bb") {
		t.Errorf("a lease that is not quarantined named its objects; on a busy host that "+
			"buries the ones that need clearing:\n%s", report)
	}
}
