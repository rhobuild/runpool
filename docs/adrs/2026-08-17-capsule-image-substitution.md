# An operator may name the capsule image, and the capsule declares its protocol

**Status:** accepted
**Date:** 2026-08-17

A release binary refuses `RUNPOOL_CAPSULE_IMAGE` whenever its built-in
capsule reference is digest-qualified (`internal/app/images.go`). The
shipped capsule is the upstream runner image plus the dind binaries,
`iptables`, `uidmap` and `iproute2` — not a toolchain image. A workload
that needs a system package has no way to add one, and no configuration
field names an alternative.

## What the refusal protects, and what it does not

It does not protect the host. The controller holds the host's Docker
socket, which is host-root authority, and every capsule runs a
privileged daemon. An operator who can configure Runpool can already
run any image with any privileges directly against that socket.
Refusing to launch their image removes a capability from Runpool
without removing it from the operator.

What the pin does protect is exactness: the controller launches the
image it was built against, and the digest cannot move underneath it.
That property survives an operator-supplied image as long as the
substitute is named by digest too.

What it also protects, and this one is real, is the meaning of a
qualified release. The gates observe one capsule. A substituted capsule
has not been through them.

## What a capsule must be

The capsule is not an arbitrary image. It is the other half of a control
protocol:

- `ENTRYPOINT` is the Runpool supervisor, which owns PID 1, boots the
  inner daemon and drains on `SIGTERM`.
- The supervisor answers three verbs over `docker exec` —
  `deliver` (JIT bundle on stdin), `start` and `state`.
- It writes the protocol version to `/run/runpool/protocol` on the
  control tmpfs at boot.

The version file exists and nothing reads it. A substituted image
carrying an older supervisor would be launched, would answer some verbs
and not others, and would fail as a job failure rather than as a
configuration error.

## Decision

**A tier may name its capsule image**, as `tiers[].capsuleImage`, and the
value must be digest-qualified. The tier already carries the shape of
the workload — cpu, memory, swap, pids — and the image belongs to that
shape. A target-wide field would not express a deployment whose heavy
tier needs a toolchain the quick tier does not.

`RUNPOOL_CAPSULE_IMAGE` keeps its current meaning for development
builds and keeps its refusal against a digest-qualified release
default. Configuration, not the environment, is where a deployment
states an image.

**The launcher reads `/run/runpool/protocol` before delivering.** A
capsule whose version is not the one this controller speaks is destroyed
and the lease resolved as a configuration failure, naming both versions.
An image that is not a Runpool capsule at all fails the same check,
because the file is absent.

**The image contract is documented** as a base to build from: the
published capsule is the base, an operator's image derives from it with
`FROM`, and the supervisor, its entrypoint and the control tmpfs are the
parts that may not be replaced.

## Consequences

- A deployment can add system packages, language toolchains or
  preinstalled software to its jobs without a fork.
- A substituted capsule is outside the configuration the release gates
  observed. `runpool status` reports the image each binding launches, so
  the deviation is visible rather than inferred, and the support
  statement names it.
- The version check turns a class of silent, per-job failures into one
  refusal at launch with both versions in the message.
- The protocol version becomes a compatibility surface: changing what
  the supervisor accepts means changing the version, and a controller
  refuses the pair it does not speak.
