# The status document is the metrics interface

**Status:** implemented
**Date:** 2026-08-28

Runpool exposes no metrics endpoint, and `observability.metrics.enabled`
is refused rather than reserved. The machine-readable account of an
instance is `runpool status --json`, versioned by its own `api_version`
and read over the transport the platform already uses for every other
command. Deciding when a person has to look is the host's job, and
[the runbook](../runbook.md) says what to evaluate.

## What forced it

The field was accepted into the schema and refused at validation with
the word "yet", which is a promise with a date nobody set. Closing it
needed one question answered: what can an operator of one host not find
out today.

The answer is nothing. Every condition that requires a person is already
in `status --json` — disk pressure at either emergency level, an attempt
held for manual review, a binding that has stopped reaching its
provider, a queue that is not draining while credits are free, a
disagreement between the books and the daemon, and an unreadable engine.
It is computed from the store and the configuration by a read-only open,
not from controller memory, which is why `runpool healthcheck` already
works the same way.

What an operator cannot do is *be interrupted* by any of it. That is a
delivery problem, and one the host solves — a timer, an exit status, and
whatever already carries alerts on that machine — not a protocol problem
this project needs to grow a listener for.

The rate questions a metrics endpoint classically answers are the
provider's data, not this instance's: GitHub holds authoritative queue
time, run time and outcome for the exact runs this host served, in its
own interface. What is unique to the controller is host-side health, and
host-side health is a snapshot.

## What was rejected

**A Prometheus endpoint on a port.** The deployment guide says the
controller has "no port to publish and no HTTP surface to expose, which
is why there is none to protect". That sentence is a published v1.0.0
property, and the installation procedure has an operator verify the
rendered Compose model has no ports at all. On a shared daemon a port is
reachable by every colocated container, so it would immediately need
protecting — which creates the obligation the contract says does not
exist, in order to publish a document that already exists.

**The same over the operator socket.** Prometheus scrapes URLs, not unix
sockets, so every consumer would need a host-side proxy — and a proxy
that can reach the socket already holds the state directory, at which
point it can run `runpool status --json` and needs no second protocol.
The socket's own record argues it is not an endpoint because a
resolution is a *write* that belongs to the lock holder. Reads do not
inherit that argument, and serving them there would erode it.

**A metrics file in the state volume.** Writing `runpool.prom` for a
textfile collector needs no listener and no dependency, but consuming it
means mounting the state volume into a third-party process, which grants
reach to the database, the socket and the lock. A metrics-only volume
fixes the reach and amends the Compose contract for every platform that
derives from it. Either way it re-encodes the status document field for
field as a second compatibility surface. If a scrapeable file is ever
genuinely demanded, this is the shape, and it is additive.

**Pushing to a collector.** A dependency tree, an endpoint and
credentials in configuration, buffering and retry semantics, and a
choice of the operator's monitoring stack made by us. Fleet assumptions
imported into a product whose scope is one host.

## What follows from it

`observability.metrics` stays in the schema and keeps refusing `true`.
Configuration parsing is strict, and the shipped example carries
`metrics.enabled: false`, so removing the field now would fail the
startup of every deployment that copied it. Removal is a v2 break, and
the reference says so rather than implying arrival.

A future scrapeable surface, if one is ever justified, is a new field
with its own name — not this one waking up.
