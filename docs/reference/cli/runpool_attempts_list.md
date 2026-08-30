## runpool attempts list

List attempts waiting for a decision

### Synopsis

Lists attempts in deterministic FIFO order. Results are bounded by --limit; pass the returned opaque cursor to continue without offset scans.

JSON output is an object with state, attempts, total and, when more rows exist, next_cursor.

```
runpool attempts list [flags]
```

### Options

```
      --cursor string   opaque cursor returned by the previous page
  -h, --help            help for list
      --json            emit the list as JSON
      --limit int       maximum attempts to return (1-1000) (default 50)
      --state string    attempt state to list: manual-review or ready (default "manual-review")
```

### SEE ALSO

* [runpool attempts](runpool_attempts.md)	 - The work held for a person, and how to decide it
