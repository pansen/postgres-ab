# Snapshottable PostgreSQL on Apple container machine

Working with PostgreSQL dumps is painful when `pg_restore` takes ~90 minutes
and the development database is unreachable for the whole window. This repo
runs Incus inside one persistent Apple container machine and provides:

- two long-lived PostgreSQL 17 backends (`pg-dev-a`, `pg-dev-b`) with
  independent, copy-on-write snapshot timelines;
- a stable client endpoint on `127.0.0.1`, with fixed role ports: `:5442` is
  active and `:5443` is staging, so a new dump can load without interrupting the
  current dataset;
- `make pg.promote` to switch the roles without copying data.

This requires Apple silicon, macOS 26, and Apple's `container` CLI 1.1 or
newer. Install the signed package from the
[Apple container releases](https://github.com/apple/container/releases).

## Design

```text
macOS client
    │
    │ 127.0.0.1:5442 / :5443          (stable, never changes)
    ▼
socat forwarder (host LaunchAgent)
    │
    │ <machine-ip>:5432 / :5433       (re-resolved on every `make start`)
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

The Apple machine is the persistent outer Linux environment. The repository
remains on macOS and is visible at the same `/Users/...` path through the
machine's home mount, so `.env`, the active-slot pointer, and exports live
outside the machine.

### Control plane (`pgdev` ↔ `pgdevd`)

The stateful control path no longer injects shell over Apple's `container exec`.
A resident daemon, **`pgdevd`**, runs inside the machine under systemd and
serves an HTTP/JSON API on the machine's `eth0` (port `5440`, bearer token in
`var/agent-token`). The host CLI, **`pgdev`**, talks to it:

```text
pgdev (macOS) ── HTTP/JSON ──▶ pgdevd (machine) ──▶ Incus socket + XFS store
```

`pgdev up`/`down`/`status`/`promote`/`refresh`/`snapshots`/`ip`/`snapshot`/
`restore` are all API calls — after boot there are **zero `container machine run`
execs on the control path**. `promote` collapses to "flip the active-slot
pointer, then reconcile the proxy `connect=` targets"; `up` provisions both
backends from a golden `pg-dev-base` image (PostgreSQL installed once, then
`incus publish`ed — minutes → seconds) and creates each slot's cluster on its
XFS slot; the daemon owns the single-mutation lock, the write-ahead journal, and
crash recovery on start. Machine setup (the XFS store + Incus topology) is
`pgdevd bootstrap`, run as the daemon unit's `ExecStartPre`. Both binaries are
built by `make pgdevd` (they share one git-stamped version); `pgdev agent
deploy` — run automatically by `make start` — cross-compiles nothing new, it
delivers the prebuilt daemon over the home mount, installs it atomically to a
machine-local path, restarts the unit, and confirms the `GET /v1/version`
handshake so a stale daemon fails loudly. Interactive work (`psql`, `shell`,
`logs`) and full export/import still run through `scripts/pg-dev-local` for now
(later slices).

### Networking

The nested `incusbr0` subnet is not routed to macOS. The two Incus proxy
devices therefore use `bind=host`: they listen on the Apple machine's own IP
and connect to pinned `.11`/`.12` addresses on the nested bridge. Do not use a
backend's 10.x Incus address from macOS.

The machine's IP on macOS's internal virtualization bridge is a lease from
macOS's built-in DHCP server (`bootpd`) and is **not pinnable**: it — and even
its whole `/24` — can change after a macOS reboot or machine recreation. Apple's
`container machine` CLI exposes no way to fix it (no `--network`, `--publish`, or
static-IP option), and the machine registers no resolvable DNS name. So rather
than chase the address, the client endpoint is decoupled from it: a per-user
`launchd` LaunchAgent runs `socat` listeners on `127.0.0.1:5442` / `:5443` and
relays them to the machine's current IP (on its `5432` / `5433`). The client
ports are deliberately offset from `5432`/`5433` so a local PostgreSQL on the
default port isn't shadowed. Clients always connect to **`127.0.0.1:5442`**
(active) / **`:5443`** (staging) — a permanent endpoint, identical on every Mac.

This is hands-off: **`make start` installs the forwarder on first run and
re-points it at the current machine IP on every run** (the machine is down until
`make start` anyway, so that is the only moment the IP can have changed). It
needs `socat` (`brew install socat`, enforced by `make deps`). `make
endpoint.status` shows the forwarder's state; `make endpoint.uninstall` removes
it (set `PG_ENDPOINT_AUTOINSTALL=0` to stop `make start` re-installing it).

`PG_CLIENT_HOST` overrides the printed client host if you prefer to address the
machine IP directly instead of using the forwarder.

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

`pgdevd bootstrap` (run as the daemon unit's `ExecStartPre`) instead creates a
sparse XFS loop filesystem (140 GiB by default) inside the machine root disk,
mounts it, and configures the Incus storage/network/profile. Apple's 1.1 boot examples
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
can take several minutes. `make start` also installs and re-points the host
forwarder that gives you the stable `127.0.0.1` endpoint (see
[Networking](#networking)) — no separate step needed.

Status prints endpoints similar to:

```text
active   host=127.0.0.1 port=5442 dbname=<PG_DB>
staging  host=127.0.0.1 port=5443 dbname=<PG_DB>

.pgpass lines:
127.0.0.1:5442:*:<PG_USER>:<PG_PASSWORD>
127.0.0.1:5443:*:<PG_USER>:<PG_PASSWORD>

psql commands:
  active:  psql --host=127.0.0.1 --port=5442 --username=<PG_USER> --dbname=<PG_DB>
  staging: psql --host=127.0.0.1 --port=5443 --username=<PG_USER> --dbname=<PG_DB>
```

Put the printed lines in `~/.pgpass`. The `127.0.0.1` host is permanent — it does
not change across reboots or machine recreation, so saved connection strings keep
working. Set `PG_CLIENT_HOST` to address the machine IP directly instead of the
forwarder.

## Day-to-day workflow

Port 5442 always means the active/current dataset. Port 5443 always means the
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

# 2. Import through the staging port on the stable endpoint.
pg_restore --host=127.0.0.1 --port=5443 --dbname="$PG_DB" \
  --jobs=4 your-dump.pgdump

# 3. Verify and checkpoint staging.
psql -h 127.0.0.1 -p 5443 -d "$PG_DB" -c '\dt'
make pg.staging.snapshot name="$(date +%Y-%m-%dT%H-%M-%S)_dump_import"

# 4. Swap roles. Open connections reconnect; host and ports stay the same.
make pg.promote
```

`make pg.promote` requires the proxy and **both backends to be running**. If
the staging backend is stopped, start it first with `make pg.staging.start`.
If the new data is bad, `make pg.promote` again immediately points `:5442`
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

**Disk space — the sparse-VM-disk trap (important):** the Apple container
machine's root disk (`vdb`) is a **sparse image on macOS that only grows**.
Blocks written inside the guest (the XFS PostgreSQL store, `pg_restore` output,
Incus image layers) are added to the macOS-side image and are **not returned to
macOS when you delete them** — Apple's runtime does not compact the disk on
discard/TRIM, and there is no supported per-machine size cap. So a large restore
can silently ratchet your Mac's free space to zero. When the next guest write
then fails, the loop-backed XFS store shuts down mid-write with an I/O error and
PostgreSQL drops into recovery (you'll see `Input/output error` on data files and
`XFS … Filesystem has been shut down`).

Controls:

- `make disk` — show macOS free space, the Apple container storage footprint,
  the guest root-disk usage, and the `.xfs` store's actual (physical) size.
- `make disk.check` — a fail-fast pre-flight (macOS free space vs
  `DISK_MIN_FREE_GB`, default 40 GiB) that gates `pg.up`, the staging
  restore/reset commands, and `import-last`. Set `DISK_MIN_FREE_GB` to **at least
  the size of the dump you're about to restore** (a restore can grow the image by
  roughly the DB size). Note the client-side `pg_restore` itself runs outside
  make, so this guards the step right before it, not the copy.
