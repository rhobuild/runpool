## runpool uninstall

Remove everything this instance owns

### Synopsis

Removes every container, network and volume this instance owns,
including cache lanes. The state volume is left for you to remove.
Without --confirm it is a dry run and prints the exact command.

```
runpool uninstall [flags]
```

### Options

```
      --confirm string      the instance id being uninstalled
      --delete-scale-sets   also delete this instance's scale sets from the provider
  -h, --help                help for uninstall
```

### SEE ALSO

* [runpool](runpool.md)	 - Docker-native autoscaling for ephemeral GitHub Actions runners
