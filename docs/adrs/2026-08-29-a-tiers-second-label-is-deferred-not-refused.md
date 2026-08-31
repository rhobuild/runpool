# A tier's second label is deferred, not refused

**Status:** accepted
**Date:** 2026-08-29

A tier is reached by exactly one label: the name of its scale set.
Runpool sets no others, and `tiers[].labels` does not exist.

That is a decision about what is built, not a claim about what GitHub
permits. Serving more than one label per tier is possible and unsafe
today for a reason that could be removed; nobody has removed it, and
the thing it would buy is already available another way.

## What forced it

An operator asked whether `runs-on: [self-hosted, acme]` could reach a
tier. Two documents answered by saying GitHub matches a scale set "by
its name and nothing else", which had stopped being true: multi-label
scale sets exist. The example those sentences illustrated was still
right for a different reason — a scale-set runner receives no default
labels at all, so none of those three words belongs to anything here.

Correcting the sentence needed the matching rule, and the matching rule
is not published for scale sets. So it was measured.

## What is measured

Against live GitHub, from `test/contract/githubactions`:

- An unlabeled scale set receives exactly one label, of type system,
  equal to its name. `TestScaleSetSystemLabel`.
- **A scale set is matched on holding all of the labels a job asks for,
  not on carrying exactly them.** A job asking for two labels was
  offered to the only set present, which carried those two and a third.
  `TestObservationJobReachesScaleSetWithSupersetLabels`.

So two tiers whose labels overlap both answer one `runs-on`, and which
of them serves a job — with its resource ceiling, its capsule image and
its egress policy — is a tie nothing documents breaking.

## What looked measured and was not

A test asserted that a scale set given custom labels stops answering to
its own name, and failed, and that failure was read as a finding.

It was not one. The set was created carrying a single label that was not
its name, and the dispatch asked for the name. Under the rule the test
beside it had just established, nothing could have arrived: the label
asked for was never among the labels carried. The experiment never
varied the thing it named, and the conclusion drawn from it — that
giving a tier labels silently breaks every workflow pointing at it — is
not supported by anything.

It is recorded because the sentence it produced was nearly written into
the documentation as evidence, and because a test that fails for the
wrong reason looks exactly like one that fails for the right one.

## What would make a second label safe

Two things, neither of which exists here.

**The name kept.** The SDK adds the name-equal label only when the
caller supplies none, so a caller that supplies any builds the whole set
itself. `actions/actions-runner-controller` does this: its reconciler
puts the scale set's name in first, unconditionally, then appends what
an operator asked for. Runpool would have to do the same, or every
workflow already written against a tier's name would stop reaching it.

**The labels proven disjoint.** A request reaches two tiers only when it
is a subset of both their label sets, so it must be a subset of their
intersection. Make every tier's labels pairwise disjoint within a target
and that intersection is empty — and `runs-on` is never empty, so no
request a workflow author can write reaches two tiers. This removes the
ambiguity rather than narrowing it.

The scope that has to be checked is the one that already refuses two
tiers of a target sharing a scale-set name, so it is the same validation
extended rather than a new kind. It has the same blind spot that one
has: it cannot see a scale set belonging to somebody else in the same
organization.

Nothing upstream does this second half. ARC deduplicates a scale set's
labels against its own name and does not compare them with any sibling's.

## Why it is not built

What it buys is not having to edit workflows during a migration off
classic self-hosted runners. That edit is always available, changes one
line per workflow, and carries no risk. A platform team serving
repositories it does not own would rather not make it, which is a real
ask — and it is entirely substitutable, which a capability gap is not.

## What would change this

A confirmed measurement that a scale set carrying its name and another
label still routes a bare `runs-on: <name>`, which is the one thing the
retracted test was reaching for and never showed. Plus an operator who
wants the migration case enough to accept the validation that makes it
safe.

Until both, the single name is what a tier answers to, and this record
is why.
