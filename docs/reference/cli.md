# CLI reference

Generated from the command tree itself — `go generate ./internal/command/...`
— so it cannot drift from what the binary does. Every page below is the
same help text `runpool <command> --help` prints.

| Command | What it does |
| --- | --- |
| [runpool](cli/runpool.md) | The root command and the global picture |
| [version](cli/runpool_version.md) | Build version; `--json` adds commit, build time, toolchain, capsule digest and qualification reference |
| [serve](cli/runpool_serve.md) | Run the controller |
| [doctor](cli/runpool_doctor.md) | Check the host, storage and credentials |
| [healthcheck](cli/runpool_healthcheck.md) | Container liveness and readiness probe |
| [status](cli/runpool_status.md) | What this instance owns, and whether the books agree with the daemon |
| [attempts](cli/runpool_attempts.md) | The work held for a person: [list](cli/runpool_attempts_list.md), [inspect](cli/runpool_attempts_inspect.md), [resolve](cli/runpool_attempts_resolve.md) |
| [gc](cli/runpool_gc.md) | Collect cache lanes and finished lease records |
| [cleanup](cli/runpool_cleanup.md) | Remove resources no live lease needs |
| [uninstall](cli/runpool_uninstall.md) | Remove everything this instance owns |
| [config](cli/runpool_config.md) | [validate](cli/runpool_config_validate.md) and [effective](cli/runpool_config_effective.md) |

## Conventions the whole tree keeps

**Exit codes.** `0` success, `1` an operational failure, `2` a usage
error. A script can branch on the difference: `2` means the invocation
was wrong, `1` means the world was.

**Destructive commands preview by default.** `cleanup`, `gc` and
`uninstall` print what they would do and change nothing until `--apply`
(or, for `uninstall`, `--confirm=<instance-id>`). A wrong instance id
is refused rather than approximated.

**Usage is printed for a parse failure, never for an operational one.**
If `uninstall` fails against a live daemon you get the error, not the
flag list.

**JSON output uses `snake_case` and always emits arrays for collections**
— an empty one is `[]`, never `null`. A command asked for `--json`
answers with a document in every case it can reach, including the one
before this instance has ever run.

`attempts inspect` is the exception, and the reason is that it is asked
about one row: before this instance has run there is no state to hold
one, so it fails naming the absent directory rather than answering with a
document about an attempt that cannot exist.

**One document is a versioned contract**: `status --json` carries an
`api_version` and is specified in [the status API
reference](status-api.md). The other `--json` outputs — `version`,
`doctor`, `attempts list` and `attempts inspect` — are reports rather
than contracts. They carry no version, and a consumer parsing them should
expect fields to be added.

## Paginated attempt listings

`attempts list` returns at most 50 rows by default and accepts `--limit` from 1
through 1000. Ordering is FIFO by the durable `(received_at, attempt_id)` key;
pagination uses that key instead of `OFFSET`, so reading a deep queue does not
scan and discard every earlier row. Pass `--cursor` the opaque value returned by
the preceding page. A cursor is scoped to its `--state` and is rejected if used
against the other queue.

The JSON form is a document rather than a bare array:

```json
{
  "state": "manual-review",
  "attempts": [],
  "total": 0
}
```

`next_cursor` is present only when another page exists. `total` is the complete
queue depth at the time that page was read; the listing is a live view, not a
database snapshot held across CLI invocations.

## Completions

`go generate ./internal/command/...` writes bash, zsh and fish
completions to `dist/completions/`. They are generated, not committed:
a release publishes them, and a working tree regenerates them.
