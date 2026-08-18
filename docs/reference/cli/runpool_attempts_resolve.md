## runpool attempts resolve

Decide a held attempt (preview by default)

### Synopsis

Decides work Runpool could not decide alone.

--retry returns the attempt to the queue and it will run. Use it only
after verifying outside Runpool — in the provider's own UI or API —
that the workload never executed.

--settle-may-have-run closes the attempt as possibly executed. It
never runs again. Use it when execution cannot be ruled out.

Without --apply this is a preview.

```
runpool attempts resolve <attempt-id> [flags]
```

### Options

```
      --actor string          who is deciding (default: $USER)
      --apply                 perform the resolution (default is a preview)
  -h, --help                  help for resolve
      --reason string         why this decision is correct (required)
      --retry                 return the attempt to the queue (verify externally that it never executed first)
      --settle-may-have-run   close the attempt as possibly executed; it never runs again
```

### SEE ALSO

* [runpool attempts](runpool_attempts.md)	 - The work held for a person, and how to decide it
