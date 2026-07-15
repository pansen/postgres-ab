# pg-dev — a/b backends behind a host-port proxy

A specification for this repo's Postgres dev wrapper, running natively on Incus.

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
- No connection pooling. Each host port is a plain TCP passthrough.
- The ports are published on the host's LAN interfaces and guarded **only** by
  the Postgres password. The trusted boundary is the LAN — fine for a home/office
  network, not for an untrusted one. (Skip `make firewall` to keep it host-only.)
- No multi-user concerns. One developer, one laptop.

Anything that adds friction in the name of robustness — extra auth steps,
credential rotation rituals, certificate management — is out of scope.

## Architecture

Three containers on the host's Incus bridge (`incusbr0`). There is **no
pgbouncer**: a bare `pg-proxy` container carries two host-bound Incus `proxy`
devices, one per client-facing port. Each is a per-connection TCP relay to a
backend's Postgres — indistinguishable from a direct connection.

```
                                host  <LAN-IP>
                       ┌────────────────┴────────────────┐
    client (app)  ───► │ :5432  ┐  proxy devices on       │
    pg_restore    ───► │ :5433  ┘  pg-proxy (bind=host)    │
                       └──────┬──────────────────┬─────────┘
                        connect │          connect │
                   tcp:<active-ip>:5432   tcp:<staging-ip>:5432
                              ▼                    ▼
                          pg-dev-a             pg-dev-b
                       (Postgres 17,         (Postgres 17,
                        own snapshots,        own snapshots,
                        pinned .11)           pinned .12)
```

