# A resolution reaches the writer

**Status:** accepted and implemented
**Date:** 2026-08-27

The controller listens on a unix socket inside its state directory —
`runpool.sock`, beside the database and the lock — and applies operator
resolutions itself. `runpool attempts resolve --apply` sends the decision
there while a controller is serving, and writes directly under the
singleton lock only when none is.

## What forced it

Every write to this state belongs to whoever holds the lock, and while a
controller runs that is the controller. The resolving command took the
lock too, so it could only run with the controller stopped — which the
runbook told an operator to do. On a shared host that is every tenant's
CI stopped for one attempt, and an attempt held as
`start_outcome_unknown` is not rare: it is the case the lease machine
holds for a person precisely because no automatic answer is safe.

## What was rejected

**A second writer.** SQLite would serialize the transactions, but the
controller opens every transaction as immediate, so a second writer makes
`SQLITE_BUSY` reachable on writes that were built on the premise it could
not be — a failed quarantine transition is swallowed with a log line, and
a lost evidence record is a job that can run twice. `store.Open` also
applies migrations and writes identity, so a newer command opening for
write beside an unrestarted controller would migrate the database under
it. The rule is not "one writer per table": it is that the process
holding the flock is the only writer, which the kernel enforces.

**An intent row the controller polls.** Writing the intent is the write
it cannot do.

**A file and a signal.** A signal carries no payload and no reply, so the
answer becomes a second file polled on an invented timeout, a crashed
command leaves requests to collect, and two operators need a lock the
socket's accept queue gives away. Same reach, more failure modes.

## Why this is not an endpoint

The deployment contract says the controller has no inbound application
endpoint: no domain, no reverse-proxy route, no published port. All of
that stays true. A socket file is none of those, and it adds nothing to
protect: reaching it means holding the state directory, which is the same
reach that already opens the database and takes the lock. In the
reference deployment that directory is a volume mounted only inside the
controller's container, which is where every operator command already
runs.

It is, however, the first thing this controller has ever listened on,
which is why it is recorded here rather than in a commit message.

## What it costs

A unix socket address is a little over a hundred bytes. A state directory
nested deeply enough is refused by the bind and by nothing else, so the
controller warns and serves without it: the offline path still exists,
and losing a listener is not worth refusing to start.

An answer can be lost after the decision travelled. The command says so
rather than guessing, because the alternative an operator reaches for is
to run it again — and a resolution that landed is refused the second
time, not repeated.
