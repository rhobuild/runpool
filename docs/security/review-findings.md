# Security review findings

[Release readiness](../release-readiness.md) holds the release authorization
gate closed until every finding affecting the release boundary is resolved or
explicitly accepted. This is where that is recorded.

[Review scope](review-scope.md) defines the boundary and the questions a review
answers. This file records what came back.

## State

One external review has been conducted, against `c8ac420`. It reported no
finding that blocks the release boundary, and one finding recorded below.

That finding was resolved after the review ended, and the resolution is held
by the regression tests its entry names rather than re-confirmed by the
reviewer. A review covers the commit it read: a later tree is covered only to
the extent it has not moved.

## Recording a finding

One entry per finding, appended in the order received, identified `RP-SEC-NNN`
in sequence. Identifiers are never reused, and an entry is never deleted — a
finding that turns out to be invalid is recorded as such.

Each entry states:

- **Reported** — the date and who reported it
- **Surface** — which of the four boundary surfaces it lands on: admission, the
  capsule envelope, egress, or ownership
- **Claim** — what the reviewer says happens, in their terms
- **Assessment** — whether it reproduces, and against which commit
- **Disposition** — `resolved`, `accepted`, or `invalid`

A resolved finding names the commit that changed the behaviour and the test
that would catch it returning. Without that test the finding is not resolved,
because nothing stops it coming back.

An accepted finding names the reasoning, who accepted it, and what would make
it unacceptable later. "Accepted" without that third part is indistinguishable
from having ignored it.

An invalid finding keeps its entry and says why it does not hold. Reviewers
read this file before starting, and a reported behaviour with no recorded
outcome reads as unexamined.

## Findings

### RP-SEC-001 — a forged account was overruled on one path only

- **Reported** — 2026-08-26, external security review of `c8ac420`.
- **Surface** — the capsule envelope, reaching attempt disposition.
- **Claim** — the threat model states that a refusal naming a runner as still
  busy outranks the capsule's own word that it never started, so a forged
  refusal-to-start cannot return an assignment to the queue while the provider
  considers it in flight. That gate ran on the serving loop's failure path. The
  recovery that resumes an interrupted release called the finalizing
  transaction directly, and a forged `created` observation there mapped
  straight to a requeue without the provider being asked.
- **Assessment** — reproduces against `c8ac420`. A job can write the state
  file: it is a tmpfs the job's own privileged daemon can bind-mount.
  Reaching the path additionally requires the controller to die between
  moving the lease to cleaning and finalizing it. The impact is bounded — a
  requeue cannot re-run work, because nothing is stored to replay and the
  provider arbitrates one runner per job. The reviewer rated it low and did
  not consider it release-blocking.

- **Disposition** — `resolved` in
  [#122](https://github.com/rhobuild/runpool/pull/122). The overrule is one
  function, and the read-back it rules on happens inside it rather than in
  each caller — asked in the wrong order, the provider answers about an
  absence and its refusal is discarded. Every path that ends a serving calls
  it: the failure handler, the resumed release, and the invariant sweep.
  Three tests fail without it, each naming the attempt returned to the queue:
  `TestAForgedAccountLosesToTheProviderOnEveryPath`,
  `TestARecordedForgeryLosesToTheProviderToo` and
  `TestAStrandedAttemptAsksTheProviderToo`.

  Resolved rather than accepted deliberately. The promise is the one protection
  the design states against the one surface it admits a job can forge; narrowing
  the sentence to fit the code was the cheaper repair and the worse one.
