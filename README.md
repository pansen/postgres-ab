# Summary

Working with PostgreSQL dumps is painful when `pg_restore` takes ~90 minutes
and your dev database is unreachable for the whole window. This repo drives
Incus containers — natively on Linux, with copy-on-write snapshots (btrfs on
Linux, ZFS under colima on macOS) — to give you:

- **two long-lived Postgres backends** (`pg-dev-a`, `pg-dev-b`) with
  independent snapshot timelines, so already-loaded states stay cheap to
  checkpoint and restore;
- **two stable host ports** (`:5432` active, `:5433` staging) served by a tiny
  `pg-proxy` container, so `pg_restore` can load a new dump on the staging
  backend through one port while your app keeps querying the active backend on
  the other;
- **a single command** (`make pg.promote`) to swap which backend is "active" —
  the host:port endpoints never change; open connections just reconnect onto
  the new dataset.

Design rationale lives in [SPEC.md](SPEC.md). The scenarios below cover the
day-to-day surface.

# Scenarios

## Once, after cloning

```shell
make deps                 # incus + jq, enable the daemon, join incus-admin (Fedora/dnf)
cp .env.example .env      # edit PG_* if you don't like the defaults
make start                # start the incus daemon, init storage/network, open the ports
make pg.up                # provision pg-dev-a, pg-dev-b, pg-proxy (~15 min)
make pg.endpoint          # prints connection info + a ready-to-paste .pgpass line
```

`make deps` adds you to the `incus-admin` group — **log out and back in once**
so the plain `incus` CLI (and every `make pg.*` target) works without `sudo`.

`make pg.endpoint` prints something like, where `<host-ip>` is **this machine's
LAN address** — the Postgres ports are published on the host (all interfaces),
so any other host on the LAN can connect there:

```
active   host=<host-ip> port=5432 dbname=<PG_DB>   (current data)
staging  host=<host-ip> port=5433 dbname=<PG_DB>   (import target / opposite of active)

.pgpass line (one line, covers both ports):
    <host-ip>:*:*:<PG_USER>:<PG_PASSWORD>
```

Paste that single line into `~/.pgpass` and you're done with auth forever.
`make start` publishes ports 5432/5433 on the host (Incus `proxy` devices,
`bind=host` on the `pg-proxy` container) and opens them on firewalld, so they
survive reboots, promotes and snapshot restores. The endpoint auto-picks your
primary NIC's IP; override with `PG_HOST_IP` in `.env`. The backends keep pinned
incusbr0 IPs that the proxy dials (override with `PG_BACKEND_A_IP`/`_B_IP`), but
clients never need them.

> **Security:** the database is now reachable by *anything on your LAN*, guarded
> only by the `$PG_PASSWORD`. That's fine for a trusted home/office network and
> in the spirit of [SPEC.md](SPEC.md)'s "one developer, one laptop" scope — but
> don't run this on an untrusted network. To keep it host-only, skip
> `make firewall` and connect from the host itself.

## Day to day: querying the database

Port **5432** always means *current data*. Pick whatever client you like:

```shell
psql -h <host-ip> -p 5432 -d $PG_DB        # any psql, any app
make pg.psql                                   # quick shell via incus exec
make pg.logs                                   # tail postgres logs
```

Port **5433** is the staging backend — the opposite slot. Useful for
ad-hoc exploration of "the other dataset" or for verifying a dump
mid-import.

There's no connection pooler in the path: each port is a plain TCP passthrough
to the backend, so `CREATE`/`DROP DATABASE`, `LISTEN`/`NOTIFY`, prepared
statements and advisory locks all behave exactly as a direct connection.
(That's why there's no separate "direct" port — the old `:5434` existed only to
dodge pgbouncer's session-pooling `ObjectInUse`, which no longer applies.)

Overview command

```shell
make status
```

## Importing a fresh dump (the headline workflow)

This is what this repo exists for. The import runs on the staging backend
(`:5433`); the active backend keeps serving live queries the entire time.

```shell
# 1. Wipe staging back to its clean `initial` snapshot.
$ make pg.staging.reset
scripts/pg-dev-local staging.reset
==> Snapshots on pg-dev-a that will be deleted:
     2026-06-01T17-20-21_dump_import
Continue? [Y/n]

# 2. Restore the dump through the staging port (:5433).
$ pg_restore --host=<host-ip> --port=5433 --dbname=$PG_DB \
           --jobs=4 your-dump.pgdump          # ~90 min, no blocking

# 3. Sanity-check the loaded data while still on staging.
$ psql -h <host-ip> -p 5433 -d $PG_DB -c '\dt'

# 4. Checkpoint the loaded state on the staging slot.
$ pg.staging.snapshot name=$(date +%Y-%m-%dT%H-%M-%S)_dump_import

# 5. Promote. Sub-second; the :5432 endpoint is unchanged, but open
#    connections are dropped so clients reconnect onto the freshly imported
#    dataset (the proxy re-points :5432 to the new active backend).
$ make pg.promote
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
make status           # versions, slot/proxy roles, per-backend table, snapshots
make pg.endpoint      # both ports with their roles + .pgpass line
make pg.logs          # tail the active backend's postgres log
```

## Tearing down

```shell
make pg.down          # delete pg-dev-a, pg-dev-b, pg-proxy (irreversible)
make stop             # stop the three containers, leave the incus daemon up
make delete           # same as pg.down — destroy the containers + their snapshots
make recreate         # delete, then start (re-provision with `make pg.up`)
```

Unlike the colima setup there's no VM to throw away, so `make delete` just
removes the containers (and their snapshots) — the incus daemon and any other
workloads on it are untouched. `make pg.export` / `make pg.import-last`
serialise the *active* backend (data + snapshots) to a tarball under `var/`,
which survives a teardown — slower than a snapshot, but the way to move a
backend between machines.

# Troubleshooting (native Incus on Linux)

Most of the Linux-specific setup is automated by `make deps` / `make start`,
but three things bite on a fresh host and are worth knowing:

- **"You don't have the needed permissions to talk to the incus daemon."**
  `make deps` adds you to the `incus-admin` group, but existing login shells
  don't pick that up until you get a fresh session — and a GUI re-login often
  isn't enough (the display manager keeps the session alive). `make start` and
  every `make pg.*` work around this by re-running under `sg incus-admin`, so
  they *just work* with no logout. The catch: commands running under `sg` can
  swallow Ctrl-C. For the cleanest experience, **reboot once** after `make deps`
  — then the group is active everywhere, the `sg` layer is skipped entirely, and
  long commands are interruptible again.

- **`make pg.up` says "Waiting for IPv4" / a container has only an IPv6 address.**
  firewalld's default zone drops DHCP on the incus bridge. `make start` fixes
  this by putting `incusbr0` in the `trusted` zone (`make firewall` on its own
  does the same). The provisioner now also fails fast with this hint instead of
  hanging forever.

- **"System doesn't have a functional idmap setup" when a container launches.**
  incusd runs as root and needs a subordinate UID/GID range, which Fedora omits.
  `make deps` adds `root:1000000:1000000000` to `/etc/subuid` and `/etc/subgid`
  and restarts incus.

# Questions

## Why a `Makefile` if you have a script

Because I like the shell autocompletion of `make`.

## Where's the design rationale

[SPEC.md](SPEC.md).
