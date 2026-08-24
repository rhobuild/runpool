package docker

import "testing"

// TestIsolatedGatewayOptionsCoverBothFamilies is the only thing that
// holds the IPv6 half in place. A daemon drops an option key it does not
// recognise without complaint, and a v4-only host assigns no v6 gateway
// whether the key was sent or not, so the preflight's observation of a
// real daemon cannot tell a build that asks for v6 isolation from one
// that stopped asking.
func TestIsolatedGatewayOptionsCoverBothFamilies(t *testing.T) {
	opts := isolatedGatewayOptions()
	for _, family := range []string{"ipv4", "ipv6"} {
		key := "com.docker.network.bridge.gateway_mode_" + family
		if opts[key] != "isolated" {
			t.Errorf("%s = %q; want %q — the bridge would carry a host address on that family, "+
				"and a capsule would have a route through it", key, opts[key], "isolated")
		}
	}
	if len(opts) != 2 {
		t.Errorf("options = %v; want exactly the two gateway modes", opts)
	}
}
