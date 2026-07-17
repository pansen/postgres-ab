# Summary

Working with PostgreSQL dumps is painful when `pg_restore` takes ~90 minutes
and your dev database is unreachable for the whole window. This repo wraps
a colima VM running incus + ZFS to give you:

- **two long-lived Postgres backends** (`pg-dev-a`, `pg-dev-b`) with
  independent ZFS snapshot timelines, so already-loaded states stay cheap to
  checkpoint and restore;
- **a tiny `pg-proxy` container** with two stable ports, so `pg_restore` can
  load a new dump on the staging backend through one port while your app
  keeps querying the active backend on the other;
- **a single command** (`make pg.promote`) to atomically swap which backend
  is "active" — clients keep their TCP connection, the dataset underneath
  changes.

See Design below for the container layout and the rationale behind it; the
scenarios below that cover the day-to-day surface.

# Design

This exists for two things, both squarely in the local-development loop:
making ~90-minute `pg_restore` imports non-blocking (a fresh dump loads on
one backend while the other keeps serving live queries), and preserving
snapshot/restore for testing migrations against an already-imported state,
same as a single-backend setup would. Switching between "the database I was
using" and "the freshly imported one" is a single command; the worst case is
a reconnect.

Explicitly **not** in scope: failover, replication, zero-downtime SLAs,
protection against a malicious actor on the host (the colima VM is a trusted
boundary), or multi-user concerns — one developer, one laptop. Anything that
adds friction in the name of that kind of robustness — extra auth steps,
credential rotation, certificate management — is out of scope.

## Containers

```
                ┌────────────────────────────────────────────────┐
client (app) ──►│ pg-proxy  (stable IP 10.x.x.10)                │
pg_restore  ──►│  bare container — no software installed         │
                │                                                │
                │  incus proxy devices (bind=instance):          │
                │    :5432 ─ active   ─┐                          │
                │    :5433 ─ staging  ─┼─ per-connection TCP relay │
                │                       │  straight to the backend │
                └──────────────┬───────┴────────────────────────┘
                               │
                  ┌────────────┴────────────┐
                  │ active  → active backend │
                  │ staging → staging backend│
                  ▼                         ▼
              pg-dev-a                  pg-dev-b
              (Postgres 17,             (Postgres 17,
               own snapshots,            own snapshots,
               own data)                 own data)
```

- `pg-proxy` — one bare container, fixed identity, lives for the life of the
  colima VM. It has no software installed at all: it exists only to host the
  two `proxy` devices, whose forkproxies run inside its network namespace and
  dial the backends' pinned incusbr0 IPs directly. `bind=instance` makes each
  forkproxy listen on pg-proxy's own pinned IP (not the colima VM's host
  address) — that pinned IP is the one address clients ever need.
- `pg-dev-a` / `pg-dev-b` — symmetric, independently-installed Postgres 17
  backends, both long-lived and always running. One is active, one is
  staging; which is which flips on `make pg.promote`, and a container never
  changes which role's data it holds until it's promoted.

There's no third "transient" container during import — staging is always
there, ready for the next dump.

There is no connection pooler in the path: each port is a plain per-connection
TCP passthrough, indistinguishable from a direct connection. `CREATE`/`DROP
DATABASE`, `LISTEN`/`NOTIFY`, prepared statements and advisory locks all
behave exactly as they would against Postgres directly — no pgbouncer
`ObjectInUse` surprises on `DROP DATABASE`, no userlist/SCRAM file to keep in
sync, no PAUSE/RESUME drain to orchestrate on promote. `promote` is just:
flip `var/active-slot`, then re-point each proxy device's `connect` target —
which restarts its forkproxy, so any open TCP connection drops and the client
reconnects onto the new backend. The host:port endpoints themselves never
change.

### Migrating from the old pgbouncer layout

Earlier versions of this repo ran a `pg-bouncer` container with direct proxy
ports *and* pgbouncer session-pool ports (`:5442`/`:5443`). That's gone: the
pooled ports are removed, and the container is renamed to the bare `pg-proxy`
described above. The migration is automatic and one-time — the first `make
pg.refresh` (which `make start` also runs whenever a proxy container is found
RUNNING) or `make pg.up` detects a leftover `pg-bouncer` container, deletes
it, and provisions `pg-proxy` at the same pinned IP, so the client-facing
endpoint doesn't change.

