# Capsule egress is a relay, not a route

**Status:** accepted and implemented
**Date:** 2026-08-13
**Supersedes:** the routed-gateway topology

The restricted profile provides no route out and a policy-enforcing relay
for permitted destinations. The implementation is covered by a live contract
suite, which the release workflow runs without skips on the locked platform.

> This record is the implementation decision. The constraint it rests on
> was already measured and written down two days earlier in
> [Plain L3 routing over a Docker internal network is rejected](2026-08-11-network-sandbox-proxy.md),
> including the conclusion that a gateway which *terminates* connections
> works where one that forwards cannot. The findings below independently
> confirm that earlier measurement.

## The design that could not be built

The design specified a per-capsule internal bridge in Engine 28's
`isolated` gateway mode, plus a per-capsule gateway container attached
to both that bridge and a Runpool uplink, with the capsule's default
route pointed at the gateway. The gateway would forward what its policy
allowed and NAT it out the uplink.

Built exactly that way, no packet ever reached the gateway. The
measurements, on the original measurement host (Engine 28.5.0, cgroup v2/systemd,
`br_netfilter` loaded, `bridge-nf-call-iptables=1`):

- the capsule's routes, the gateway's two legs, `ip_forward`, the
  installed ruleset and the NAT rule were all exactly as intended;
- the gateway's own `FORWARD` chain counted **zero packets**, including
  its default DROP;
- the host's `DOCKER-ISOLATION-STAGE-1` chain counted the missing
  packets against
  `-i br-<capsule> ! -d <capsule subnet> -j DROP`.

That rule is how Docker implements `--internal`. With bridge netfilter
enabled — Docker's own default — it applies to frames bridged *within*
the network too, so a packet the capsule addresses to `1.1.1.1` is
dropped by the host as it crosses the bridge toward the gateway, before
any container-side rule is consulted. The daemon also refuses
`gateway_mode=isolated` on a non-internal network ("gateway mode
'isolated' can only be used for an internal network"), so the two
options cannot be separated: an internal bridge cannot host a router.

## What the constraint actually gives us

The same rule that broke the topology is a stronger deny than the
gateway could have enforced. The host drops **every** packet the capsule
addresses outside its own bridge subnet, in the host's namespace, where
a privileged capsule has no reach. Rewriting its routes, flushing its
firewall, or running anything at all inside dind does not lift it. The
default-deny is therefore kernel-enforced and unconditional, rather than
enforced by a container that must stay correct.

What remains reachable from the capsule is its bridge neighbours, which
is exactly one container: the gateway.

## Decision

Egress is a relay. The gateway keeps both legs and the policy, and it
serves the capsule two things on the internal leg:

- **DNS**, relayed to the daemon's embedded resolver;
- **an HTTP proxy** (CONNECT and absolute-URI), which resolves the
  destination itself, checks every resolved address against the policy,
  and connects only to an allowed one.

The capsule is created with its resolver and `HTTP(S)_PROXY` pinned to
the gateway, inherited by the runner, the job and the inner daemon, so
checkouts, package installs and image pulls all take that path.

Resolve-then-dial in the gateway is also the DNS rebinding defense: the
address that is checked is the address that is dialed, and a name is
never the unit of policy.

The gateway's own ruleset is filter-only. It accepts DNS and proxy
connections from the capsule's subnet, forwards nothing, and refuses the
deny set on OUTPUT as a second layer under the proxy's check. It does
not touch the nat table: flushing nat destroys the daemon-installed
rules for the embedded resolver the gateway itself resolves through —
found the same way, by measurement.

## Consequences

The deny has two halves, and they are dropped by different mechanisms. The
`internal` flag installs the kernel rules that drop traffic leaving the bridge,
which covers every destination outside its own subnet. The isolated gateway
mode leaves the bridge with no host address at all, which covers the
destinations the first half never sees: traffic to a host-local address is
delivered without passing the chain that `internal` installs.

Each half is proved by the thing that can see it. The kernel drop is proved by
the live bypass suite, on a kernel, with a privileged capsule trying to lift
it. The isolated mode is proved by the preflight, which asks the daemon what it
assigned the bridge rather than whether it accepted the request — an option key
a daemon does not recognise is dropped without complaint, so a create tells it
nothing. The preflight does not prove the kernel drop, and the bypass suite
does not prove the mode.

- **Kernel-enforced deny.** No route out exists at all; the bypass suite
  asserts unproxied TCP to public addresses fails just as private ones
  do.
- **Only proxy-aware traffic leaves.** HTTPS, HTTP and anything
  tunnelled over CONNECT work. Protocols that ignore proxy environment
  variables — `git+ssh`, arbitrary TCP to a service on the internet — do
  not. They fail closed, which is the safe direction, but it is a real
  functional limit of the restricted profile and is documented as one.
  Service containers are unaffected: they run inside the capsule, on the
  inner daemon's own network.
- **`unsafe-open-egress` keeps its meaning.** It builds no sandbox: a
  plain bridge with host egress, for deployments that accept it.
- Restoring full L3 egress later means giving up either the host's
  `--internal` rule (a host firewall change through `DOCKER-USER`) or
  the shared bridge (a veth pair plumbed between namespaces). Both are
  more privileged than this; neither is needed for the qualified
  profile.