- `pg-proxy` — a bare container (no packages, no config). It exists only to host
  the two `proxy` devices. With `bind=host` the forkproxy listens on the host's
  `0.0.0.0:<port>` (so any LAN machine can reach it at the host's IP) and dials
  the `connect` target from inside `pg-proxy`'s netns, over `incusbr0`, to the
  backend's pinned IP. `pg-proxy`'s own address is irrelevant and is not pinned.
  - **:5432** → the currently **active** backend.
  - **:5433** → the currently **staging** backend.

  Both `connect` targets are derived from the single state file
  `var/active-slot`, so they cannot drift.
- `pg-dev-a`, `pg-dev-b` — symmetric backends, both long-lived, both running.
  At any moment one is **active** (served on :5432) and the other is
  **staging** (served on :5433).

Client-facing semantics are stable forever; the physical slot underneath
flips, the port semantics don't:

- **:5432** = current data — psql, application, anything that wants the
  active dataset.
- **:5433** = where new dumps land / verify staging — `pg_restore` aims here,
  sanity checks aim here.

Because nothing pools connections, `CREATE`/`DROP DATABASE`, `LISTEN`/`NOTIFY`,
prepared statements and advisory locks all behave exactly as a direct
connection on either port. (An earlier pgbouncer-based design needed a separate
pooler-free `:5434` to dodge session-pooling `ObjectInUse` on `DROP DATABASE`;
with a plain proxy that problem doesn't exist, so there is no `:5434`.)

There is no third "transient" container during import. The staging slot is
permanently there, ready to receive the next dump.

## Network identity

The user-facing endpoint is **the host's LAN IP, on ports 5432 and 5433**. The
proxy devices listen on `0.0.0.0`, so every host interface works; `make
pg.endpoint` reports the primary one (override with `PG_HOST_IP`). The endpoint
does not change on promote, restart, or backend snapshot/restore.

The two backend containers keep **pinned** `incusbr0` IPs (`.11`/`.12` by
default; override with `PG_BACKEND_A_IP` / `PG_BACKEND_B_IP`). The proxy dials
them by IP, and a pinned `ipv4.address` yields a static lease that Incus
regenerates on daemon start — so the target survives a host reboot. A dynamic
lease could drift to a different address and silently break the forward. The
backend IPs are an internal detail; they are never written into a client
`.pgpass` or connection string.

On Fedora, firewalld must (a) allow the published ports and (b) trust the
`incusbr0` bridge, otherwise DHCP on the bridge is dropped and a backend comes
up IPv6-only. `make firewall` does both (`--permanent`, so it persists).

## Authentication

Single guiding principle: **set up `.pgpass` once, never touch it again.**

- One Postgres role (`$PG_USER` from `.env`) with one password, used for both
  application access through the proxy and direct backend operations.
- Clients authenticate **directly against the backend Postgres** (the proxy is a
  transparent TCP relay, not an auth endpoint). There is no `userlist.txt`, no
  SCRAM verifier to regenerate, no separate admin user or admin console.
- Backend Postgres `pg_hba.conf` accepts the role over `scram-sha-256` from any
  address (`0.0.0.0/0`) and via the local socket. This is deliberate: the
  connection arrives from the proxy's `incusbr0` address, and locking it down
  further buys nothing given the LAN-exposure model above.

The `.pgpass` line a developer adds once is port-wildcarded so it covers both
ports (at the host's LAN IP):

```
<host-ip>:*:*:$PG_USER:$PG_PASSWORD
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

Snapshots are copy-on-write on a btrfs (native Linux) or ZFS (under colima on
macOS) storage pool, so already-loaded states stay cheap to checkpoint and
restore.

## Workflow

### Steady state (no import in flight)

The active slot serves queries on the host's :5432. The staging slot is running
but idle, reachable on :5433, holding its `initial` snapshot. Day-to-day
snapshot/restore on the active slot works exactly as `make pg.snapshot` /
`make pg.restore`.

### Importing a new dump

```shell
# 1. Reset staging to its clean initial state.
make pg.staging.reset

# 2. Run pg_restore against the staging port. The active port (:5432) keeps
#    serving live queries the whole time.
pg_restore … --host=<host-ip> --port=5433 --dbname=$PG_DB …    # ~90 min

# 3. Sanity check the new data while still on staging.
psql --host=<host-ip> --port=5433 --dbname=$PG_DB -c '\dt'

# 4. Take a checkpoint of the loaded state.
make pg.staging.snapshot name=initial-loaded

# 5. Promote. The :5432 endpoint is unchanged; both forwards re-point together.
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

Two parallel families plus the flip. The staging family operates **directly on
the backend container** via `incus exec`, because it's used for ops/snapshot
work — not through the proxy:

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

Plus the proxy-aware operations:

| command              | meaning                                                  |
| -------------------- | -------------------------------------------------------- |
| `make pg.endpoint`   | print both host ports and their roles + `.pgpass` line   |
| `make pg.promote`    | flip active/staging: re-point both proxy forwards        |
| `make pg.refresh`    | re-pin backend IPs + re-assert the host forwards         |
| `make status`        | versions, slot/proxy roles, per-backend table, snapshots  |

`make pg.endpoint` prints both port mappings so a client always knows where
to point what:

```
active   host=<host-ip> port=5432 dbname=$PG_DB   (current data)
staging  host=<host-ip> port=5433 dbname=$PG_DB   (import target / opposite of active)
```

Direct-to-backend tooling (`pg.psql`, `pg.staging.psql`, `pg.shell`,
snapshot/restore ops) goes via `incus exec` against the container, because
snapshots are an Incus-level operation on the container, not a Postgres-level
one. The proxy is in the path only of client applications and the import
workflow.

## State

A single file `var/active-slot` contains the literal text `a` or `b` and is
the source of truth for which slot is active. It is written atomically
(tmpfile + rename) and is the *only* persistent thing `make pg.promote` mutates;
the two proxy `connect` targets are derived from it.

Loss of `var/active-slot` is recoverable: the proxy devices are authoritative
(whichever backend IP the `:5432` device's `connect` names is the active slot;
the file just caches that decision for shell tooling). It also defaults to `a`
when absent.

## Implementation outline

`cmd_up` (one-time provisioning):

1. Launch `pg-dev-a`, pin its eth0 to its static IP, install Postgres 17, write
   config, create role and DB, `incus snapshot create pg-dev-a initial`.
2. Launch `pg-dev-b` the same way (independent install — keeps the two truly
   symmetric and decoupled). Snapshot `initial`.
3. Launch `pg-proxy` (bare container, no install). Wait for it to be running.
4. Write `var/active-slot=a` and create the two host-bound proxy devices:
   - `main`    → `listen=tcp:0.0.0.0:5432`, `connect=tcp:<active-ip>:5432`.
   - `staging` → `listen=tcp:0.0.0.0:5433`, `connect=tcp:<staging-ip>:5432`.
5. Print `pg.endpoint` and a ready-to-paste `.pgpass` line.

`cmd_promote`:

1. Read `var/active-slot`, derive the new `(active, staging)` pair (a/b swapped).
2. Atomically write the new value of `var/active-slot`.
3. Re-point both proxy devices' `connect` targets from the new state
   (`incus config device set pg-proxy {main,staging} connect=…`). Re-pointing
   restarts the forkproxy, so open TCP connections drop and clients reconnect —
   the `host:port` endpoints are unchanged.
4. Print new status.

There is no drain step. With no pool there is no session state to release, so
promote is unconditionally sub-second and cannot hang. The only observable
effect is that in-flight connections reconnect (onto the new active on :5432, or
the new staging on :5433). During the import workflow :5433 has no clients —
`pg_restore` is the only thing that talks to it and has already finished by
step 4 — so nothing is disturbed there.

`cmd_refresh` (run on every `make start`, so the setup survives a host reboot):
re-pin each backend's static IP if it has drifted, then re-create-or-re-point
the two host forwards from current state.