## Snapshots

Snapshots are per backend container and never merge or copy between slots.
Each backend gets an `initial` snapshot once, at provisioning time (clean
role + database, no user data) — that's the target of `make pg.staging.reset`
before every import. Everything after that is whatever you name it. A
promote never touches snapshots; the previously-active slot keeps its full
timeline as the rollback path.

## Authentication

One Postgres role (`$PG_USER`/`$PG_PASSWORD` from `.env`) covers application
access, `pg_restore`, and ad-hoc psql through every port — one `.pgpass` line
covers everything, forever. This is a deliberate dev-convenience tradeoff, not
a privilege-separation model.

# Scenarios

## Once, after cloning

```shell
make deps                 # incus, colima, jq on macOS
cp .env.example .env      # edit PG_* if you don't like the defaults
make start                # boot colima with the incus runtime
make pg.up                # provision pg-dev-a, pg-dev-b, pg-proxy (~15 min)
make pg.status            # prints connection info + a ready-to-paste .pgpass line
```

```
active   host=<proxy-ip> port=5432 dbname=<PG_DB>   (current data)
staging  host=<proxy-ip> port=5433 dbname=<PG_DB>   (import target / opposite of active)

.pgpass line (one line, covers both ports):
    <proxy-ip>:*:*:<PG_USER>:<PG_PASSWORD>
```

Paste that single line into `~/.pgpass` and you're done with auth forever.
The proxy IP is pinned at the device level — it survives reboots, promotes,
snapshot restores, everything short of `make pg.down`. Override the auto-pick
by setting `PG_PROXY_IP` in `.env` (the legacy `PG_BOUNCER_IP` name still
works too).

## Day to day: querying the database

Port **5432** always means *current data*. It's a pooler-free direct line to
the active backend (an incus `proxy` device on pg-proxy) — so it follows
promote but carries no session stickiness, and test suites that
`CREATE`/`DROP` databases work here. Pick whatever client you like:

```shell
psql -h <proxy-ip> -p 5432 -d $PG_DB        # any psql, any app
make pg.psql                                   # quick shell via incus exec
make pg.logs                                   # tail postgres logs
```

Port **5433** is the staging backend — the opposite slot, also a direct proxy.
Useful for ad-hoc exploration of "the other dataset", for verifying a dump
mid-import, and as a fast `pg_restore` target (no single-threaded pooler in the
COPY path).

Overview command

```shell
make pg.ip pg.status pg.snapshots
```

## Importing a fresh dump (the headline workflow)

This is what this repo exists for. The import runs on the staging backend
(:5433, the pooler-free direct proxy); the active backend keeps serving live
queries the entire time.

```shell
# 1. Wipe staging back to its clean `initial` snapshot.
$ make pg.staging.reset
scripts/pg-dev-local staging.reset
==> Snapshots on pg-dev-a that will be deleted:
     2026-06-01T17-20-21_dump_import
Continue? [Y/n]

# 2. Restore the dump through the staging port (:5433, pooler-free direct proxy).
$ pg_restore --host=<proxy-ip> --port=5433 --dbname=$PG_DB \
           --jobs=4 your-dump.pgdump          # ~90 min, no blocking

# 3. Sanity-check the loaded data while still on staging.
$ psql -h <proxy-ip> -p 5433 -d $PG_DB -c '\dt'

# 4. Checkpoint the loaded state on the staging slot.
$ pg.staging.snapshot name=$(date +%Y-%m-%dT%H-%M-%S)_dump_import

# 5. Promote. Sub-second; the :5432 endpoint is unchanged, but open
#    connections are dropped so clients reconnect onto the freshly imported
#    dataset (re-pointing a proxy device's `connect` restarts its forkproxy).
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
make pg.status        # pointer + container states + both ports/roles + .pgpass line
make pg.refresh        # re-pin backend IPs + re-assert proxy forwards (also runs the pg-bouncer→pg-proxy migration if needed)
```

## Tearing down

```shell
make pg.down          # delete pg-dev-a, pg-dev-b, pg-proxy (irreversible)
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
