## runpool

Docker-native autoscaling for ephemeral GitHub Actions runners

### Synopsis

Runpool coordinates GitHub Actions scale-set demand against a finite
pool of per-job execution capsules on one Docker host.

```
runpool [flags]
```

### Options

```
  -h, --help   help for runpool
```

### SEE ALSO

* [runpool attempts](runpool_attempts.md)	 - The work held for a person, and how to decide it
* [runpool cleanup](runpool_cleanup.md)	 - Remove resources no live lease needs (dry run by default)
* [runpool config](runpool_config.md)	 - Validate and inspect the configuration
* [runpool doctor](runpool_doctor.md)	 - Check the host, storage and credentials
* [runpool gc](runpool_gc.md)	 - Collect cache lanes and finished lease records (dry run by default)
* [runpool healthcheck](runpool_healthcheck.md)	 - Container health probe
* [runpool serve](runpool_serve.md)	 - Run the controller (configuration from the environment)
* [runpool status](runpool_status.md)	 - What this instance owns, and whether the books agree with the daemon
* [runpool uninstall](runpool_uninstall.md)	 - Remove everything this instance owns
* [runpool version](runpool_version.md)	 - Print the build version
