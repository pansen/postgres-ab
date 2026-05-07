# Summary

Working with Postgres database dumps can be painful, if you want to test database data- or schema migrations. Since `pg_restore` can be slow if there are complex indexes like `GIN` indexes.

On Linux there is tooling to solve this problem: lightweight system containers via `incus` (backed by LXC, using kernel namespaces and cgroups) and file system snapshots via ZFS. Both are available on macOS through `colima`, which runs a Linux VM transparently in the background.

This repository provides some scripting to serve a daily loop of creating and restoring snapshots of a Postgres instance.

# Usage

## Once

```shell
make deps
```

## Daily

```shell
# Start colima
make pg.up

# Create a snapshot
make pg.snapshot name=$(date +%Y-%m-%dT%H-%M-%S)_dump_import

# List snapshots
make pg.snapshots

# Restore a named snapshot, drops potential following after confirmation
make pg.restore name=initial

# Restore the most recent snapshot without confirmation
make pg.restore-last

# Tail postgres logs
make pg.logs
```


## Special

Snapshots are bound to one colima instance (`make pg.up`). Destroying the instance will kill all snapshots. You may export and import snapshots, but while faster than `pg_restore` in my case, it still is _not fast_ (Macbook Pro M3).

```shell
make pg.export
time make recreate pg.import-last

# ...
+------+-------+------+------+------+-----------+
| NAME | STATE | IPV4 | IPV6 | TYPE | SNAPSHOTS |
+------+-------+------+------+------+-----------+
incus info --resources | head -n5
System:
  UUID: 4ff85147-1f53-9349-ae7c-adad38143614
  Vendor: Apple Inc.
  Product: Apple Virtualization Generic Platform
  Version: 1
scripts/pg-dev-local import-last
==> Importing from /Users/a/p/pansen/colima-incus/var/pg-dev-2026-04-26T22:41:05.tar.gz...
Imported and started pg-dev

real    0m27.368s
user    0m1.674s
sys     0m2.401s
```

# Questions

## Why a `Makefile` if you have a script

Because I like to have the shell autocompletion of `make`.
