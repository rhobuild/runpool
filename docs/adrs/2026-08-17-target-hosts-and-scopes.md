# A target is any host the protocol serves, at any scope it defines

**Status:** implemented
**Date:** 2026-08-17

`ParseTargetURL` refuses any host that is not `github.com`, with the
message "V1 supports github.com targets only". One rule closes three
different doors, and they are not equally justified.

## What the provider library serves

The library derives its API URL with **enterprise as the default case**:
the same host, under `/api/v3`. It switches to `api.<host>` only for
hosts it recognises as hosted, which are `github.com`, `www.github.com`,
`github.localhost` and any `*.ghe.com`. It also carries a distinct
enterprise scope, whose runner registration goes to
`/enterprises/<name>/actions/runners/registration-token`.

Measured against that, the single rule refuses:

- **`*.ghe.com`** — GitHub's own hosted service under a data-residency
  hostname. Identical endpoints, identical code path, different host
  string.
- **A self-hosted Enterprise Server** — the same endpoints under
  `/api/v3`.
- **Enterprise scope** — a different registration endpoint that no test
  in this repository exercises.

## The misclassification underneath

The host rule is also masking a defect that exists with the rule in
place. `https://github.com/enterprises/acme` passes the host check, has
two path segments, and both segments satisfy the owner and repository
patterns. It is therefore accepted as **repository `acme` owned by
`enterprises`**, and the canonical URL is rebuilt from a hardcoded
`https://github.com/` prefix.

An enterprise URL is not refused today. It is silently understood as
something else.

## Decision

**The host is not the unit of refusal.** Any `https` host is accepted;
the canonical URL is built from the host that was given rather than from
a literal. A host that does not serve the protocol fails at the live
credential check in `runpool doctor`, which already makes a real call,
with an error about the host rather than a rule about a name.

**Enterprise is a scope**, parsed from `/enterprises/<name>` and carried
through as a third value beside organization and repository. The
lifecycle core keys on opaque provider identity and is unaffected. The
rules that branch on scope are: runner groups, which enterprises have,
so the organization rule extends to them; and cache lanes, which stay
repository-only, because the identity that justifies a persistent lane
is unchanged by this.

**Verification is stated per path, because it differs.** A hermetic test
pins the derivation for each shape — a hosted host resolves to
`api.<host>`, an enterprise host to `<host>/api/v3`, and each URL shape
to its scope. That proves this side of the boundary entirely. What it
does not prove is that a given remote answers, and the support matrix
says which hosts and scopes the gates actually observed: `github.com`,
at organization and repository scope.

## Consequences

- An enterprise URL stops being misread as a repository. That is a
  correctness fix, independent of everything else here.
- A deployment on Enterprise Cloud with data residency, or on Enterprise
  Server, can be configured. Neither has been observed by the gates, and
  the support matrix says so.
- Enterprise scope reaches an endpoint no suite in this repository
  reaches. It is the least exercised path of the three and is documented
  as such, rather than grouped with paths that share the endpoints the
  suites do exercise.
- A typo in a host no longer fails at configuration validation. It fails
  at `runpool doctor`, one step later, against the real service — which
  is where a wrong-but-well-formed host was always going to fail.

## Amendment

**Date:** 2026-08-19

One sentence above is no longer true. "The lifecycle core keys on opaque
provider identity and is unaffected" described the binding key as it was
when this was written. It is now built from the configured target id, the
runner group and the scale set name — the scope and the URL are not in
it, and the target id is.

What that changes for an operator: **renaming a `targets[].id` produces a
different binding, and the old one is forgotten on the same startup.**

The new key matches no row, so a row is written for it. The old row is
then unclaimed, and `ForgetUnclaimedBindings` deletes it along with the
adapter metadata holding the scale set id registered under the old name.
The one exception is a binding that still holds deliveries: forgetting it
would orphan the attempts hanging off them, so it is kept.

The visible consequence is a refusal, not a silent duplicate. The new
binding has no record of the scale set, which still exists at the
provider under the unchanged name, so the first pass refuses to adopt it:

> scale set %q already exists in runner group %q (id %d) and this
> instance has no record of creating it

That pass writes its intention, so the next one adopts and the binding
serves. What does not come back is that binding's contact history, and a
`scaleSetName` rename is worse than a `targets[].id` one: `runpool
uninstall --delete-scale-sets` names the sets to remove from the binding
rows, so the set under the old name is orphaned at the provider and the
only local record of it is gone.

A target id is therefore part of the binding's identity rather than a
label on it, and renaming one is a migration.

The decision this ADR records is unchanged: enterprise is a scope, the
host is not the unit of refusal, and the rules that branch on scope are
still runner groups and cache lanes. Only the claim about what the
lifecycle keys on was wrong.
