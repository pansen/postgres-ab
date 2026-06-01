# Summary

Working with PostgreSQL dumps is painful when `pg_restore` takes ~90 minutes
and your dev database is unreachable for the whole window. This repo wraps
a colima VM running incus + ZFS to give you:

- **two long-lived Postgres backends** (`pg-dev-a`, `pg-dev-b`) with
  independent ZFS snapshot timelines, so already-loaded states stay cheap to
  checkpoint and restore;
- **one pgbouncer container** with two listeners on a stable IP, so
  `pg_restore` can load a new dump on the staging backend through one port
  while your app keeps querying the active backend on the other;
- **a single command** (`make pg.promote`) to atomically swap which backend
  is "active" — clients keep their TCP connection, the dataset underneath
  changes.

Design rationale lives in [SPEC.md](SPEC.md). The scenarios below cover the
day-to-day surface.

# Scenarios

## Once, after cloning

```shell
make deps                 # incus, colima, jq on macOS
cp .env.example .env      # edit PG_* if you don't like the defaults
make start                # boot colima with the incus runtime
make pg.up                # provision pg-dev-a, pg-dev-b, pg-bouncer (~15 min)
make pg.endpoint          # prints connection info + a ready-to-paste .pgpass line
```

`make pg.endpoint` prints something like (with `<bouncer-ip>` being .10 in
whatever subnet `incus network get incusbr0 ipv4.address` reports — e.g.
`192.168.100.10` if your bridge is on `192.168.100.0/24`):

```
active   host=<bouncer-ip> port=5432 dbname=<PG_DB>   (current data)
staging  host=<bouncer-ip> port=5433 dbname=<PG_DB>   (import target / opposite of active)

.pgpass line (one line, covers both ports):
    <bouncer-ip>:*:*:<PG_USER>:<PG_PASSWORD>
```

Paste that single line into `~/.pgpass` and you're done with auth forever.
The bouncer IP is pinned at the device level — it survives reboots,
promotes, snapshot restores, everything short of `make pg.down`. Override
the auto-pick by setting `PG_BOUNCER_IP` in `.env`.

## Day to day: querying the database

Port **5432** always means *current data*. Pick whatever client you like:

```shell
psql -h <bouncer-ip> -p 5432 -d $PG_DB        # any psql, any app
make pg.psql                                   # quick shell via incus exec
make pg.logs                                   # tail postgres logs
```

Port **5433** is the staging backend — the opposite slot. Useful for
ad-hoc exploration of "the other dataset" or for verifying a dump
mid-import.

## Importing a fresh dump (the headline workflow)

This is what this repo exists for. The import runs on staging through the
bouncer; the active backend keeps serving live queries the entire time.

```shell
# 1. Wipe staging back to its clean `initial` snapshot.
make pg.staging.reset

# 2. Restore the dump through the bouncer's staging port (:5433).
pg_restore --host=<bouncer-ip> --port=5433 --dbname=$PG_DB \
           --jobs=4 your-dump.pgdump          # ~90 min, no blocking

# 3. Sanity-check the loaded data while still on staging.
psql -h <bouncer-ip> -p 5433 -d $PG_DB -c '\dt'

# 4. Checkpoint the loaded state on the staging slot.
make pg.staging.snapshot name=initial-loaded

# 5. Promote. Sub-second; clients keep their TCP connections through the
#    bouncer; the dataset underneath flips.
make pg.promote
```

After `pg.promote`, apps on :5432 see the freshly imported data. The
previously active backend is now reachable on :5433 with its full snapshot
timeline intact, ready to be rolled back to or wiped for the next import.

## Rolling back a bad import

You promoted, ran the app, and the new data is broken. The previous backend
is untouched on :5433. One command undoes the promote:

```shell
make pg.promote          # flips back — :5432 now points at the old data again
```

No data is regenerated. The pointer just inverts.

## Snapshotting / restoring during normal dev

Each backend has its own snapshot timeline. The unprefixed commands act on
whichever slot is currently active:

```shell
make pg.snapshot name=$(date +%Y-%m-%dT%H-%M-%S)_before-migration
# ... run a destructive migration ...
make pg.restore name=$(date +%Y-%m-%dT%H-%M-%S)_before-migration
make pg.restore-last                  # most recent snapshot, no confirmation
make pg.snapshots                     # list
```

The `pg.staging.*` family mirrors these for the staging slot, if you want
to stage multiple checkpoints before a promote.

## Inspecting state

```shell
make pg.status        # active slot + container states
make pg.endpoint      # both ports with their roles + .pgpass line
make pg.bouncer.logs  # tail both pgbouncer instances
```

## Tearing down

```shell
make pg.down          # delete pg-dev-a, pg-dev-b, pg-bouncer (irreversible)
make stop             # stop colima
make delete           # nuke colima entirely; rebuild fresh with `make recreate`
```

Snapshots live inside the colima VM. `make delete` loses them all.
`make pg.export` / `make pg.import-last` serialise the *active* backend
(data + snapshots) to a tarball under `var/`, which survives a colima
rebuild — slower than ZFS snapshots, but the only way out of the VM.

# Questions

## Why a `Makefile` if you have a script

Because I like the shell autocompletion of `make`.

## Where's the design rationale

[SPEC.md](SPEC.md).
