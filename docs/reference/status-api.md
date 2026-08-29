# Status API reference

`runpool status --json` emits a reporting document, not the database.
Persistence rows change with migrations; this shape changes only with
its `api_version`, so something reading it can be written once.

**Current version: `v1`.** The product version and this document version move
independently; consumers must reject unknown versions rather than interpreting
them under the wrong schema.

## Rules the document keeps

- **`snake_case` throughout.** Persistence field names never appear; a
  test asserts they have not leaked in.
- **Collections are always arrays.** An empty one is `[]`, never
  `null`, so a consumer branches on length rather than presence.
- **`null` means absent, not empty.** `disk_pressure` is `null` before
  the monitor has run once; `egress_sandbox` is `null` before the first
  rediscovery pass and for the whole life of an instance that maintains
  no policy; `discrepancies` is `null` when the daemon could not be
  reached, which is different from finding none.

## The two forms, and their discriminator

`served` is a boolean in every v1 document, and it is what a consumer
branches on — never on which fields happen to exist.

**`served: false`** is the whole document before the controller's first
serve: no state directory exists yet, so there is nothing to report and
nothing is invented. It carries exactly `api_version`, `served`,
`state_dir` and `detail`.

```json
{"api_version": "v1", "served": false,
 "state_dir": "/var/lib/runpool/state",
 "detail": "this instance has not run yet"}
```

**`served: true`** carries the full shape below.

This document is `status`'s alone. `attempts list` answers a different
question — what is held for review, what is ready — and on an instance
that has never run its answer is an empty array in its own shape, not
this document; `attempts inspect` of any id fails then, naming the
absent state, because the attempt it was asked about cannot exist.

## Shape

| Field | Type | Meaning |
| --- | --- | --- |
| `api_version` | string | `v1` |
| `served` | boolean | The discriminator above; `true` in this form |
| `instance` | string | This instance's opaque id |
| `host_topology` | string | Effective `shared-daemon` or `dedicated-daemon`; `unknown` only when status cannot read configuration |
| `schema_version` | number | The state schema in use |
| `scheduling` | object, optional | Present when status can read configuration: `mode`, `instance_parallelism`, `effective_parallelism`, `active`, `available`, `queued`, and `tiers[]` |
| `disk_pressure` | object or null | `level`, `free_bytes`, `free_inodes`, `managed_bytes`, `measured_at` |
| `egress_sandbox` | object or null | `last_pass_at`, and `error` when the last rediscovery failed. A non-empty `error` means every gateway on this host is closed to all egress; a `last_pass_at` that has stopped moving means the pass itself has stopped running |
| `bindings` | array | `target_id`, `provider_kind`, `source_binding_key`, and the provider reach fields below |
| `leases` | array | `id`, `state`, `terminal`, `attempt_id`, `project`, `runtime_name`, `evidence`, `created_at`, `resources[]`. Every live lease, plus recent finished ones — see below |
| `leases[].resources` | array | What the lease owns on the daemon: `kind`, `role`, `name`, `lease_id`, and `state` — one of `planned`, `creating`, `present`, `cleanup_pending`, `deleting`, which is the books' account of the object rather than the daemon's |
| `released_total` | number | How many finished leases the store holds, which is more than the `leases` array carries |
| `cache_lanes` | array | `id`, `source_project_key`, `generation`, `leased_by`, `last_used` |
| `manual_review` | array | Attempts held for a person: `id`, `workload`, `project`, `state`, `review_reason`, `age_seconds`, and once resolved `resolution` and `reviewed_by` |
| `containers` | array | Owned containers: `name`, `role`, `lease_id`, `running` |
| `networks`, `volumes` | array | Owned objects: `kind`, `role`, `name`, `lease_id`. No `state` — it is recorded per lease, and these are reported from the daemon |
| `discrepancies` | array or null | Where the books and the daemon disagree; `null` if the daemon could not be asked |
| `engine_error` | string, optional | Why the daemon could not be asked |
| `capsule_image_error` | string, optional | Why the shipped capsule image could not be resolved. It concerns only the tiers that name no `capsule_image` of their own: those report what the build ships rather than what a launch would run, while a tier naming its own image reports that one |

