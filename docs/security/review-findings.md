# Security review findings

[Release readiness](../release-readiness.md) holds the release authorization
gate closed until every finding affecting the release boundary is resolved or
explicitly accepted. This is where that is recorded.

[Review scope](review-scope.md) defines the boundary and the questions a review
answers. This file records what came back.

## State

Two reviews have been conducted.

The first was external, against `c8ac420`. It reported no finding that blocks
the release boundary, and one finding recorded below. That finding was
resolved after the review ended, and the resolution is held by the regression
tests its entry names rather than re-confirmed by the reviewer.

The second was internal, after `v1.1.0`, against the tree at `88244d4`. It
covered each boundary surface, the build and release chain, and what reaches an
operator-readable surface. Four findings are recorded below; each is resolved
with the test that holds it or accepted with the reasoning.

A review covers the commit it read: a later tree is covered only to the extent
it has not moved. The current tree postdates both reviews and changes admission,
durable identity and qualification. Neither earlier review authorizes its next
release; the release gate requires an external review of the final candidate
commit.

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

### RP-SEC-002 — a quoted log was bounded in lines, not in bytes

- **Reported** — 2026-08-28, internal review of the capsule envelope at `88244d4`.
- **Surface** — the capsule envelope.
- **Claim** — a capsule that cannot start has its last output quoted into the
  controller's log. The daemon's tail bounds newline-delimited records and not
  bytes, so a capsule writing one unterminated line makes "the last five lines"
  the whole log it has written, read into the controller's memory and folded
  into one log record.
- **Assessment** — reproduces. Only an operator's own derived capsule image can
  reach it, and that operator is trusted by the threat model, so the reviewer
  rated it low and not release-blocking. It compounds with RP-SEC-003.
- **Disposition** — `resolved`. The quotation is capped at two kilobytes, cut
  from the end because the last thing a dying process said is the reason it
  died. `TestACapsuleThatSaidTooMuchIsCutOff` fails without the cap.

### RP-SEC-003 — the pre-start gate was the call graph and nothing else

- **Reported** — 2026-08-28, internal review of the capsule envelope at `88244d4`.
- **Surface** — the capsule envelope.
- **Claim** — reading a container's log is safe only because every path that
  does it runs before the runner is authorized to fork, so nothing a job wrote
  can be in it. That is true of the call graph and of nothing else: no type
  distinguishes a prepared runtime from a started one, and the architecture
  suite checks which packages may import which — it has no vocabulary for which
  method may be called when, and structurally cannot. A diagnostic added later
  to a path that runs after the start would compile and pass every test.
- **Assessment** — the invariant holds as shipped; the reviewer traced every
  path and found none that breaks it. What was missing was anything that would
  notice it breaking.
- **Disposition** — `resolved`. `TestInspectingAStartedCapsuleReadsNoLogs` fails
  if the path that observes a capsule which has run reads its log.
  `docs/security/threat-model.md` records the exposure that remains: an
  operator's own image writing into the operator's own log.

### RP-SEC-004 — an assignment refusal names a private repository

- **Reported** — 2026-08-28, internal review of data and credential flow at `88244d4`.
- **Surface** — admission, reaching an operator-readable surface.
- **Claim** — the refusal of an assignment with no workload key names its owner
  and repository. That error is now recorded against the binding and read back
  by `runpool status`, and a binding may serve a whole organization, so a
  private repository name from elsewhere in that organization can be persisted
  and shown.
- **Assessment** — reproduces, and reaches only a reader who already holds the
  state directory, which is host-root equivalent. The same reader already sees
  the same owner and repository as `leases[].project` for every workload
  admitted normally, so this extends an accepted disclosure to one rare
  additional path rather than opening a new one. Rated low.
- **Disposition** — `accepted`. Removing the names was tried and reverted: a
  GitHub run id is unique within a repository and not across one, so without
  the pair the error identifies nothing an operator can go and look at.
  `TestAnAssignmentWithoutAWorkloadKeyIsRefused` states that requirement and
  refused the change. Accepted by the maintainer on 2026-08-28. It stops being
  acceptable if `status --json` ever becomes readable by anything less trusted
  than the host's operator, or if `leases[].project` stops carrying the same
  pair — either would remove the reason this is a disclosure already made.

### RP-SEC-005 — a fixture scale set claimed a label real jobs ask for

- **Reported** — 2026-08-28, internal review of data and credential flow at `88244d4`.
- **Surface** — not a product surface: the live contract suite's own fixtures.
- **Claim** — a contract test registers a scale set labelled `self-hosted` in
  the fixture organization's default runner group. It opens no session, so a
  job in that organization dispatched with `runs-on: self-hosted` during the
  window could be assigned to it and wait unserved — which the provider's own
  stranded-grant handling calls unrecoverable from this side.
- **Assessment** — reproduces in principle. The window was the test's remaining
  runtime, and the risk is starvation of an unrelated job rather than exposure
  or execution.
- **Disposition** — `resolved`. The set is deleted the moment the answer it
  exists for is in hand, rather than at the end of the test, which is the
  smallest window that still answers the question. The shared cleanup treats a
  set already gone as success, the same contract every removal in this product
  honours.
