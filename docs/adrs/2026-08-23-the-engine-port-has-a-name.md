# The container engine is a port, and the Moby client is one adapter behind it

**Status:** implemented
**Date:** 2026-08-23

## Context

[The Docker API client record](2026-08-12-docker-api-client.md) decided that
all daemon operations live behind one adapter and no domain package imports
Moby types. The tree obeyed it. What it did not have was a name: the adapter's
own types left it under a `docker.` qualifier, so a package that launches
capsules said `docker.ContainerSpec` when it meant *a container to run*, and
six packages held either that vocabulary or the concrete client.

The cost was not aesthetic. A concrete client cannot be told to be
unreachable, to have lost a container, or to refuse a name that is taken —
and those are the answers that decide whether an attempt is settled or held
for a person. Those paths were reachable only from a live daemon, which is to
say only in the half of the suite that cannot ask for a refusal.

## Decision

**`internal/engine` is the container engine as Runpool needs it.** It holds
the vocabulary — a container to run, a network to create, an execution's
state, an owned object, the ownership every object carries — and the three
answers a caller acts on: absence, a name already taken, a daemon that cannot
be reached. It holds no client and reaches nothing.

**`internal/engine/docker` is the Moby adapter**, and the only place Moby's
types appear. An architecture test confines the SDK to it rather than leaving
the rule as prose.

**Consumers declare the interface they need.** `internal/app`,
`internal/cache`, `internal/capsule`, `internal/command` and `internal/lease`
each name the operations they use, so a fake can answer them. The adapter's
concrete client satisfies each structurally and knows about none of them.

**The label values are a compatibility surface.** A controller sweeps the
objects an older controller stamped, so the exact document `Ownership` writes
is pinned by a test rather than by convention.

## Consequences

- The packages that launch capsules and manage cache lanes depend on the
  port, not on an adapter. `internal/app` and `internal/command` still know
  the adapter because constructing it is what a composition root does, and
  `internal/doctor` holds it for one field, where a nil client has to stay a
  nil interface.
- Paths that only a refusal reaches are covered hermetically: an execution
  nobody can decide, a create that failed for a reason other than a taken
  name, a removal the daemon will not do.
- A second adapter is possible and is not implied. Nothing here anticipates
  one, and the split earns itself on the engine there is.

## What a second engine would still need

Not an adapter. Three things the port cannot supply:

**The isolated bridge.** The restricted network profile rests on Engine 28's
`gateway_mode` options, which is what leaves a capsule with no route out —
the product's headline promise. netavark has no equivalent, so a Podman host
could serve `unsafe-open-egress` and nothing else, which the product contract
refuses to promise. A spike starts by answering what replaces that, not by
writing API calls.

**A version scale that is not Docker's.** `platform.MinimumEngineMajor` is 28
because Engine 28 introduced that mode. Another engine's numbering says
nothing about it, so the floor would have to become a capability the engine
is asked for rather than a number it is compared against.

**Exec with a real stdin.** The per-runner credential travels over `exec`
onto a tmpfs and is persisted nowhere. An engine whose exec cannot take stdin
would have to put it somewhere, and that somewhere is durable.

Until those are answered, this record decides the shape and claims nothing
about a second engine.
