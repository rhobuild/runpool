Fixes #

## What changes

<!--
The observable behaviour that differs after this merges. Two or three
sentences: the commits carry the detail, and this is the reviewer's map
of what to expect across them.
-->

## What was found

<!--
The finding, not the fix. What made the old behaviour wrong, what would
have followed from it, and how you know: a measurement, an upstream
report, a log, a run.

Where a constant was chosen — a retry count, a timeout, a threshold —
say what decided it. Where something nearby looks like it should have
changed and did not, say why it did not.
-->

## Evidence

<!--
What you observed, not the command you typed.

Most of this repository's CI is strong enough that a committed test is
better evidence than pasted output. One surface is not: the provider
contracts never run on a pull request, because they reach protected
fixtures. For anything under internal/githubactions this
section is the only record of the observation that will ever exist.

Link the workflow run and its evidence artifact where one was produced.
Say what skipped — a gated suite with its environment unset passes by
running nothing. Redact fixture repository identifiers.

"Hermetic only" is a complete answer when it is the honest one; say why.
-->

```text
observed output, or: hermetic only — <reason>
```

## What would notice this regressing

<!--
Name the test, contract or gate that fails if this behaviour returns —
the way a closed security finding names one, and for the same reason:
without it, nothing stops the behaviour coming back. A bug fix names a
test that fails without its fix.

If nothing would notice, say so. That is a reviewable answer, not a
failing one.
-->

## Risk, compatibility and operations

<!--
Trust boundary, credentials, ownership, cleanup, networking, resource
limits, failure behaviour. Then CLI, configuration, status API, schema,
migration, deployment, rollback and runbook effects.

Give the detail backing the assessment, not the assessment alone. N/A
with its reason, never bare.

If this constrains what the code may do later it needs an ADR, or it
amends one: link it. If it looks like a decision and is not, say why not.
-->

## Changelog

<!--
The entry for CHANGELOG.md, in this repository's voice: grouped by the
public or operational effect, not by the commit. Nothing automated
checks this, which is why it is a field and not a checkbox.

Write NONE for a change with no user-visible or operational effect.
-->

```text
NONE
```

## Notes for your reviewer

<!--
Optional. What you are unsure about, what you would look at first, and
what you deliberately left alone.
-->
