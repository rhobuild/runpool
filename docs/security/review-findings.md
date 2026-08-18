# Security review findings

[Release readiness](../release-readiness.md) holds the release authorization
gate closed until every finding affecting the release boundary is resolved or
explicitly accepted. This is where that is recorded.

[Review scope](review-scope.md) defines the boundary and the questions a review
answers. This file records what came back.

## State

No external review has been conducted. The register is empty, and the release
authorization gate is closed on that basis rather than on an absence of
findings.

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

None recorded.
