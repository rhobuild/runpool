---
name: Bug report
about: Something behaves differently from what the documentation says
labels: bug
---

## What happened

<!-- What you observed, and what you expected instead. -->

## How to reproduce

<!-- The smallest sequence that shows it. -->

## Host and version

<!--
`runpool version`, and the relevant part of `runpool doctor` — it
reports the engine, cgroup and capacity facts most reports turn out to
depend on.
-->

```text
runpool doctor output
```

## State

<!--
`runpool status` if the controller has run. Redact private repository
URLs; the lease and attempt ids are what matter.
-->

```text
runpool status output
```

> Do not report a security issue here. See SECURITY.md.
