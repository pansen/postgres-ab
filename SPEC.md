# pg-dev — a/b backends behind pgbouncer

A specification for the next iteration of this repo's Postgres dev wrapper.

## Purpose

This setup exists for **two** things, both squarely in the local-development
loop:

1. **Make 90-minute `pg_restore` imports non-blocking.** While a fresh dump is
   loading, the previously imported database stays available for queries.
2. **Preserve the existing snapshottability** of an already-imported state, so
   schema/data migrations can be tested by snapshot-then-restore as today.

Switching between "the database I was using" and "the freshly imported one"
should be *mostly* seamless — clients keep their endpoint, the change is a
single command, the worst case is a reconnect.

## Non-purpose

This is **not** a production or HA design. Specifically:

- No failover, no replication, no zero-downtime SLA.
- No protection against a malicious actor on the host. The colima VM is a
  trusted boundary.
- No multi-user concerns. One developer, one laptop.

Anything that adds friction in the name of robustness — extra auth steps,
credential rotation rituals, certificate management — is out of scope.

## Architecture

Three containers inside the colima VM. The bouncer container runs **two**
pgbouncer processes side-by-side, one per client-facing port:

```
                ┌──────────────────────────────────────┐
client (app) ──►│ pg-bouncer  (stable IP 10.x.x.10)    │
                │                                      │
pg_restore  ──►│  :5432 ── active   ini  ─┐            │
                │  :5433 ── staging  ini  ─┼─ cross    │
                │  (admin is the special   │  connect  │
                │   `pgbouncer` db on same │           │
                │   port, pgb_admin user)  │           │
                └──────────────┬───────────┴───────────┘
                               │
                  ┌────────────┴────────────┐
                  │ :5432 → active backend  │
                  │ :5433 → staging backend │
                  ▼                         ▼
              pg-dev-a                  pg-dev-b
              (Postgres 17,             (Postgres 17,
               own snapshots,            own snapshots,
               own data)                 own data)
```

- `pg-bouncer` — single container, fixed identity, lives for the life of the
  colima VM. Hosts two pgbouncer instances:
  - **active**  — `listen_port=5432`, `[databases] $PG_DB` → whichever
    backend is currently active.
  - **staging** — `listen_port=5433`, `[databases] $PG_DB` → the *other*
    backend.

  pgbouncer has no separate admin port: admin commands are issued by
  connecting to the special `pgbouncer` virtual database on the same
  `listen_port` as the `pgb_admin` user.
  Both `.ini` files are rendered from the single state file `var/active-slot`,
  so they are always cross-connected and cannot drift.
- `pg-dev-a`, `pg-dev-b` — symmetric backends, both long-lived, both running.
  At any moment one is **active** (served on :5432) and the other is
  **staging** (served on :5433).

Client-facing semantics are stable forever; the physical slot underneath
flips, the port semantics don't:

- **:5432** = current data — psql, application, anything that wants the
  active dataset.
- **:5433** = where new dumps land / verify staging — `pg_restore` aims here,
  sanity checks aim here.

There is no third "transient" container during import. The staging slot is
permanently there, ready to receive the next dump.

## Network identity

The user-facing endpoint is **the pgbouncer container's IP, on ports 5432 and
5433**. That IP must be stable across the life of the colima VM and not
change on promote, restart, or backend snapshot/restore. This is achieved by
pinning the pgbouncer container's address at the device level, e.g.:

```
incus config device override pg-bouncer eth0 ipv4.address=10.x.x.10
```

The two backend containers' IPs are an internal detail. Tooling reaches them
by container name (`pg-dev-a`, `pg-dev-b`) via incus's bridge DNS; they are
never written into a client `.pgpass` or connection string.

## Authentication

Single guiding principle: **set up `.pgpass` once, never touch it again.**

- One Postgres role (`$PG_USER` from `.env`) with one password, used for both
  application access through the bouncer and direct backend operations.
- That same role/password is registered in a single
  `/etc/pgbouncer/userlist.txt`, shared by both pgbouncer instances and
  generated once during `cmd_up`. No `auth_query`, no SCRAM verifier
  regeneration after every reconfigure.
