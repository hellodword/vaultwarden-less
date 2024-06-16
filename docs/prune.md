## prune

I only backup the sql text file, so no need to prune with restic:

```sh
restic snapshots --cache-dir=/path/to/cache

restic forget --cache-dir=/path/to/cache --prune --verbose=2 --keep-<...>=<...>
restic forget --cache-dir=/path/to/cache --prune --verbose=2 <snapshot ID>
```
