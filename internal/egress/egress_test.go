package egress

import (
	"net/netip"
	"strings"
	"testing"
)

func policy() Policy {
	return Policy{
		InternalSubnet: "172.20.0.0/24",
		UplinkSubnet:   "172.21.0.0/24",
		Allow:          []string{"10.9.8.0/24"},
		Deny:           BuildDeny("172.21.0.0/24", []string{"203.0.113.0/24"}, []string{"172.17.0.0/16"}),
	}
}

func TestValidate(t *testing.T) {
	if err := policy().Validate(); err != nil {
		t.Fatal(err)
	}
	bad := policy()
	bad.Deny = append(bad.Deny, "not-a-cidr")
	if err := bad.Validate(); err == nil {
		t.Fatal("malformed deny cidr validated; half a policy is an open gateway")
	}
	if err := (Policy{}).Validate(); err == nil {
		t.Fatal("empty policy validated")
	}
	noDeny := policy()
	noDeny.Deny = nil
	if err := noDeny.Validate(); err == nil {
		t.Fatal("a policy with an empty deny set validated")
	}
}

// TestBuildDeny: the deny set always carries the baseline — private
// ranges, loopback, link-local metadata, CGNAT, multicast — plus the
// uplink, host and Docker subnets, without duplicates.
func TestBuildDeny(t *testing.T) {
	deny := BuildDeny("172.21.0.0/24", []string{"203.0.113.0/24", "10.0.0.0/8"}, []string{"172.17.0.0/16"})

	for _, must := range []string{
		"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", // RFC1918
		"169.254.0.0/16", // link-local / cloud metadata
		"127.0.0.0/8", "100.64.0.0/10", "224.0.0.0/4",
		"172.21.0.0/24", // the uplink itself
		"203.0.113.0/24",
		"172.17.0.0/16",
	} {
		if !contains(deny, must) {
			t.Errorf("deny set lacks %s", must)
		}
	}
	seen := map[string]bool{}
	for _, c := range deny {
		if seen[c] {
			t.Errorf("duplicate deny entry %s", c)
		}
		seen[c] = true
	}
}

// TestDecide is the check every relayed connection passes through: it
// is what makes DNS rebinding irrelevant, because the gateway applies
// it to the address it has already resolved and is about to dial.
func TestDecide(t *testing.T) {
	d, err := policy().Compile()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		"1.1.1.1":         true,  // ordinary public
		"140.82.121.4":    true,  // github
		"169.254.169.254": false, // cloud metadata
		"127.0.0.1":       false, // loopback
		"10.1.2.3":        false, // RFC1918
		"192.168.1.1":     false,
		"172.17.0.5":      false, // another Docker network
		"172.21.0.1":      false, // the uplink itself
		"203.0.113.9":     false, // a host interface network
		"224.0.0.1":       false, // multicast
		"0.0.0.0":         false,
		"255.255.255.255": false,
		"10.9.8.7":        true, // the operator's explicit allow, inside a denied range
		"::1":             false,
		"2606:4700::1111": false, // IPv6 has no path here at all
	}
	for text, want := range cases {
		addr := netip.MustParseAddr(text)
		if got := d.Allowed(addr); got != want {
			t.Errorf("Allowed(%s) = %v; want %v", text, got, want)
		}
	}
}

// TestRenderRules pins the gateway's own ruleset: it accepts DNS and
// proxy connections only from the capsule's subnet, forwards nothing
// at all, and never touches the nat table — flushing nat would destroy
// the daemon rules its own resolver depends on.
func TestRenderRules(t *testing.T) {
	out := policy().RenderIPTables("eth0", 3128)

	for _, must := range []string{
		":INPUT DROP", ":FORWARD DROP", ":OUTPUT DROP",
		"-A INPUT -i eth0 -s 172.20.0.0/24 -p udp --dport 53 -j ACCEPT",
		"-A INPUT -i eth0 -s 172.20.0.0/24 -p tcp --dport 3128 -j ACCEPT",
		"-A OUTPUT -d 169.254.0.0/16 -j REJECT",
		"-A OUTPUT -d 10.9.8.0/24 -j ACCEPT",
	} {
		if !strings.Contains(out, must) {
			t.Errorf("ruleset lacks %q\n%s", must, out)
		}
	}
	if strings.Contains(out, "*nat") || strings.Contains(out, "MASQUERADE") {
		t.Error("the gateway must not touch the nat table: flushing it destroys the daemon's embedded-resolver rules")
	}
	if strings.Contains(out, "-A FORWARD -") {
		t.Error("the gateway is a relay, not a router; no FORWARD rule may exist")
	}

	// The operator's allow renders before the denies, so a hole inside
	// a denied range works while the range itself stays denied.
	allowAt := strings.Index(out, "-A OUTPUT -d 10.9.8.0/24 -j ACCEPT")
	denyAt := strings.Index(out, "-A OUTPUT -d 10.0.0.0/8 -j REJECT")
	if allowAt == -1 || denyAt == -1 || allowAt > denyAt {
		t.Errorf("allow must render before deny: allow@%d deny@%d", allowAt, denyAt)
	}
}

