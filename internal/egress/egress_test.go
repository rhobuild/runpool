package egress

import (
	"net/netip"
	"slices"
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
	// One address, not the range it sits in -- and against the thin deny
	// set, so the refusal is the decider's rather than the list's.
	thin.Allow = append(thin.Allow, "169.254.169.254/32")
	one, err := thin.Compile()
	if err != nil {
		t.Fatal(err)
	}
	if !one.Allowed(metadata) {
		t.Fatal("the named address is not reachable; nothing below means anything")
	}
	if one.Allowed(netip.MustParseAddr("169.254.1.1")) {
		t.Error("naming one link-local address reopened the rest of the range")
	}
}

// TestLinkLocalCannotBeReopenedWholesale: the widening rule does not
// reach this one, and without it the range is one line away.
//
// The baseline withholds link-local as a /16, and an allow of exactly
// that is broader than nothing, so it passes the rule that refuses an
// allow for what it would take with it. While the decider refused
// link-local before consulting the allow list such an entry was inert
// and costing nothing; consulted first, it hands a job the whole range
// its instance keeps its own credentials in.
func TestLinkLocalCannotBeReopenedWholesale(t *testing.T) {
	for _, allow := range []string{
		"169.254.0.0/16",   // exactly the withheld range: not broader than anything
		"169.254.0.0/17",   // half of it
		"169.254.169.0/24", // the neighbourhood of the metadata address
		"169.254.169.254/31",
	} {
		p := policy()
		p.Allow = append(p.Allow, allow)
		if err := p.Validate(); err == nil {
			t.Errorf("allow %s was accepted; it reaches link-local addresses nobody named", allow)
		}
	}
	// And the one shape that is the point of the field.
	p := policy()
	p.Allow = append(p.Allow, "169.254.169.254/32")
	if err := p.Validate(); err != nil {
		t.Errorf("naming one link-local address was refused: %v", err)
	}
}

// TestAnAllowInTheV4InV6FormIsRefused: it renders into the ruleset and
// never matches at decision time, because a 128-bit prefix contains no
// 32-bit address. That is the firewall agreeing with the file while the
// relay refuses everything, reached through notation rather than order.
func TestAnAllowInTheV4InV6FormIsRefused(t *testing.T) {
	mapped := "::ffff:198.18.5.0/120"
	p := policy()
	p.Allow = append(p.Allow, mapped)
	if err := p.Validate(); err == nil {
		d, cerr := p.Compile()
		if cerr != nil {
			t.Fatal(cerr)
		}
		t.Errorf("allow %s was accepted; it renders into the ruleset verbatim while the "+
			"relay reaches 198.18.5.7 = %v", mapped, d.Allowed(netip.MustParseAddr("198.18.5.7")))
	}
	q := policy()
	q.Deny = append(q.Deny, mapped)
	if err := q.Validate(); err == nil {
		t.Errorf("deny %s was accepted; it renders into an IPv4 ruleset and matches nothing", mapped)
	}

	// The reason has to be the true one. The rules about ranges compare a
	// prefix's width against IPv4 widths, so a mapped prefix reaches them
	// as something 128 bits wide and is refused for covering ranges it
	// does not cover: ::ffff:127.0.0.0/97 spans two billion addresses a
	// relay would reach, and being told it is loopback helps nobody.
	wide := policy()
	wide.Allow = append(wide.Allow, "::ffff:127.0.0.0/97")
	err := wide.Validate()
	if err == nil {
		t.Fatal("a mapped prefix spanning most of IPv4 was accepted")
	}
	if !strings.Contains(err.Error(), "IPv4") {
		t.Errorf("refused with %q; the prefix is refused for its notation, and any other "+
			"reason is one that is not true of it", err)
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

	// A prefix that begins at one of those addresses and holds ordinary
	// ones is not the same thing. 0.0.0.0/8 starts at an address no
	// connection reaches and covers sixteen million that do, so deciding
	// from the first address would refuse a range an operator may name.
	for _, allow := range []string{"0.0.0.0/8", "0.0.0.0/31", "224.0.0.0/3"} {
		if RefusedOutright(netip.MustParsePrefix(allow)) {
			t.Errorf("%s is reported as unreachable, but it holds addresses a relay reaches; "+
				"an operator naming it would be refused with a reason that is not true of it", allow)
		}
	}
}

// TestTheDenySetIsIPv4Whatever TheDaemonReports: what fills this set is
// a daemon, not an operator.
//
// A deny set renders into an IPv4 ruleset, and the policy refuses one
// carrying an address that is not IPv4 — so a host with a single
// IPv6-enabled Docker network would fail every capsule launch over
// something nobody in the deployment chose. Nothing is left reachable by
// dropping it: a capsule has no IPv6 at all.
func TestTheDenySetIsIPv4WhateverTheDaemonReports(t *testing.T) {
	deny := BuildDeny("172.21.0.0/24",
		[]string{"203.0.113.0/24", "2001:db8::/32"},
		[]string{"172.17.0.0/16", "fd00::/64"})

	for _, c := range deny {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			t.Errorf("deny set carries %q, which is not a prefix at all", c)
			continue
		}
		if !p.Addr().Is4() {
			t.Errorf("deny set carries %s; the ruleset it renders into is IPv4, and the "+
				"policy refuses a set carrying it, so every launch on this host fails", c)
		}
	}
	// And the v4 ones it was given are still there.
	for _, want := range []string{"172.21.0.0/24", "203.0.113.0/24", "172.17.0.0/16"} {
		if !slices.Contains(deny, want) {
			t.Errorf("deny set lost %s; another network on this daemon is never a legitimate "+
				"capsule destination", want)
		}
	}
	if err := (Policy{
		InternalSubnet: "172.20.0.0/24", UplinkSubnet: "172.21.0.0/24", Deny: deny,
	}).Validate(); err != nil {
		t.Errorf("the policy built from what the daemon reported does not validate: %v", err)
	}
}

