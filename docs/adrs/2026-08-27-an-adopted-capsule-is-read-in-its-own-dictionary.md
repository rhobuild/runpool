# An adopted capsule is read in its own dictionary

**Status:** implemented
**Date:** 2026-08-27

A controller confirms a running capsule's control protocol before it
trusts any state word that capsule reports, including capsules it adopted
rather than launched. A capsule whose protocol it does not speak is
unavailable — held for a person — rather than interpreted.

Its exit code is the exception, and is trusted from every version. The
reserved abort code is never renumbered.

## What forced it

A controller replaced while capsules run adopts them, which is a promise
the product makes: a redeploy does not re-run somebody's job. The launch
that would have refused an unspeakable protocol belonged to the
controller before it, and nothing re-asks.

Both bumps this protocol has had moved what an existing word means.
Version 2 moved when `waiting` is written; version 3 split `starting` out
of `waiting`, because a version 2 supervisor answers `waiting` for the
whole start preamble. A launcher reads `waiting` as proof the runner
never started — so an adopted version 2 capsule, asked by a version 3
controller, reports a word that is understood, wrongly, and the
assignment it is forking a runner for is returned to the queue.

Nothing refuses that. An unknown word already fails safe; a known word
whose meaning moved is the shape no check downstream can catch.

## What was rejected

**Re-running the launch check at adoption and unwinding a mismatch.**
That kills running jobs on every protocol bump, which is the promise
adoption exists to keep. The adoption happy path reads no state word at
all — it waits for an exit — so there is nothing to protect there.

**Trusting the exit code less.** It is the one value that survives the
capsule: the control surface is a tmpfs that dies with the container, so
an aborted capsule has no file left to declare a version. Requiring one
would leave every adopted abort unclassifiable, which settles work that
never ran as complete. The invariant runs the other way — the number is
frozen, and the test that says so is never updated.

## What it costs

One exec, on the path that inspects an ambiguous start. That path already
holds an attempt for a person when it cannot reach the capsule, so a
protocol read that fails reaches an operator the same way the state read
already did.

An operator meets a new refusal: a capsule from an older release, still
running under a newer controller, whose attempt is held rather than
classified. That is the correct outcome and it was previously a silent
misclassification, but it is a case that did not exist before and the
runbook's manual-review section is where it lands.