- **Reclaim** a bloated VM disk with **`make recreate`** — deleting the machine
  frees the entire sparse image on macOS; then rebuild and restore from a prior
  `make pg.export`. This is the only dependable shrink today (`container system
  prune` only touches the CLI's ~GB of image/build cache, not the root disk).
- **Cap future growth** (strategic): relocate the bulky data onto a dedicated
  APFS volume with a quota (`diskutil apfs addVolume … -quota …`) mounted into
  the machine, so the payload can't consume the whole macOS volume. Not wired up
  here yet — see `issues/`.

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
- `PG_ACTIVE_PORT`, `PG_STAGING_PORT` — machine-side proxy ports (`5432`/`5433`);
- `PG_CLIENT_ACTIVE_PORT`, `PG_CLIENT_STAGING_PORT` — host loopback ports the
  forwarder listens on (`5442`/`5443`);
- `PG_BACKEND_A_IP`, `PG_BACKEND_B_IP` — optional nested bridge pins;
- `PG_CLIENT_HOST` — client host printed for connections (default `127.0.0.1`,
  the forwarder endpoint);
- `PG_MACHINE_IP` — optional override of the discovered machine IP.

## Why a Makefile if there is a script?

Shell completion for `make` targets is convenient; the script contains the
actual lifecycle logic.