- Both pgbouncer pools run in **session** mode — preserves prepared
  statements, `SET`, advisory locks; behaves indistinguishably from a direct
  connection from the client's point of view.
- Each pgbouncer's admin interface is the special `pgbouncer` virtual
  database on its own `listen_port` (`:5432` for active, `:5433` for
  staging), accessed by connecting as `pgb_admin` (declared in
  `admin_users` and listed in `userlist.txt`). Promote and observability
  go through `incus exec pg-bouncer -- psql -p {5432,5433} -U pgb_admin
  -d pgbouncer …`.
- The `pgb_admin` user shares the application user's password. One secret,
  one `.pgpass` line — dev convenience, explicit non-goal w.r.t. privilege
  separation.
- Backend Postgres `pg_hba.conf` accepts the role from the bouncer's IP and
  from the local socket. No host-wide open auth.

The `.pgpass` line a developer adds once is port-wildcarded so it covers both
listeners:

```
10.x.x.10:*:*:$PG_USER:$PG_PASSWORD
```

After that, `psql`, application clients, migration tools and `pg_restore` all
just work, forever, regardless of which slot is active.

## Snapshot model

Snapshots are **per backend slot**. `pg-dev-a` and `pg-dev-b` have
independent snapshot timelines that never merge or get copied between slots.

- `initial` on each slot = a clean, role-and-database-bootstrapped Postgres
  with no user data. Taken once during `cmd_up`, used to reset that slot
  before each import.
- Subsequent named snapshots on a slot record points-in-time after data has
  been loaded or modified on that slot.

The active slot keeps producing snapshots as you work. The staging slot
typically holds `initial` (waiting for the next import) plus, briefly during
an import, intermediate marks like `initial-loaded`.

A promote does *not* touch snapshots. The previously active slot keeps its
full timeline as the rollback path.

## Workflow

### Steady state (no import in flight)

The active slot serves queries through the bouncer on :5432. The staging slot
is running but idle, reachable through the bouncer on :5433, holding its
`initial` snapshot. Day-to-day snapshot/restore on the active slot works
exactly as today's `make pg.snapshot` / `make pg.restore`.

### Importing a new dump

```shell
# 1. Reset staging to its clean initial state.
make pg.staging.reset

# 2. Run pg_restore through the bouncer's staging port. The active port
#    (:5432) keeps serving live queries the whole time.
pg_restore … --host=10.x.x.10 --port=5433 --dbname=$PG_DB …    # ~90 min

# 3. Sanity check the new data while still on staging.
psql --host=10.x.x.10 --port=5433 --dbname=$PG_DB -c '\dt'

# 4. Take a checkpoint of the loaded state.
make pg.staging.snapshot name=initial-loaded

# 5. Promote. Atomic from the client's point of view (PAUSE → reload → RESUME
#    on both admin consoles). Both port mappings flip together.
make pg.promote
```

After step 5, the slot that was staging is now active (served on :5432). The
slot that was active becomes the new staging (served on :5433) — still
holding its data and snapshots, ready to be either rolled back to (re-promote)
or reset for the next import.

### Rollback

A bad import is undone by a second `make pg.promote`. The previously active
slot is untouched; the pointer just flips back. No data is regenerated.

## Command surface

Two parallel families plus the flip, mirroring today's surface. The staging
family operates **directly on the backend container** via `incus exec`,
because it's used for ops/snapshot work — not through the bouncer:

| acts on active             | acts on staging                | meaning                  |
| -------------------------- | ------------------------------ | ------------------------ |
| `make pg.psql`             | `make pg.staging.psql`         | psql into that slot (via incus exec) |
| `make pg.shell`            | `make pg.staging.shell`        | bash in that slot        |
| `make pg.logs`             | `make pg.staging.logs`         | tail postgres logs       |
| `make pg.snapshot name=…`  | `make pg.staging.snapshot …`   | snapshot that slot       |
| `make pg.restore name=…`   | `make pg.staging.restore …`    | restore on that slot     |
| `make pg.restore-last`     | `make pg.staging.restore-last` | restore most recent      |
| `make pg.snapshots`        | `make pg.staging.snapshots`    | list snapshots           |
|                            | `make pg.staging.reset`        | shortcut: restore `initial` |