// outputRules extracts the OUTPUT chain in order. The order is the
// decision: iptables evaluates top to bottom, so a terminal ACCEPT that
// renders above a REJECT does not "mostly work" — it makes every rule
// below it unreachable and the deny layer an accept-all.
func outputRules(out string) []string {
	var rules []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "-A OUTPUT ") {
			rules = append(rules, line)
		}
	}
	return rules
}

// TestTheRulesetIsHeldLineByLine: the existing render test checks that
// lines exist, and existence is not a ruleset. Moving the terminal
// ACCEPT above the deny loop kept every line present and every test
// green while the kernel accepted everything; adding one INPUT accept
// for the proxy port without a source subnet did the same while any
// gateway on the uplink relayed any capsule's traffic under its own
// policy. A chain is asserted as the ordered sequence it is evaluated
// as, and INPUT as the exact set of accepts it may hold.
func TestTheRulesetIsHeldLineByLine(t *testing.T) {
	out := policy().RenderIPTables("eth0", 3128)

	rules := outputRules(out)
	if len(rules) == 0 {
		t.Fatal("no OUTPUT rules at all, so this proves nothing")
	}
	last := rules[len(rules)-1]
	if last != "-A OUTPUT -j ACCEPT" {
		t.Fatalf("the last OUTPUT rule is %q; the unconditional accept must be the final rule", last)
	}
	for _, r := range rules[:len(rules)-1] {
		if r == "-A OUTPUT -j ACCEPT" {
			t.Fatalf("an unconditional accept renders above %q; every rule below it is unreachable "+
				"and the deny layer is an accept-all", rules[len(rules)-1])
		}
	}
	// Every REJECT is above the terminal accept by the assertion above;
	// what remains is that each is present for the whole deny set.
	for _, c := range policy().Deny {
		want := "-A OUTPUT -d " + c + " -j REJECT --reject-with icmp-admin-prohibited"
		if !slices.Contains(rules, want) {
			t.Errorf("the deny set entry %s has no REJECT rule", c)
		}
	}

	// INPUT is enumerated exactly. A rule added here is a door into the
	// gateway, and the door the source subnet exists to close is another
	// capsule's traffic arriving over the shared uplink.
	var input []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "-A INPUT ") {
			input = append(input, line)
		}
	}
	wantInput := []string{
		"-A INPUT -i lo -j ACCEPT",
		"-A INPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT",
		"-A INPUT -i eth0 -s 172.20.0.0/24 -p udp --dport 53 -j ACCEPT",
		"-A INPUT -i eth0 -s 172.20.0.0/24 -p tcp --dport 53 -j ACCEPT",
		"-A INPUT -i eth0 -s 172.20.0.0/24 -p tcp --dport 3128 -j ACCEPT",
	}
	if !slices.Equal(input, wantInput) {
		t.Errorf("INPUT chain =\n%s\nwant exactly\n%s", strings.Join(input, "\n"), strings.Join(wantInput, "\n"))
	}
}

// TestTheIP6RulesetIsExactlyLoopback: the previous assertion here could
// not fire — it excused any ACCEPT whenever "-o lo" appeared anywhere in
// the ruleset, and the loopback accept is always in it, so a fully open
// IPv6 ruleset passed. The capsule has no IPv6 and the ruleset is what
// turns "not enabled" into "denied even if something enables it", so it
// is asserted as the exact document it is.
func TestTheIP6RulesetIsExactlyLoopback(t *testing.T) {
	want := strings.Join([]string{
		"*filter",
		":INPUT DROP [0:0]",
		":FORWARD DROP [0:0]",
		":OUTPUT DROP [0:0]",
		"-A INPUT -i lo -j ACCEPT",
		"-A OUTPUT -o lo -j ACCEPT",
		"COMMIT",
		"",
	}, "\n")
	if got := RenderIP6Tables(); got != want {
		t.Errorf("ip6 ruleset =\n%s\nwant exactly\n%s", got, want)
	}
}

// TestTheBaselineDenyIsTheTwelveRanges. Writing the twelve out is the
// point: the baseline is the floor under every discovered deny, the
// validator refuses operator allowances against what it withholds, and
// a range deleted from it was withheld from capsules on every host —
// while the previous test pinned seven of the twelve, so the other five
// could go without a failure anywhere.
func TestTheBaselineDenyIsTheTwelveRanges(t *testing.T) {
	want := []string{
		"0.0.0.0/8",
		"10.0.0.0/8",
		"100.64.0.0/10",
		"127.0.0.0/8",
		"169.254.0.0/16",
		"172.16.0.0/12",
		"192.0.0.0/24",
		"192.168.0.0/16",
		"198.18.0.0/15",
		"224.0.0.0/4",
		"240.0.0.0/4",
		"255.255.255.255/32",
	}
	if !slices.Equal(baselineDeny, want) {
		t.Errorf("baselineDeny = %v\nwant the twelve ranges written here, so a deletion is a diff "+
			"in a security constant and not a silent narrowing", baselineDeny)
	}
}