Each binding reports what its own loop last managed: `last_contact_at`
when a provider call last succeeded, and `last_error` with
`last_error_at` for what the loop cannot do now. A success clears the
error, and a failure leaves the last success alone, so the pair says both
how long a binding has been failing and what at. All three are absent on
a binding that has not served yet.

`last_error` is not only about the provider. A loop that reaches its
provider perfectly well and cannot persist what it is handed is a loop
that turns nothing into work, and it records the failure here for the
same reason: the poll that carried the message refreshed
`last_contact_at`, so without this the binding would report as healthy
forever while nothing it was offered ever became an attempt. Read the
error text for which it is.

They are reported because nothing else in this document distinguishes an
instance with no work to do from one reaching nothing: both hold no
leases, and both answer every other field identically. A binding whose
`last_error_at` is at or after its `last_contact_at` is not serving,
however healthy the rest of the document looks — the two are recorded by
the same loop pass and a tie means the failure came last.

`scheduling.queued` is how many attempts wait for admission across every
binding. An attempt waiting for admission holds no lease, so a queue that
stopped draining is invisible in `leases`.

`scheduling.mode` is `global` when `scheduling.parallelism` is configured and
`independent-tiers` otherwise. `instance_parallelism` is the configured global
value or `null`; `effective_parallelism` is the global value or the sum of tier
limits. Each tier reports `id`, `parallelism`, `active`, currently
admissible `available` capacity, and the `capsule_image` its jobs run in —
the image the tier names, or the one this build ships. All unreleased leases count as active,
including cleanup, failed, and quarantined leases.

`leases` carries **every unreleased lease**, without exception — that set is
what the instance is still responsible for, and a consumer may rely on it being
complete. Released leases are **recent history, not all of it**: the array
carries at most the **50** that finished most recently, so the document stays
bounded by live work rather than by every job the host has ever run. A
consumer may rely on that ceiling — it is the reason `released_total` exists.

Both halves are ordered oldest first, but by different clocks, because they
answer different questions. Unreleased leases are ordered by `created_at`: they
are still running, so when each began is what ranks them. Released leases are
ordered by `updated_at`, which for a finished lease is when it finished — a
lease that was held for weeks and released a minute ago is recent history, and
ranking it by its start would put it outside the window the array carries.

`released_total` is how many finished leases the store holds. A consumer that
needs that figure must read it there rather than measure the `leases` array,
whose length is what the document reports and not what exists.

## Vocabulary

The document is provider neutral, and deliberately so: a consumer that
parses `source_binding_key` is reading the wrong document. It is the
binding's own identity, versioned and opaque here; for a GitHub Actions
binding it is built from the configured target id, the runner group and
the scale set name, which is enough to tell two bindings apart without
knowing what any of it means.

One consequence an operator needs, because it is not visible from the
key: renaming a `targets[].id` in configuration produces a different
binding, and the old one is forgotten on the same startup — with the
scale set id recorded against it. The renamed binding then meets a
refusal on its first pass, adopts on the next, and does not get its
contact history back. It is a rename of the binding, not of a label on
it; `docs/adrs/2026-08-17-target-hosts-and-scopes.md` has the whole of
it.

Lease states are `reserved`, `provisioning`, `runtime_registered`,
`workload_running`, `draining`, `cleaning`, `released`, `failed`,
`quarantined`. They describe the host resources an attempt consumes,
never what the workload did — `evidence` is the attempt's, joined in
here because a report is where the two belong side by side.

## Discrepancies

The comparison covers containers, networks **and** volumes. Judged from
containers alone — as it once was — a leaked network was invisible to
the one command that claims to answer whether the books agree.

Instance infrastructure is recognised rather than flagged: the uplink
network and cache lanes carry no lease on purpose, and reporting them
as orphans would bury the real findings. What is reported:

- a container, network or volume belonging to no live lease;
- a lease claiming to be running with no container;
- an owned object carrying neither a lease nor a persistent role.

An unreachable daemon yields `discrepancies: null` and a
`engine_error`, never an empty list — a report that compared nothing
must not look like a clean one.