func TestRenderIP6DeniesEverything(t *testing.T) {
	out := RenderIP6Tables()
	for _, must := range []string{":INPUT DROP", ":FORWARD DROP", ":OUTPUT DROP"} {
		if !strings.Contains(out, must) {
			t.Errorf("ip6 ruleset lacks %q", must)
		}
	}
	if strings.Contains(out, "ACCEPT") && !strings.Contains(out, "-o lo") {
		t.Error("ip6 accepts something beyond loopback")
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestTheKernelAndTheRelayAgreeAboutAnAllow: an operator gets one
// answer, or the configuration is a lie.
//
// The ruleset and the decider are two renderings of one policy, and the
// operator sees only the first. An allow the kernel accepts and the
// relay refuses leaves a rule in the firewall that matches the file, and
// a gateway that answers 403 to every request through it with nothing
// anywhere saying why.
func TestTheKernelAndTheRelayAgreeAboutAnAllow(t *testing.T) {
	for _, allow := range []string{
		"10.9.8.0/24",        // the ordinary case: a hole in a private range
		"169.254.169.254/32", // one address of link-local, named
		"100.64.7.0/24",      // CGNAT
		"198.18.5.0/24",      // benchmarking
	} {
		p := policy()
		p.Allow = append(p.Allow, allow)
		if err := p.Validate(); err != nil {
			t.Errorf("allow %s: rejected by the policy: %v", allow, err)
			continue
		}
		d, err := p.Compile()
		if err != nil {
			t.Fatal(err)
		}
		prefix := netip.MustParsePrefix(allow)
		addr := prefix.Masked().Addr()
		accepted := strings.Contains(p.RenderIPTables("eth0", 3128),
			"-A OUTPUT -d "+allow+" -j ACCEPT")
		if reached := d.Allowed(addr); accepted != reached {
			t.Errorf("allow %s: the ruleset accepts it = %v, the relay reaches %s = %v. "+
				"An operator reading the firewall and an operator reading the logs "+
				"see different policies.", allow, accepted, addr, reached)
		}
	}
}

// TestLinkLocalIsRefusedUntilItIsNamed: the default is what matters
// here, since a cloud instance keeps its own credentials there.
func TestLinkLocalIsRefusedUntilItIsNamed(t *testing.T) {
	metadata := netip.MustParseAddr("169.254.169.254")

	// A deny set that does not mention link-local, so the refusal has to
	// come from the decider. BuildDeny always includes the range, and a
	// policy carrying it would refuse the address through the list --
	// proving the list works, not this. The reload channel takes whatever
	// deny set it is handed, so a policy without it is reachable.
	thin := policy()
	thin.Deny = []string{"10.0.0.0/8"}
	if err := thin.Validate(); err != nil {
		t.Fatal(err)
	}
	plain, err := thin.Compile()
	if err != nil {
		t.Fatal(err)
	}
	if plain.Allowed(metadata) {
		t.Error("link-local is reachable with nothing denying it and nothing naming it; " +
			"a job that wanders into the metadata service arrives")
	}

	p := policy()
	p.Allow = append(p.Allow, "169.254.169.254/32")
	named, err := p.Compile()
	if err != nil {
		t.Fatal(err)
	}
	if !named.Allowed(metadata) {
		t.Error("an allow naming one link-local address does not take effect, so the entry is " +
			"accepted, rendered into the ruleset, and refused on every request")
	}
	// One address, not the range it sits in.
	if named.Allowed(netip.MustParseAddr("169.254.1.1")) {
		t.Error("naming one link-local address reopened the rest of the range")
	}
}

// TestWhatNoConnectionReachesCannotBeAllowed: an allow through one of
// these is a line that does nothing, so it is refused rather than
// accepted and ignored.
func TestWhatNoConnectionReachesCannotBeAllowed(t *testing.T) {
	for _, allow := range []string{
		"127.0.0.1/32",       // the gateway itself
		"224.0.0.1/32",       // multicast
		"255.255.255.255/32", // broadcast
		"0.0.0.0/32",         // no destination
	} {
		p := policy()
		p.Allow = append(p.Allow, allow)
		if err := p.Validate(); err == nil {
			t.Errorf("allow %s was accepted; the ruleset would carry its accept while the "+
				"relay refused every request through it", allow)
		}
		if !RefusedOutright(netip.MustParsePrefix(allow)) {
			t.Errorf("%s is not reported as unreachable, so the configuration validator "+
				"would accept it too", allow)
		}
	}
}
