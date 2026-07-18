# Snapshottable PostgreSQL on Apple container machine

Working with PostgreSQL dumps is painful when `pg_restore` takes ~90 minutes
and the development database is unreachable for the whole window. This repo
runs Incus inside one persistent Apple container machine and provides:

- two long-lived PostgreSQL 17 backends (`pg-dev-a`, `pg-dev-b`) with
  independent, copy-on-write snapshot timelines;
- stable role ports on the Apple machine: `:5432` is active and `:5433` is
  staging, so a new dump can load without interrupting the current dataset;
- `make pg.promote` to switch the roles without copying data.

This requires Apple silicon, macOS 26, and Apple's `container` CLI 1.1 or
newer. Install the signed package from the
[Apple container releases](https://github.com/apple/container/releases).

## Design

```text
macOS client
    │
    │ <apple-machine-ip>:5432 / :5433
    ▼
┌──────────────── Apple container machine ────────────────┐
│ Incus host                                               │
│                                                         │
│ pg-proxy (bare Incus container, device owner)           │
│   bind=host :5432 ─ active  ─┐                           │
│   bind=host :5433 ─ staging ─┼─ raw TCP proxy devices   │
│                              │                           │
│                 ┌────────────┴────────────┐              │
│                 ▼                         ▼              │
│             pg-dev-a                  pg-dev-b           │
│             PostgreSQL 17             PostgreSQL 17      │
│                 │                         │              │
│                 ▼                         ▼              │
│          XFS slot a/current        XFS slot b/current    │
│          + reflink snapshots       + reflink snapshots   │
└─────────────────────────────────────────────────────────┘
```

The Apple machine is the persistent outer Linux environment. The Makefile
runs `scripts/pg-dev-local` as root inside it, where the Incus socket and the
database storage are available. The repository remains on macOS and is visible
at the same `/Users/...` path through the machine's home mount, so `.env`, the
active-slot pointer, and exports live outside the machine.

### Networking

The nested `incusbr0` subnet is not routed to macOS. The two Incus proxy
devices therefore use `bind=host`: they listen on the Apple machine's own IP
and connect to pinned `.11`/`.12` addresses on the nested bridge. `make
pg.status` discovers and prints the current machine IP; do not use a backend's
10.x Incus address from macOS.

The client endpoint address is the machine's IP on macOS's internal
virtualization bridge (`bridge100`, host side `192.168.64.1/24`) — a
host-only NAT network whose leases come from macOS's built-in DHCP server
(`bootpd`), not from your LAN's DHCP. The endpoint is therefore reachable
only from this Mac, and the lease is **not pinned**: it can change after a
macOS reboot or machine recreation. Re-check with `make pg.status` after
reboots, and update any saved `.pgpass` entries or connection strings
accordingly. `PG_MACHINE_IP` in `.env` overrides only what is printed by the
script; it does not pin the actual IP.

There is no connection pooler. Each port is a per-connection TCP passthrough,
so `CREATE`/`DROP DATABASE`, `LISTEN`/`NOTIFY`, prepared statements, advisory
locks, and parallel `pg_restore` behave like direct PostgreSQL connections.
Promoting re-points both proxy devices. Updating them may drop existing TCP
sessions, so clients should reconnect; the role ports themselves do not change.

### Snapshots on Apple's stock kernel

Apple 1.1's recommended container-machine kernel lacks the Btrfs, ZFS, and
DM-thin stack needed by Incus's optimized snapshot backends. Incus therefore
falls back to `dir`, whose snapshots are full directory copies—not practical
for repeated multi-gigabyte PostgreSQL checkpoints.

`scripts/apple-machine-init` instead creates a sparse XFS loop filesystem
(140 GiB by default) inside the machine root disk. Apple's 1.1 boot examples
show a 512 GiB root device, but that size is not a documented compatibility
guarantee. PostgreSQL data for each slot is mounted from the XFS filesystem,
and snapshot commands use reflink copies. Creating a checkpoint is fast and
consumes additional blocks only as the live dataset and snapshots diverge.

Snapshots cover PostgreSQL data, not the disposable Ubuntu container root.
PostgreSQL configuration is provisioned identically in both containers. Every
snapshot stops PostgreSQL cleanly before cloning the data and starts it again
afterward. Restoring an older snapshot retains the original workflow's
timeline semantics: snapshots newer than the target are shown and deleted
after confirmation (interactively you get a [Y/n] prompt, but non-interactively
you must pass `force=1`, e.g. `make pg.restore name=foo force=1`, because stdin
cannot answer prompts through the machine transport).

The XFS size is a logical ceiling; the backing file is sparse. Apple container
CLI 1.1 does not offer a machine disk-size flag. `PG_DATA_DISK_SIZE` is only
used at first creation; to grow it later, raise the value in `.env` and the next
`make start` will expand the sparse XFS store online via `xfs_growfs`. Shrinking
the store is not supported.

### Scope and safety model

This is a local-development tool for one trusted developer and laptop. It is
not replication, automatic failover, a zero-downtime service, or a hardened
multi-user deployment. PostgreSQL durability features such as `fsync` are
deliberately disabled for import speed. Snapshots are checkpoints on the same
physical disk, not backups.

## First setup

```shell
make deps
cp .env.example .env
# edit credentials/resources if desired

make start     # first run builds and creates the persistent Apple machine
make pg.up     # provisions both PostgreSQL backends and the proxy
make pg.status
```

The first `make start` builds an Ubuntu 26.04 machine image containing systemd,
Incus, jq, and XFS tools. Later starts reuse the named persistent machine.
`make pg.up` installs PostgreSQL 17 in two nested Ubuntu 24.04 containers and
can take several minutes.

Status prints endpoints similar to:

```text
active   host=192.168.64.2 port=5432 dbname=<PG_DB>
staging  host=192.168.64.2 port=5433 dbname=<PG_DB>

.pgpass line:
    192.168.64.2:*:*:<PG_USER>:<PG_PASSWORD>

psql commands:
  active:  psql --host=192.168.64.2 --port=5432 --username=<PG_USER> --dbname=<PG_DB>
  staging: psql --host=192.168.64.2 --port=5433 --username=<PG_USER> --dbname=<PG_DB>
```

Put the printed line in `~/.pgpass`. If the Apple machine receives a different
IP later, rerun `make pg.status` and update the host field. `PG_MACHINE_IP` can
override only the address printed by the script when a routed alias is used.

## Day-to-day workflow

Port 5432 always means the active/current dataset. Port 5433 always means the
opposite staging dataset.

```shell
make start
make pg.status
make pg.psql
make pg.logs
```

Load a fresh dump without blocking the active database:

```shell
# 1. Reset staging to its clean initial checkpoint.
make pg.staging.reset

# 2. Import through the staging port printed by `make pg.status`.
pg_restore --host=<machine-ip> --port=5433 --dbname="$PG_DB" \
  --jobs=4 your-dump.pgdump

# 3. Verify and checkpoint staging.
psql -h <machine-ip> -p 5433 -d "$PG_DB" -c '\dt'
make pg.staging.snapshot name="$(date +%Y-%m-%dT%H-%M-%S)_dump_import"

# 4. Swap roles. Open connections reconnect; host and ports stay the same.
make pg.promote
```

`make pg.promote` requires the proxy and **both backends to be running**. If
the staging backend is stopped, start it first with `make pg.staging.start`.
If the new data is bad, `make pg.promote` again immediately points `:5432`
back to the previous backend and its untouched timeline.

## Snapshot and restore commands

Unprefixed commands operate on the active physical slot:

```shell
make pg.snapshot name="$(date +%Y-%m-%dT%H-%M-%S)_before-migration"
make pg.restore name=<snapshot>
make pg.restore-last
make pg.snapshots
```

The staging slot has the parallel command family:

```shell
make pg.staging.snapshot name=<snapshot>
make pg.staging.restore name=<snapshot>
make pg.staging.restore-last
make pg.staging.reset
make pg.staging.stop
make pg.staging.start
```

`force=1` replaces a same-named snapshot:

```shell
make pg.snapshot name=before-test force=1
```

Snapshot names may contain letters, digits, dots, underscores, and hyphens.

## Inspecting and entering the environments

```shell
make status               # endpoints, roles, states, IPs, timelines
make status/incus         # Incus versions/resources/list
make pg.ip                # compact endpoint → backend mapping
make pg.refresh           # repair internal IP pins and proxy targets
make machine.status       # Apple machine JSON
make machine.shell        # shell as the mapped macOS user
make machine.shell.root   # root shell for diagnostics
make pg.shell             # shell in the active PostgreSQL container
```

## Export, deletion, and recovery

Snapshots disappear with the Apple machine. `make pg.export` writes a recovery
tarball under the host repository's `var/` directory containing both Incus
backends, the proxy, active-slot state, and the unmounted sparse XFS image.
Archiving the filesystem image preserves its shared reflink extents instead of
expanding each checkpoint into a full independent copy:

```shell
make pg.export

# Restore over the current complete setup, or after rebuilding an empty
# outer machine:
make recreate
make pg.import-last
```

`make pg.import-last` only reads the full-setup `pg-dev-all-*.tar.gz` archives
produced by this version; per-container exports from the old Colima setup are
not importable.

Exports are intentionally much slower and larger than reflink checkpoints.
Use them before deleting or recreating the outer machine. Import accepts an
empty Incus host or a complete three-container setup and rolls back on a
failed validation/startup; it refuses a partial setup. Keep enough free disk
for the extracted sparse XFS image and the previous image during the swap.

```shell
make stop          # stop the persistent Apple machine; keep all data
make system.stop   # also stop Apple's container services
make pg.down       # delete all three Incus containers AND both XFS slot trees
make delete        # delete the outer machine and everything inside it
make recreate      # delete, rebuild, and start a fresh outer machine
```

`make delete`, `make recreate`, and `make pg.down` are destructive. Host-side
exports under `var/` survive outer-machine deletion. If a leftover experimental
machine from early testing exists (e.g. one created by hand named `pg`), it is
unrelated to this setup and can be reclaimed with `container machine delete
<name>`.

## Constraints & gotchas

**Repository location:** The repository must live under your macOS home directory.
The Apple container machine only mounts `$HOME` (home-mount), and every make
target executes the repo's scripts inside the machine via that mount. A repo
outside `$HOME` fails with a raw missing-directory error.

**systemd status:** `systemctl is-system-running` inside the machine reports
`degraded` permanently. This is expected and harmless—Apple's guest kernel has
no loadable-module support, so `systemd-modules-load` can never succeed.

## Configuration

See `.env.example`. The main settings are:

- `MACHINE_NAME`, `MACHINE_CPUS`, `MACHINE_MEMORY` — persistent Apple machine;
- `PG_DATA_DISK_SIZE` — first-creation XFS logical size;
- `PG_ACTIVE_PORT`, `PG_STAGING_PORT` — client-facing role ports;
- `PG_BACKEND_A_IP`, `PG_BACKEND_B_IP` — optional nested bridge pins;
- `PG_MACHINE_IP` — optional printed client-address override.

## Why a Makefile if there is a script?

Shell completion for `make` targets is convenient; the script contains the
actual lifecycle logic.
