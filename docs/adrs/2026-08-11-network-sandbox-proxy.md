# Plain L3 routing over a Docker internal network is rejected

**Status:** accepted; implemented by [the egress relay](2026-08-13-egress-relay.md)
**Date:** 2026-08-11
**Scope:** rejects one topology — direct L3 routing across an `internal`
bridge — not transparent sandboxing as such. Finding 3 below is the
shape the implementation eventually took.

> Independently re-measured on 2026-08-13. The routed topology produced the
> same findings:
> zero packets in the gateway's `FORWARD` chain, the drop counted by
> `DOCKER-ISOLATION-STAGE-1`, and the daemon refusing `isolated` without
> `internal`.

## Context

The evaluated topology used a per-capsule network sandbox: an internal bridge in
Engine 28's `isolated` gateway mode so it carries no host address, a
per-capsule egress gateway attached to both that bridge and a
Runpool-owned uplink, and a short-lived route initializer that points
the capsule's default route at the gateway. Traffic would then be
forwarded by the gateway, which applies a default-deny policy.

A live measurement on Docker Engine 28.5 shows that topology cannot work.

## Findings

**1. `internal` blocks forwarding at the host, not at the bridge.**
Docker implements an internal network with host-level rules:

```text
-A DOCKER-ISOLATION-STAGE-1 ! -d 172.23.0.0/16 -i br-<id> -j DROP
```

Any packet entering the host from that bridge whose destination lies
outside the bridge's own subnet is dropped by the host. With
`br_netfilter` enabled — the norm on Docker hosts — this applies even to
frames bridged toward another container on the same network. A capsule
that sets its default route to a gateway container therefore emits
packets addressed to `1.1.1.1`, and the host drops them before the
gateway ever sees them. Measured: the gateway's `FORWARD` chain counted
zero packets, while a ping to the gateway's own address (a destination
inside the subnet) succeeded.

**2. `isolated` gateway mode requires `internal`.** Docker refuses the
combination the design implies as an alternative:

```text
gateway mode 'isolated' can only be used for an internal network
```

So "no host gateway address on the bridge" cannot be obtained without
also accepting the destination-based drop rules.

**3. The drop rule keys on destination, which leaves one path open.**
Traffic addressed *to the gateway's own internal address* is inside the
subnet and is delivered normally. The gateway can then originate its own
connections from its uplink interface, which are not subject to the
rule. A gateway that terminates connections — a proxy — works where a
gateway that forwards packets cannot.

## Evidence

A capsule running the real runner image on an internal, isolated bridge,
with a proxy on the gateway's internal address (Engine 28.5, forge):

| Probe | Result |
|---|---|
| Direct HTTPS to api.github.com, no proxy | blocked |
| HTTPS to api.github.com through the proxy | reachable |
| Host address `:22` through the proxy | blocked |
| `docker0` gateway `:80` through the proxy | blocked |
| Host address, RFC1918, metadata, loopback (routing probe) | blocked |

## What this rejects, and what it does not

Rejected: **direct L3 routing** from the capsule to a gateway across an
`internal` bridge — the capsule emitting packets addressed to public
destinations and expecting a neighbour container to forward them.

Not rejected: transparent sandboxing in general. Finding 3 says the host
rule keys on the *destination of the packet that leaves the bridge*. Two
shapes satisfy it: a proxy, which terminates connections at the
gateway's address; and **encapsulation**, where the capsule's inner
traffic is wrapped in an outer packet addressed to the gateway's
internal address, decapsulated there, filtered, and NATed out a
Runpool-owned uplink. Encapsulation keeps transparency — arbitrary
protocols, no proxy variables — while still satisfying the rule.

Encapsulation was evaluated separately and rejected for V1 on portability and
privilege grounds. The accepted per-capsule gateway is a **relay**, not a
router:

- the capsule network stays `internal` with `gateway_mode=isolated`, so
  the host itself enforces default-deny egress;
- the gateway attaches to that network and to the Runpool uplink, runs
  a DNS relay and an HTTP `CONNECT` proxy with an address-and-port policy,
  and holds no socket, secret or volume;
- the runner, job, and inner daemon inherit
  `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`; and
- the route initializer disappears. The gateway receives `NET_ADMIN` only for
  its own fail-closed ruleset; the capsule receives no network capability.

This is stronger than the routed topology in one respect and weaker in
another, and both must be stated plainly:

- **Stronger:** egress is denied by the host kernel, not by rules
  Runpool installs. A gateway that crashes, a policy that fails to load,
  a workflow that manipulates routes inside privileged dind — none of
  them produce egress. Fail-closed is structural.
- **Weaker:** it is not transparent. Only protocols that traverse an
  HTTP proxy work; a workflow that ignores the proxy variables has no
  egress at all rather than unfiltered egress. Arbitrary TCP is unsupported.
  DNS is served by the gateway, while relay connections resolve and enforce
  policy against the exact address they dial.

## Superseding decision

V1 adopts the policy-enforcing relay described in
[the egress relay ADR](2026-08-13-egress-relay.md). It preserves the
kernel-enforced no-route property and makes the compatibility cost explicit.
Transparent L3 egress is not part of the V1 restricted profile.
