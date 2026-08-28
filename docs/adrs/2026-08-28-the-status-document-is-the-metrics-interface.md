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

The answer is nothing, across nine conditions: disk pressure at either
emergency level, a disk measurement that has stopped arriving, an attempt
held for manual review, a binding that has stopped reaching its provider,
a queue that is not draining while credits are free, a disagreement
between the books and the daemon, an unreadable engine, a live lease that
has stopped moving, and a default capsule image that cannot be
resolved. It is computed from the store and the configuration
by a read-only open, not from controller memory, which is why
`runpool healthcheck` already works the same way.

Three of those eight were not in the first list drawn up for this
decision, and two of them are worth recording, because one was not a gap
in a list at all.

A **lease that has stopped moving** was: it is not terminal, so it holds an admission
credit, and it is deliberately not a discrepancy because the lease is
live. It is in the document — `leases[].state` — and the first list
simply did not name it. Worse, a host wedged entirely on quarantined
leases has no free credits, which is the condition under which "the queue
is not draining" stays quiet: the worse the wedge, the less the rest of
the list had to say.

A **binding that cannot persist what it is handed** was a defect in the
controller, not in the list. The poll that carries a message records
contact when it succeeds, which is right — the provider answered. But a
delivery that could not be persisted was left for redelivery, and
redelivery arrives through another poll, which records contact again. A
binding wedged that way refreshed its own health forever while nothing it
was offered became an attempt, so it was not queued either. It read as
healthy from every angle. The acquisition failure beside it already
recorded a failure for exactly this reason; its two sibling branches did
not. They do now, which is what makes this record's claim true rather
than narrower.

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

What is watched is a shape rather than a list of names, and that is part
of the decision rather than an implementation detail of a script. An
enumerated set of states to worry about loses to a state machine: a lease
comes to rest in quarantine, and it also comes to rest when the commit
that would have released it keeps failing, and the next way will not be
on a list written today. So the check is "not terminal, not running work,
and older than the longest a healthy one is bounded to" — and a second,
looser one for a lease that outlived the job ceiling in any state at all.
The disk is watched the same way: its level is only rewritten by a
measurement that succeeded, and the probe measures by running a
container, so watching the level alone reports "normal" straight through
a full disk. What is watched is the timestamp.

Rates and durations are out of this decision's scope rather than swept
into it by omission. How long jobs took, how many failed and how long
they queued are the provider's records for the exact runs this host
served, and what is durable on this side is the attempt trail, the audit
log and evidence, which is never pruned. Turning that into rates would be
a command that reads the store and prints them — a separate decision,
made when something needs them, and not a thing an endpoint would have
been the answer to.
