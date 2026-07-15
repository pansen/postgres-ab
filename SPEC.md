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

Three containers inside the colima VM. The bouncer container carries **two
access paths** to each backend: a pooler-free direct proxy on the primary
port, and a pgbouncer session pool one range up.

```
                ┌────────────────────────────────────────────────┐
client (app) ──►│ pg-bouncer  (stable IP 10.x.x.10)              │
pg_restore  ──►│                                                │
                │  DIRECT proxies (incus proxy devices):         │
                │    :5432 ─ active   ─┐                          │
                │    :5433 ─ staging  ─┼─ per-connection TCP relay │
                │                       │  straight to the backend │
                │  POOLED pgbouncer (session mode):              │
                │    :5442 ─ active   ini ─┐                       │
                │    :5443 ─ staging  ini ─┼─ cross-connect        │
                │    (admin is the special │                       │
                │     `pgbouncer` db on the│                       │
                │     same port, pgb_admin)│                       │
                └──────────────┬───────────┴────────────────────┘
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

- `pg-bouncer` — single container, fixed identity, lives for the life of the
  colima VM. Hosts:
  - **two direct-forward proxy devices** — `:5432` → active backend, `:5433`
    → staging backend. Each is an incus `proxy` device that relays a raw TCP
    connection straight to the backend's Postgres (no pooler in the path). The
    `connect` target is re-pointed whenever active flips, so the port semantics
    are stable across promote.
  - **two pgbouncer instances** — **active** (`listen_port=5442`) and
    **staging** (`listen_port=5443`), each `[databases] $PG_DB` → whichever
    backend fills that role. pgbouncer has no separate admin port: admin
    commands are issued by connecting to the special `pgbouncer` virtual
    database on the same `listen_port` as the `pgb_admin` user. Both `.ini`
    files are rendered from the single state file `var/active-slot`, so they
    are always cross-connected and cannot drift.
- `pg-dev-a`, `pg-dev-b` — symmetric backends, both long-lived, both running.
  At any moment one is **active** (served on :5432 direct / :5442 pooled) and
  the other is **staging** (served on :5433 direct / :5443 pooled).

Client-facing semantics are stable forever; the physical slot underneath
flips, the port semantics don't:

- **:5432** = current data — psql, application, anything that wants the active
  dataset. Pooler-free, so it also carries no session stickiness: a client
  disconnect tears the backend connection down, which is what test suites that
  `CREATE`/`DROP` databases need (see below).
- **:5433** = where new dumps land / verify staging — `pg_restore` aims here,
  sanity checks aim here. Also pooler-free, so a bulk COPY stream isn't relayed
  through a single-threaded pgbouncer.
- **:5442 / :5443** = the pgbouncer session pools for active / staging, for
  callers that specifically want pooling. The tradeoff of the pool: through it,
  pgbouncer (`pool_mode=session`) keeps an idle server connection to a
  just-used database, so a subsequent `DROP DATABASE` fails with `ObjectInUse`
  — which is exactly why the primary :5432/:5433 are the pooler-free proxies.

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
  database on its own `listen_port` (`:5442` for active, `:5443` for
  staging), accessed by connecting as `pgb_admin` (declared in
  `admin_users` and listed in `userlist.txt`). Promote and observability
  go through `incus exec pg-bouncer -- psql -p {5442,5443} -U pgb_admin
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

The active slot serves queries on :5432 (direct) / :5442 (pooled). The staging
slot is running but idle, reachable on :5433 / :5443, holding its `initial`
snapshot. Day-to-day snapshot/restore on the active slot works exactly as
today's `make pg.snapshot` / `make pg.restore`.

### Importing a new dump

```shell
# 1. Reset staging to its clean initial state.
make pg.staging.reset

# 2. Run pg_restore through the staging port (:5433, pooler-free direct proxy —
#    no single-threaded pgbouncer relay in the COPY path). The active port
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

After step 5, the slot that was staging is now active (served on :5432/:5442).
The slot that was active becomes the new staging (served on :5433/:5443) — still
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
Direct (pooler-free, promote-aware — use for apps, tests, and imports):
  active   host=10.x.x.10 port=5432 dbname=$PG_DB   (current data)
  staging  host=10.x.x.10 port=5433 dbname=$PG_DB   (import target / opposite of active)

Pooled (pgbouncer, session mode — use when you need session pooling):
  active   host=10.x.x.10 port=5442 dbname=$PG_DB
  staging  host=10.x.x.10 port=5443 dbname=$PG_DB
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
(whatever the active ini names is `a` or `b`; the file just caches that
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
   - `pgbouncer-active.ini`  → `listen_port=5442`, `$PG_DB` → `pg-dev-a`.
   - `pgbouncer-staging.ini` → `listen_port=5443`, `$PG_DB` → `pg-dev-b`.
6. Set up systemd template instances `pgbouncer@active` and
   `pgbouncer@staging` reading the matching `.ini`, sharing `userlist.txt`.
   Enable and start both.
7. Write `var/active-slot=a`.
8. Add the two direct-forward proxy devices (`:5432` → active backend, `:5433`
   → staging backend).
9. Print `pg.endpoint` and a ready-to-paste `.pgpass` line.

`cmd_promote`:

1. Read `var/active-slot`, derive the new `(active, staging)` pair (a/b
   swapped).
2. `_promote_drain active 5442` and `_promote_drain staging 5443`.
3. Re-render both `.ini` files from the new state, from the same shared
   template. They remain cross-connected by construction.
4. `_bouncer_admin active "RELOAD;"` / `_bouncer_admin staging "RELOAD;"` and
   `_bouncer_admin active "RESUME $PG_DB;"` /
   `_bouncer_admin staging "RESUME $PG_DB;"`.
5. Atomically write the new value of `var/active-slot`.
6. Re-point the direct-forward proxies (`:5432` → new active, `:5433` → new
   staging) at their backends.
7. Print new status.

**Why `_promote_drain`, not a bare `PAUSE`.** `PAUSE` takes effect at once but
only *returns* after every server connection is released. In **session**
pooling a server stays bound to its client for the whole session, so a single
idle-but-connected client (an app pool, a forgotten `psql`) makes a bare
`PAUSE` block forever — this is a real stall we hit in practice, not a
theoretical one. `_promote_drain` runs the `PAUSE` with a `PROMOTE_PAUSE_TIMEOUT`
(default 10s) budget: if it drains in time, clients keep their TCP connection
and get re-routed on their next query (the nice path); if it doesn't, we force
the swap with `KILL`, which drops the lingering idle clients — they reconnect
to the new backend on their next query. Promote can no longer hang.

Total promote wall-clock: sub-second when nothing is holding a session open;
up to `PROMOTE_PAUSE_TIMEOUT` when an idle client has to be forced off. Both
port mappings flip together. (During the import workflow :5433 has no clients —
`pg_restore` is the only thing that talks to it and has already finished by
step 4 — so its drain is instant.)
