package docker

import "testing"

// TestInstanceInfrastructureIsDecidedByTheLeaseID pins the rule the two
// sweeps used to spell as a list of roles. The list only looked
// equivalent: add a fourth persistent role and every sweep written that
// way deletes it as garbage. The lease id is the real test, because a
// capsule's objects carry theirs in the same label map as the rest of
// their identity and the instance's own are created without one.
//
// The status report deliberately still reads the role. It is not
// deciding what to remove but what to explain, and an object with
// neither a lease nor a role it knows is the discrepancy it exists to
// name.
func TestInstanceInfrastructureIsDecidedByTheLeaseID(t *testing.T) {
	for name, tc := range map[string]struct {
		res  OwnedResource
		want bool
	}{
		"uplink network":        {OwnedResource{ID: "n1", Role: "uplink"}, true},
		"cache lane":            {OwnedResource{ID: "v1", Role: "cache-lane"}, true},
		"a role added later":    {OwnedResource{ID: "v2", Role: "some-future-role"}, true},
		"no role at all":        {OwnedResource{ID: "v3"}, true},
		"a lease's network":     {OwnedResource{ID: "n2", LeaseID: "lse-1", Role: "capsule-net"}, false},
		"a lease's dind volume": {OwnedResource{ID: "v4", LeaseID: "lse-1", Role: "dind-data"}, false},
	} {
		if got := tc.res.InstanceInfrastructure(); got != tc.want {
			t.Errorf("%s: InstanceInfrastructure() = %v; want %v", name, got, tc.want)
		}
	}
}