Plus the bouncer-aware operations:

| command              | meaning                                                |
| -------------------- | ------------------------------------------------------ |
| `make pg.endpoint`   | print both bouncer ports and their roles (see below)   |
| `make pg.promote`    | flip active/staging on both bouncer instances at once  |
| `make pg.status`     | print pointer + state of all three containers          |

`make pg.endpoint` prints both port mappings so a client always knows where
to point what:

```
active   host=10.x.x.10 port=5432 dbname=$PG_DB   (current data)
staging  host=10.x.x.10 port=5433 dbname=$PG_DB   (import target / opposite of active)
```

Direct-to-backend tooling (`pg.psql`, `pg.staging.psql`, `pg.shell`,
snapshot/restore ops) goes via `incus exec` against the container, because
snapshots are an incus-level operation on the container, not a Postgres-level
one. The bouncer is in the path of client applications and the import
workflow.

`pg.staging.host` is no longer needed for the import workflow — the import
endpoint is just `10.x.x.10:5433`. It may be retained as an ops convenience
(printing the staging container's container-level IP for direct-to-backend
debugging), but it is not part of the documented import path.

## State

A single file `var/active-slot` contains the literal text `a` or `b` and is
the source of truth for which slot is active. It is written atomically
(tmpfile + rename) and is the *only* thing `make pg.promote` mutates besides
the two pgbouncer `[databases]` lines (one per `.ini`).

Loss of `var/active-slot` is recoverable: either `.ini` is authoritative
(whatever the :5432 ini names is `a` or `b`; the file just caches that
decision for shell tooling).

## Implementation outline

`cmd_up` (one-time provisioning per colima VM):

1. Launch `pg-dev-a`, install Postgres 17, write config, create role and DB,
   `incus snapshot create pg-dev-a initial`.
2. Launch `pg-dev-b` the same way (independent install — keeps the two truly
   symmetric and decoupled). Snapshot `initial`.
3. Launch `pg-bouncer`, install pgbouncer, pin its eth0 to the chosen stable
   IP.
4. Render `/etc/pgbouncer/userlist.txt` once from `$PG_USER`/`$PG_PASSWORD`,
   shared by both instances.
5. Render two `.ini` files from a single template + initial state
   (`active-slot=a`):
   - `pgbouncer-active.ini`  → `listen_port=5432`, admin `6432`,
     `$PG_DB` → `pg-dev-a`.
   - `pgbouncer-staging.ini` → `listen_port=5433`, admin `6433`,
     `$PG_DB` → `pg-dev-b`.
6. Set up systemd template instances `pgbouncer@active` and
   `pgbouncer@staging` reading the matching `.ini`, sharing `userlist.txt`.
   Enable and start both.
7. Write `var/active-slot=a`.
8. Print `pg.endpoint` and a ready-to-paste `.pgpass` line.

`cmd_promote`:

1. Read `var/active-slot`, derive the new `(active, staging)` pair (a/b
   swapped).
2. `_bouncer_admin active "PAUSE $PG_DB;"` and
   `_bouncer_admin staging "PAUSE $PG_DB;"`.
3. Re-render both `.ini` files from the new state, from the same shared
   template. They remain cross-connected by construction.
4. `_bouncer_admin active "RELOAD;"` / `_bouncer_admin staging "RELOAD;"` and
   `_bouncer_admin active "RESUME $PG_DB;"` /
   `_bouncer_admin staging "RESUME $PG_DB;"`.
5. Atomically write the new value of `var/active-slot`.
6. Print new status.

Total promote wall-clock: sub-second. Both port mappings flip together,
dominated by the time PAUSE waits for in-flight transactions to finish (none
on :5433 during the import workflow, because `pg_restore` is the only thing
that talks to it and it has already finished by step 4 of the workflow).
