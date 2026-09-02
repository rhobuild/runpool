# Credential file readability is deployment policy

**Status:** implemented
**Date:** 2026-09-01
**Amends:** [GitHub App credentials](2026-08-17-github-app-credentials.md) — the unconditional owner-only file-mode refusal

Runpool refused every token or GitHub App key file carrying any group or
other permission bit. That is the right default when the operator makes the
file, but a deployment does not always control the mode or uid of a mounted
file. A control plane may create it under its own umask, a secret mechanism may
materialize a read-only `0444` file, and a filesystem without POSIX modes
reports whatever its mount options say. Recreating or rotating the mount can
also restore those properties after a one-time host-side change.

An external `chmod 600` still satisfies the original rule, but it is not a
durable Runpool decision: it lives outside effective configuration and may be
lost when the source is recreated. Accepting a wider file silently would be
worse, because an operator reading the configuration would still believe the
owner-only defence holds. The controller cannot infer the right exception on
its own either: whether a group or other read bit reaches another account is a
deployment fact only the operator can accept.

## Decision

Every file-backed credential carries a `filePermissions` policy, one rung of
a ladder ordered narrowest first. Each rung accepts strictly more than the
one before across POSIX mode and ownership, and the last ignores both:

| Policy | Refuses | Ownership rule |
| --- | --- | --- |
| `owner-only` | every group or other bit | no |
| `allow-group-read` | group write or execute, and every other bit | controller owner when group/other bits are present |
| `allow-world-read` | group or other write or execute | controller owner when group/other bits are present |
| `ignore-mode-and-owner` | nothing based on POSIX mode or uid | not checked |

`owner-only` is defaulted into the effective configuration of every
file-backed credential and preserves the original rule. The two middle rungs
require controller ownership only when group or other bits are actually
present, because a Unix owner can change those bits after the check. A `0600`
file relies on no widening, so every rung accepts it regardless of whether a
privileged controller or the deployment operator owns it.
`ignore-mode-and-owner` consults neither, for filesystems where neither carries
useful information and for the operator who explicitly delegates those
decisions to the deployment environment; its warning says so.

The ladder is a private table in `internal/config`: name, refused bits,
ownership rule and warning. The validator, credential reader and doctor obtain
policy values from it without exposing mutable shared state. A refusal names
the narrowest rung that would accept the file, so widening is a choice of the
exact word. A new rung is a row, a test case and a reference row; no existing
rung changes meaning. The policy is invalid on an environment-backed
credential because no file mode exists to govern.

Mode policy never disables structural input checks. The opened descriptor must
be a regular file and its content is capped at 1 MiB under every rung,
including `ignore-mode-and-owner`. Opening once, inspecting that descriptor
and reading from it also prevents a path replacement between validation and
use.

The durable decision is the configured policy, not the mode observed at one
instant. `config effective` prints it, and `doctor` warns on every rung above
the default; the controller runs the same check at startup, so its log
carries the warning even if the file happens to be `0600` at that moment and
is recreated `0644` on a later deployment.

## Consequences

- A platform deployment can encode the permission policy it needs and no
  longer depends on an external `chmod` job. The mounted credential must still
  exist and satisfy the selected policy plus the regular-file and size checks.
- The secure default and every existing configuration keep their behavior.
- A wider rung acknowledges a real disclosure. It does not make a `0644`
  credential a secret from other local accounts, and documentation must never
  describe it that way.
- Permission policy is scoped to one credential. A deployment-managed token
  does not relax a separately provisioned GitHub App private key.
- Operator freedom over mode and ownership does not turn a FIFO, device or
  unbounded stream into a credential source.
- Policies are named. Raw numeric modes or a generic `insecure: true` would
  permit more than the operator can see from the word; each of the four names
  states one thing another account can do.
