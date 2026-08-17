// Package egress is the capsule egress policy: what the controller
// computes, what travels to the gateway, and what the gateway enforces.
// It is pure — the deny set, the decision for one address, and the
// gateway's own ruleset are functions of their inputs — so the policy a
// capsule runs under is reproducible and testable without a kernel.
//
// The topology this serves (see docs/adrs/2026-08-13-egress-relay.md):
// the capsule's only network is an internal bridge in Engine 28's
// isolated gateway mode, which makes the host kernel drop every packet
// the capsule sends to any address outside that bridge's own subnet.
// That deny is absolute — a privileged capsule rewriting its own
// routes and firewall cannot lift a rule that lives in the host's
// namespace. What the capsule can still reach is its bridge
// neighbours, which is exactly one container: the gateway.
//
// Egress therefore is not routing but relaying. The gateway resolves
// names for the capsule and opens connections on its behalf through
// its second leg, applying this policy to every address it is about to
// connect to. Because the gateway resolves and then connects to the
// address it validated, a resolver that answers with a private address
// — DNS rebinding — changes nothing.
package egress

import (
	"fmt"
	"net/netip"
	"strings"
)

// ProxyPort is where the relay listens on the internal leg. It is the
// capsule's only outbound surface, published to it as HTTP(S)_PROXY.
//
// It lives here rather than in the gateway because both sides of the
// relationship need it: the gateway to listen, and the capsule to point
// its proxy variables at. This package is the policy vocabulary they
// already share, so naming the port here is what lets the capsule stop
// importing the whole gateway for one integer.
const ProxyPort = 3128

// Policy is what the gateway needs: the subnets that identify its two
// legs, and the allow/deny sets it applies to every destination. Allow
// is consulted before Deny, so an operator's allowPrivateCIDRs punch a
// named hole through the private-range denies without weakening
// anything else.
type Policy struct {
	InternalSubnet string   `json:"internal_subnet"`
	UplinkSubnet   string   `json:"uplink_subnet"`
	Allow          []string `json:"allow,omitempty"`
	Deny           []string `json:"deny"`
}

// Validate parses every prefix so a malformed policy fails before the
// gateway reports ready — half a policy is an open gateway.
func (p Policy) Validate() error {
	if _, err := netip.ParsePrefix(p.InternalSubnet); err != nil {
		return fmt.Errorf("internal subnet: %w", err)
	}
	if _, err := netip.ParsePrefix(p.UplinkSubnet); err != nil {
		return fmt.Errorf("uplink subnet: %w", err)
	}
	if len(p.Deny) == 0 {
		return fmt.Errorf("deny set is empty; a policy that denies nothing is not a policy")
	}
	for _, c := range append(append([]string{}, p.Allow...), p.Deny...) {
		if _, err := netip.ParsePrefix(c); err != nil {
			return fmt.Errorf("cidr %q: %w", c, err)
		}
	}
	return nil
}

// Decider is a compiled policy: parsed once, consulted per connection.
type Decider struct {
	allow []netip.Prefix
	deny  []netip.Prefix
}

// Compile parses the policy's prefixes. It fails on the first bad one,
// so a Decider always represents a complete policy.
func (p Policy) Compile() (*Decider, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	d := &Decider{}
	for _, c := range p.Allow {
		prefix, err := netip.ParsePrefix(c)
		if err != nil {
			return nil, err
		}
		d.allow = append(d.allow, prefix)
	}
	for _, c := range p.Deny {
		prefix, err := netip.ParsePrefix(c)
		if err != nil {
			return nil, err
		}
		d.deny = append(d.deny, prefix)
	}
	return d, nil
}

// Allowed reports whether the gateway may connect to an address.
// Anything that is not an ordinary global unicast IPv4 address is
// refused outright: IPv6 has no path here, and the special-purpose
// forms (loopback, link-local, multicast, unspecified) are the ones an
// attacker reaches for. An explicit allow prefix wins over a deny;
// otherwise a deny match refuses; otherwise the address is public and
// permitted.
func (d *Decider) Allowed(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.Is4() || !addr.IsGlobalUnicast() || addr.IsLoopback() ||
		addr.IsLinkLocalUnicast() || addr.IsMulticast() || addr.IsUnspecified() {
		return false
	}
	for _, p := range d.allow {
		if p.Contains(addr) {
			return true
		}
	}
	for _, p := range d.deny {
		if p.Contains(addr) {
			return false
		}
	}
	return true
}

// baselineDeny is every IPv4 range that is never a legitimate public
// destination for a CI job: the private ranges, loopback, link-local
// (cloud metadata lives there), CGNAT, benchmarking, multicast,
// reserved, and broadcast. Operator allowPrivateCIDRs punch holes
// through it at decision time; nothing widens the list away.
var baselineDeny = []string{
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

// BuildDeny composes the deny set: the baseline, the host's own
// interface networks, every Docker network subnet the daemon knows,
// and the uplink subnet. Duplicates are dropped, order is stable.
func BuildDeny(uplinkSubnet string, hostCIDRs, dockerSubnets []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(c string) {
		if c != "" && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	for _, c := range baselineDeny {
		add(c)
	}
	add(uplinkSubnet)
	for _, c := range hostCIDRs {
		add(c)
	}
	for _, c := range dockerSubnets {
		add(c)
	}
	return out
}

// WidensBaselineDeny reports whether prefix would reopen a whole range the
// baseline refuses, as a side effect of being broader than it.
//
// Allow is consulted before deny, so `0.0.0.0/0` in allowPrivateCIDRs
// readmits every private range while the profile still reads
// `public-internet-only` — the hazard this exists to refuse. What it must
// not refuse is the two legitimate shapes: a prefix wholly inside a denied
// range (a targeted reopening, which is the field's whole purpose), and a
// prefix in public space, which is a no-op because the profile already
// permits the public internet — and which is how an operator reaches a
// service on the host's own public address, since BuildDeny adds the host's
// interface CIDRs at runtime.
//
// So the rule is containment in one direction only: an allow that strictly
// contains a denied range takes that whole range with it.
func WidensBaselineDeny(prefix netip.Prefix) bool {
	for _, raw := range baselineDeny {
		denied, err := netip.ParsePrefix(raw)
		if err != nil {
			continue
		}
		if prefix.Bits() < denied.Bits() && prefix.Contains(denied.Addr()) {
			return true
		}
	}
	return false
}

// RenderIPTables is the gateway's own ruleset, applied to the filter
// table only. The nat table is deliberately untouched: the gateway
// routes nothing, and flushing nat would destroy the daemon-installed
// rules that make its own embedded resolver work.
//
// The shape:
//   - INPUT drops everything except loopback, established replies, and
//     DNS and proxy connections from the capsule's subnet. Other
//     gateways share the uplink, so restricting by source subnet is
//     what keeps one capsule's gateway from serving another's.
//   - FORWARD drops everything: the gateway is a relay, never a
//     router, and a rule that would forward is a rule that would
//     defeat the address policy applied in userspace.
//   - OUTPUT allows loopback and established, refuses the deny set as
//     a second layer under the proxy's own check, and permits the
//     rest so the relay can reach public destinations.
func (p Policy) RenderIPTables(internalIf string, proxyPort int) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	w("*filter")
	w(":INPUT DROP [0:0]")
	w(":FORWARD DROP [0:0]")
	w(":OUTPUT DROP [0:0]")
	w("-A INPUT -i lo -j ACCEPT")
	w("-A INPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT")
	w("-A INPUT -i %s -s %s -p udp --dport 53 -j ACCEPT", internalIf, p.InternalSubnet)
	w("-A INPUT -i %s -s %s -p tcp --dport 53 -j ACCEPT", internalIf, p.InternalSubnet)
	w("-A INPUT -i %s -s %s -p tcp --dport %d -j ACCEPT", internalIf, p.InternalSubnet, proxyPort)
	w("-A OUTPUT -o lo -j ACCEPT")
	w("-A OUTPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT")
	for _, c := range p.Allow {
		w("-A OUTPUT -d %s -j ACCEPT", c)
	}
	for _, c := range p.Deny {
		w("-A OUTPUT -d %s -j REJECT --reject-with icmp-admin-prohibited", c)
	}
	w("-A OUTPUT -j ACCEPT")
	w("COMMIT")
	return b.String()
}

// RenderIP6Tables denies IPv6 outright: the capsule's network does not
// enable it, and a default-deny ruleset makes "not enabled" into
// "denied even if something enables it".
func RenderIP6Tables() string {
	return strings.Join([]string{
		"*filter",
		":INPUT DROP [0:0]",
		":FORWARD DROP [0:0]",
		":OUTPUT DROP [0:0]",
		"-A INPUT -i lo -j ACCEPT",
		"-A OUTPUT -o lo -j ACCEPT",
		"COMMIT",
		"",
	}, "\n")
}
