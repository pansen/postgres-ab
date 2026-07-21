# Spec 0001 — `pgdev`: replace the shell automation with a Go appliance

Status: proposed · Owner: TBD · Supersedes: `scripts/pg-dev-local`,
`scripts/host-endpoint`, `scripts/apple-machine-init`, most of `Makefile`.

This is an implementation spec. It is designed to be executed **in slices**
(§8); each slice is independently shippable and leaves `make` working. Anything
marked *(decision)* needs sign-off before that slice starts.

---

## 1. Problem & goals

The project (snapshottable PG17 inside one Apple `container` machine running
Incus) works but is ~2,540 lines of bash across four files that "read hard and
look brittle." The length is not incidental to shell: the engine is an ad-hoc
distributed system (host ↔ machine ↔ nested containers) glued through **three
hostile interfaces**, and ~half the code only compensates for them:

| Hostile interface | Compensation in current code |
|---|---|
| Apple `container exec` is broken (signals / 2nd-exec / stdin) | `Makefile:30` `PG_DEV_AUTO` TTY sniff; `exec </dev/null` (`apple-machine-init:9`, `pg-dev-local:285`); restore refuses non-interactive confirm (`pg-dev-local` ~978) |
| Machine IP is an unpinnable DHCP lease | entire `scripts/host-endpoint` (launchd + socat + cached-IP file) |
| Stock kernel has no CoW storage backend | hand-rolled transactional snapshot state machine: trap closures, `.tmp.$$`/`.replaced.$$` renames, `set +e` islands, `_slot_recovery_artifact` refusal (`pg-dev-local:723`) |

**Goals:** (1) replace bash with a typed, testable Go codebase; (2) give the
system real interfaces so the compensations stop needing to exist; (3) make the
snapshot engine crash-safe *and* unit-testable; (4) keep every existing `make`
entry point working through the whole migration.

**Non-goals:** HA, replication, multi-user, pooling, changing the client-facing
contract (`127.0.0.1:5442` active / `:5443` staging stays identical).

---

## 2. Fixed facts the design relies on

- **Incus REST API over the local unix socket**, official Go client
  `github.com/lxc/incus/client` (`ProtocolIncus`): `ConnectIncusUnix`,
  `CreateInstance`/`CreateInstanceFromImage`, `ExecInstance`,
  `UpdateInstanceState`, `UpdateInstance` (device config), `GetInstanceState`,
  `GetImageAlias`. The ~100 `incus` shell-outs become typed calls.
- **Apple `container` is Swift + XPC + CLI only — no REST API.** Machine
  *lifecycle* (create/run/stop/delete/build image, read eth0 IP) stays CLI, all
  behind one package (`internal/applecli`). This is a small surface.
- **PostgreSQL** = wire protocol → `pgx` for readiness/health probes.
- **Storage:** stock kernel has XFS + loop + reflink; no Btrfs/ZFS/dm-thin.

---

## 3. Current-state constants (source of truth for the port)

From `scripts/pg-dev-local:23-40`, `apple-machine-init`, `host-endpoint`, `.env.example`:

```
Containers:     pg-dev-a, pg-dev-b (BACKEND_PREFIX=pg-dev), pg-proxy
Nested image:   images:ubuntu/24.04/cloud
Slots:          a, b   → /var/lib/pg-dev-local/<slot>/{current,snapshots}
XFS store:      image /var/lib/pg-dev-local.xfs (sparse, reflink=1), size PG_DATA_DISK_SIZE (default 140G)
                mounted at /var/lib/pg-dev-local, fstab loop,nofail
Active pointer: <repo>/var/active-slot        (host-visible via home-mount)
Machine IP file:<repo>/var/machine-ip
Mutation lock:  /run/lock/pg-dev-local.lock   (flock, single mutation at a time)
Backend IPs:    <incusbr0-prefix>.11 / .12    (PG_BACKEND_A_IP / _B_IP override)
Machine ports:  5432 active / 5433 staging    (PG_ACTIVE_PORT / PG_STAGING_PORT)
Client ports:   5442 active / 5443 staging    (PG_CLIENT_ACTIVE_PORT / _STAGING_PORT)
Client host:    127.0.0.1                      (PG_CLIENT_HOST)
launchd label:  me.pansen.<MACHINE_NAME>-forward
PG creds:       PG_USER / PG_DB / PG_PASSWORD (required)
Machine:        MACHINE_NAME=vpg, MACHINE_CPUS=4, MACHINE_MEMORY=12G, image local/pg-incus-machine:26.04
```

`.env` is the single config source and stays so (loaded by both `pgdev` on the
host and `pgdevd` via the home-mount; the daemon reads a materialized subset).

---

## 4. Target architecture

One Go module (`module pansen.me/pgdev`, Go ≥ 1.23). Two binaries.

The machine becomes a **network appliance**: after boot there are **zero
`container machine run` execs on the control path**. `pgdev` (host) → HTTP/JSON →
`pgdevd` (machine) → Incus Go client over the unix socket.

```
cmd/pgdev/            host CLI (cobra)
cmd/pgdevd/           in-machine daemon (systemd unit, HTTP/JSON on 127-of-machine)
internal/config/      load .env, resolve derived values (IPs, ports, paths)
internal/blueprint/   desired topology as typed constants + Blueprint(activeSlot)
internal/reconcile/   diff live Incus state vs blueprint; repair to match
internal/incusops/    typed wrapper over github.com/lxc/incus/client
internal/task/        Task/Step, write-ahead journal, resume-on-start, recovery
internal/store/       XFS store: bootstrap/mount/grow, reflink clone, atomic swap
internal/pg/          pgx readiness/health, role+db provisioning, clean stop
internal/agentapi/    HTTP server (pgdevd) + typed client (pgdev)
internal/forward/     host TCP forwarder with self-healing IP rediscovery
internal/applecli/    the ONLY place `container` is shelled out
internal/backend/     Backend interface (see §11 Option Zero) — abstracts "slot"
```

**Imperative/declarative line** — everything recomputable from a constant is
declarative and *reconciled*; everything that is history is *journaled*. No
Terraform (`promote` rewrites proxy `connect=` on every flip → permanent
by-design drift; a state file would be stale within a workday).

---

## 5. Component specs

### 5.1 `internal/config`
```go
type Config struct {
    PGUser, PGDB, PGPassword string
    MachineName              string   // vpg
    ActivePort, StagingPort  int      // 5432 / 5433 (machine side)
    ClientActivePort, ClientStagingPort int // 5442 / 5443 (host side)
    ClientHost               string   // 127.0.0.1
    BackendAIP, BackendBIP   string   // resolved: <incusbr0 prefix>.11/.12 unless overridden
    DataDiskSize             string   // 140G
    RepoRoot                 string   // for var/ home-mount paths
}
func Load() (Config, error)   // reads .env, applies defaults from §3, validates required
```
- `BackendAIP/BIP` default resolution mirrors `_incusbr0_prefix`
  (`pg-dev-local:163`): read `incus network get incusbr0 ipv4.address`, take the
  prefix, append `.11`/`.12`.

### 5.2 `internal/blueprint`
Desired topology as data. `Blueprint(activeSlot)` returns the full intended
state; the reconciler makes reality match it.
```go
type Slot string // "a" | "b"
type Backend struct {
    Name     string // pg-dev-a
    Slot     Slot
    IP       string // pinned eth0 (.11/.12)
    DataDir  string // /var/lib/pg-dev-local/<slot>/current  → bind-mounted /var/lib/postgresql
}
type ProxyDevice struct {
    Name    string // "active" | "staging"
    Listen  string // connect=tcp:<machineIP>:<machinePort> ... actually bind=host listen
    Connect string // tcp:<backendIP>:5432
}
type Blueprint struct {
    Network  IncusNetwork   // incusbr0: ipv4.address=auto nat=true, ipv6 auto nat
    Pool     IncusPool      // default, driver dir
    Profile  IncusProfile   // default: root disk pool=default, eth0 nic network=incusbr0
    Backends [2]Backend
    Proxy    ProxyContainer // pg-proxy + two ProxyDevice (bind=host), targets = f(activeSlot)
}
func Compute(cfg Config, active Slot) Blueprint
```
- Proxy device `connect` targets are a **pure function of (cfg, activeSlot)** —
  this is the collapse that makes `promote = SetActive(b); Reconcile()`.
- PG config file content (`99-dev.conf`, `pg_hba.conf`) from
  `pg-dev-local:317-352` moves here as an embedded template constant.

### 5.3 `internal/incusops`
Thin typed wrapper; every method maps to a Go-client call. No business logic.
```go
type Client struct{ /* wraps incus.ConnectIncusUnix("/var/lib/incus/unix.socket", nil) */ }
func (c *Client) EnsurePool(p IncusPool) error
func (c *Client) EnsureNetwork(n IncusNetwork) error
func (c *Client) EnsureProfileDevices(p IncusProfile) error
func (c *Client) InstanceState(name string) (State, error)       // GetInstanceState
func (c *Client) Launch(name, image string, cfg InstanceConfig) error // CreateInstanceFromImage + start; NO stdin (kills </dev/null hack)
func (c *Client) SetInstanceState(name string, action string, force bool) error // UpdateInstanceState start/stop/restart
func (c *Client) AddDiskDevice(name, dev, source, path string, shift bool) error
func (c *Client) SetEth0Static(name, ip string) error            // UpdateInstance device override
func (c *Client) SetProxyDevices(container string, devs []ProxyDevice) error // UpdateInstance — replaces _set_forwards
func (c *Client) Exec(name string, argv []string, opts ExecOpts) (int, error) // ExecInstance; opts carry stdin bytes, env
func (c *Client) WaitIPv4(name, wantIP string, timeout time.Duration) (string, error)
func (c *Client) Publish(container, alias string) error          // golden image (slice 3)
```
Mapping of the ~100 CLI calls: `incus launch`→`Launch`; `incus config device
add`→`AddDiskDevice`; `incus config device override eth0`→`SetEth0Static`;
`incus exec … -- bash`→`Exec` with stdin; `incus stop/start/restart`→
`SetInstanceState`; `_set_forwards`/`_ensure_proxy_device`→`SetProxyDevices`;
`_state`→`InstanceState`; `_wait_for_ipv4`→`WaitIPv4`.

### 5.4 `internal/reconcile`
```go
func Reconcile(ic *incusops.Client, bp blueprint.Blueprint) error
```
Level-triggered: read live Incus state, compute the diff against `bp`, apply the
minimal changes. Replaces **four** current partial reconcilers: `_set_forwards`,
`_repair_backend_ip` (`pg-dev-local:194`), `cmd_refresh` (`:1261`), and promote's
hand-rolled rollback (`:617`). Must be idempotent and safe to run on every
`status`/`promote`/`start`.

### 5.5 `internal/task` — write-ahead journal engine (the crux)
Replaces the trap/temp-rename/`_slot_recovery_artifact` machinery.
```go
type Step struct {
    Name string
    Do   func(ctx context.Context) error
    Undo func(ctx context.Context) error // best-effort compensation
}
type Task struct {
    ID      string   // e.g. "snapshot-pg-dev-a-<name>" (deterministic, one in flight per slot)
    Steps   []Step
    Ensures []Ensure // postconditions enforced on BOTH commit and rollback (e.g. "PostgreSQL running")
    Commit  int      // index at/after which the task is durable → roll forward, else roll back
}
type Ensure struct { Name string; Run func(ctx context.Context) error }
type Journal interface {
    Begin(t Task) error          // fsync intent record to $DATA_ROOT/journal/<id>.json before any Do
    Advance(id string, step int) error
    Commit(id string) error      // delete record
    Pending() ([]Record, error)  // read on daemon start
}
func Run(ctx, j Journal, t Task) error
func Recover(ctx, j Journal, registry map[string]Task) error // called on pgdevd start + before any new mutation
```
Journal record format (`$DATA_ROOT/journal/<id>.json`, mode 0600, fsync + fsync
parent dir on write):
```json
{ "id": "...", "task": "snapshot", "args": {...}, "step": 2, "commit": 3, "startedUnix": 0 }
```
Recovery algorithm: for each pending record, if `step >= commit` → roll forward
remaining `Do`s (must be idempotent); else run `Undo` for completed steps in
reverse. Converge to before-state or after-state, **never between**. This is
what turns `_slot_recovery_artifact`'s "refuse & page human" into "auto-heal on
next start."

#### 5.5.1 Port targets — the exact scary lines to retire

This slice replaces specific, identified code. Precise ranges (all
`scripts/pg-dev-local` unless noted):

**A. Snapshot/restore reflink transaction — `658–1128` (~470 lines).**
- `_snapshot` **739–860**: cleanup closure `_snapshot_signal_cleanup` **758–791**;
  PID-suffixed temp record **748–749**; CoW clone + rename-commit **809–833**;
  hand-threaded rollback sites **834–841**, **843–854**.
- `_restore` **862–1116** (the single scariest function): cleanup closure
  **883–956**; the stdin-forced confirmation refusal **968–988** (esp. **977–986**);
  staged rename commit **1046–1050**, timeline-prune staging **1075–1099**, commit
  point **1103**; seven rollback sites **1001, 1023, 1038, 1053, 1066, 1094**,
  final cleanup **1104–1110**.
- Support: `_slot_recovery_artifact` **723–737**, `_stop_checked` **675–698**,
  `_pg_wait_ready` **658–669**, `_snapshot_records` **710–715**.

**B. Export/import transaction — `1279–1755` (~475 lines).**
- `cmd_export` **1279–~1426**; `cmd_import_last` **1427–~1755**: cleanup closure
  `_import_signal_cleanup` **1453–1473**; `_rollback_import` (relies on bash
  dynamic scoping, admitted **1537–1539**) **1540→~1650**; loop-device XFS
  validation **1498–1526**.

**C. Reflink substrate — `scripts/apple-machine-init:101–186` (~85 lines).**
- Image creation via loop staging **101–121**; mount+validate **123–138**;
  reflink probe **147–155**; online grow **161–186**; bootstrap cleanup trap
  **60–76**.

Six structural hazards concentrated in A/B, each dissolved by the engine:

| Hazard (with anchor) | Replaced by |
|---|---|
| transaction record = filesystem path existence; recovered by scanning (`723`; admitted at `1009–1011`) | explicit journal record + `Recover()` |
| cleanup closures read parent locals via dynamic scoping (`758`,`883`,`1453`,`1540`) | typed `Step{Do,Undo}` |
| ~13 hand-threaded rollback sites | one `task.Run()` unwind |
| `set +e` islands (`761`,`886`,`1455`,`1543`) | engine owns error handling |
| correctness feature that is pure compensation — restore refusal `977–986` | gone (no `exec` transport; `force` is an explicit flag) |
| untestable (side effects on live XFS + Incus + PG) | fault-injecting executor, property-tested |

#### 5.5.2 Worked example — `_snapshot` (`739–860`) as a `Task`

Conceptual, to fix the altitude of libraries + abstraction. The 66-line
trap-closure + path-bookkeeping collapses into a linear list of reversible steps;
the generic engine (above) owns journaling, rollback, and postconditions.

```go
type Snapshotter struct {
    incus *incusops.Client   // github.com/lxc/incus/client wrapper (typed API, no exec transport)
    store *store.XFS         // reflink clone + atomic dir swap
    pg    *pg.Probe          // jackc/pgx readiness
}

// Snapshot BUILDS (does not run) the transaction → pure, trivially testable.
func (s *Snapshotter) Snapshot(c backend.Container, name SnapshotName, force bool) task.Task {
    slot := s.store.SlotFor(c)
    src  := slot.Current()            // .../current
    dst  := slot.Snapshot(name)       // .../snapshots/<name>
    return task.Task{
        ID: fmt.Sprintf("snapshot:%s:%s", c, name),
        Steps: []task.Step{
            {Name: "stop-postgres",         // snapshot a quiesced data dir
             Do:   func(ctx) error { return s.pg.Stop(ctx, c) },
             Undo: func(ctx) error { return s.pg.Start(ctx, c) }},
            {Name: "stage-reflink-clone",   // cp -a --reflink=always --sparse=auto src -> staging
             Do:   func(ctx) error { return s.store.StageClone(ctx, src, dst) },
             Undo: func(ctx) error { return s.store.DiscardStaged(ctx, dst) }},
            {Name: "publish",               // the mv-target->replaced / mv-tmp->target dance, as ONE reversible op
             Do:   func(ctx) error { return s.store.PublishStaged(ctx, dst, store.Replace(force)) },
             Undo: func(ctx) error { return s.store.UnpublishStaged(ctx, dst) }},
        },
        Ensures: []task.Ensure{           // enforced on success AND rollback — mirrors bash 778–785
            {Name: "postgres-running", Run: func(ctx) error { return s.pg.EnsureRunning(ctx, c) }},
        },
    }
}

// Call site (API handler, behind the single-mutation mutex):
func (h *Handler) snapshot(ctx, req SnapshotReq) error {
    name, err := ParseSnapshotName(req.Name)   // replaces _validate_snapshot_name (regex at the boundary)
    if err != nil { return err }
    if err := h.store.Require(); err != nil { return err } // replaces _require_data_store
    return task.Run(ctx, h.journal, h.snap.Snapshot(req.Container, name, req.Force))
}
```

Library/abstraction carried by the example:

| Bash construct (`739–860`) | Replacement | Library |
|---|---|---|
| `incus exec … systemctl stop/start`, `_state` | `pg.Stop/Start`, `incus.InstanceState` | `github.com/lxc/incus/client` |
| `_pg_wait_ready` | `pg.EnsureRunning` | `jackc/pgx` |
| `cp --reflink` + `.tmp.$$`/`.replaced.$$` renames | `store.StageClone/PublishStaged/UnpublishStaged` | `internal/store` (coreutils `cp` + `os.Rename`) |
| `_snapshot_signal_cleanup`, `set +e`, `SIGNAL_CLEANUP=` | `Step.Undo` + `task.Run` unwind | `internal/task` |
| ensure-PG-up in cleanup (`778–785`) | `Ensures` | `internal/task` |
| `_slot_recovery_artifact` refusal | `Journal.Recover()` at start | `internal/task` |

Wins: rollback is composed not hand-written (no forgotten site); recovery is
automatic not a refusal; `Snapshot()` returns data, so a fake executor can crash
at step N and assert the FS converged to before/after, never between. **Caveat
(not oversold):** `StageClone`/`PublishStaged`/`Stop`/`Start` still do the real
reflink+rename+systemd work and their *idempotency* is what must be gotten right
and tested — the win is that the hard part is isolated in small named functions,
not smeared across 122 lines with a dynamically-scoped closure.

### 5.6 `internal/store` — XFS reflink store
```go
func Bootstrap(cfg) error         // truncate → losetup → mkfs.xfs -m reflink=1 → mv → mount → reflink probe → grow. Ports apple-machine-init:101-186
func SlotPaths(slot) (current, snapshots string)
func StageReflinkClone(src, dstTmp string) error // cp -a --reflink=always --sparse=auto <src> <dstTmp> (coreutils wrapped; Go owns the txn)
func SwapDirs(a, b string) error   // atomic same-fs renames (the .tmp/.replaced dance, as tested funcs)
func Snapshots(slot) ([]Snapshot, error) // creation-ordered by mtime; ports _snapshot_records:710
```
Keep the rename primitive (correct) but as library funcs the journal drives.
Wrap coreutils `cp` for the walk (battle-tested); Go owns the transaction.

### 5.7 `internal/pg`
```go
func WaitReady(ctx, connString string, timeout) error  // pg_isready equiv via pgx; ports _pg_wait_ready:658
func StopClean(ic, container string) error             // stop PG cleanly before cloning data; ports _stop_checked:675
func Provision(ctx, ic, container string, cfg) error   // apt-install PG17 (or golden image), write conf, create role+db; ports _provision_backend:275
```

### 5.8 `internal/agentapi` — the daemon API
- Transport: HTTP/JSON, bound on the machine's eth0 (reachable from host).
  Bearer token read from a `0600` file on the home-mount (`var/agent-token`);
  single-user laptop — no TLS/cert ceremony.
- Endpoints:
  | Method | Path | Body → Response |
  |---|---|---|
  | GET | `/v1/version` | → `{gitSha, apiVersion}` (handshake for `pgdev agent deploy`) |
  | GET | `/v1/healthz` | → `{ok}` |
  | GET | `/v1/status` | → per-backend state/endpoints, roles, snapshot counts & timelines (ports `cmd_status:526`) |
  | GET | `/v1/snapshots?slot=` | → `[]Snapshot` |
  | POST | `/v1/promote` | → new active slot + status |
  | POST | `/v1/snapshot` | `{slot, name, force}` → result |
  | POST | `/v1/restore` | `{slot, name|--last, force}` → result (no interactive confirm; `force` is explicit) |
  | POST | `/v1/reconcile` | → applied diff |
  | POST | `/v1/export` | → tarball path under `var/` |
  | POST | `/v1/import` | `{archive}` → result |
- Server enforces the domain invariants centrally: the slot state machine and
  the single-mutation mutex (today's `flock` on `/run/lock/pg-dev-local.lock`).

**Two transports, split by shape of work (the appliance reframe).** The core
idea is that the machine stops being "a shell you inject scripts into over Apple's
broken `container exec`" and becomes "a service you send requests to." Two
resident endpoints replace that pipe:
- *Stateful control* (promote/snapshot/restore/reconcile/export/import) → **this
  HTTP/JSON API.** Needs types, the journal (§5.5), the mutex, boot-time recovery.
- *Interactive + file transfer* (`psql`, container `shell`, `logs`, "get a dump
  into staging") → **SSH**, via a resident `sshd` or **`ssh2incus`**
  (`github.com/mobydeck/ssh2incus`) in the machine. SSH already nails PTYs,
  signals, stdin, and SCP/SFTP; doing them over HTTP would reinvent SSH badly.

`ssh2incus` **complements** this design as the interactive transport — it does
**not** substitute for it. It cures one of the three hostile interfaces (broken
`exec`, for the instance-access subset) but leaves the snapshot state machine
(§5.5) and the IP drift (§5.9) untouched, and it moves the pipe without reducing
the shell. Note it inherits the endpoint problem: reachable only at the drifting
machine IP (port 2222), discovered/forwarded the same way as 5442/5443.

### 5.9 `internal/forward` — host endpoint (kills socat)
- `pgdev endpoint serve`: in-process Go forwarder, `io.Copy` pairs, one listener
  per client port (`5442`/`5443` → machine `5432`/`5433`). Run by launchd.
- `pgdev endpoint install`: generate the launchd plist (ports `_write_plist`) —
  `ProgramArguments = [pgdev, endpoint, serve]`, `KeepAlive`, `RunAtLoad`.
- Drift made to **disappear**, not chased: (1) `pgdevd` **pushes** its eth0 IP
  to `var/machine-ip` on boot/address-change; (2) forwarder **re-resolves on
  dial failure** (read file, fall back to `container machine run … ip addr`) and
  retries. The stale-IP class of bug is structurally gone. No mDNS.

### 5.10 `internal/applecli`
Only place `container` is invoked: `MachineCreate/Run/Set/Stop/Delete`,
`ImageBuild`, `MachineIP`. Everything from `Makefile:16-113` and
`host-endpoint:_machine_ip`.

### 5.11 `cmd/pgdev` (cobra) & `cmd/pgdevd`
- `pgdev`: `up`, `down`, `status`, `promote`, `snapshot --name --force`,
  `restore --name|--last --force`, `snapshots`, `staging {psql,shell,logs,
  snapshot,restore,restore-last,reset,start,stop}`, `psql`, `shell`, `logs`,
  `ip`, `refresh`, `export`, `import-last`, `endpoint {install,serve,refresh,
  uninstall,status}`, `agent deploy`, `machine {shell,status,...}`.
  - `--force` replaces `force=1` make-var; `term.IsTerminal` replaces
    `PG_DEV_AUTO`; `pgdev psql` runs **local** psql against `127.0.0.1:5442`
    (no exec transport).
- `pgdevd`: `serve` (HTTP), `bootstrap` (systemd `ExecStartPre` → `store.Bootstrap`
  + `reconcile` daemon topology, ports `apple-machine-init`), runs
  `task.Recover` on start.
**Hot-deploy into the RUNNING machine (hard requirement — no image rebuild per
dev cycle).** `pgdev agent deploy`:
1. Cross-compile on the Mac: `GOOS=linux GOARCH=arm64 go build ./cmd/pgdevd` →
   `var/pgdevd.new`. No Go toolchain in the machine; this is why the image stays
   minimal.
2. **Deliver via the home-mount** (repo `var/` is visible in-machine at the same
   path) — no `container cp`, no image layer.
3. **Atomic install to a machine-local run path:** `mv var/pgdevd.new
   /usr/local/bin/pgdevd` (rename, never write-in-place → avoids `ETXTBSY` on the
   live binary; the running process keeps its old inode until restart). Run from
   the machine-local copy, *not* the home-mount, so the unit survives a boot
   where the home-mount isn't up yet — the mount is the delivery channel, not the
   run location.
4. **Restart:** one fire-and-forget `systemctl restart pgdevd` (a one-shot
   control exec Apple's transport handles fine; routed over SSH once §5.8 is up).
5. **Confirm:** poll `GET /v1/version` until the embedded git-sha matches, with a
   timeout — `deploy` fails loudly on a stale daemon instead of silently running
   old code.

Image rebuild (`Dockerfile.machine` / `container machine create`) is reserved for
**base changes only** (systemd/incus/xfs tooling, the unit file) or shipping a
blessed release (bake `pgdevd` + unit into the image). The inner dev loop never
touches `container machine create/recreate`. *(Pure-dev shortcut: point
`ExecStart` at the home-mount path to skip step 3 — faster loop, but the daemon
won't start after a reboot without the mount; keep the copy by default.)*

---

## 6. Old shell → new code map (for incremental porting)

| Shell (file:sym) | New home |
|---|---|
| `pg-dev-local` `_set_active`/`_active`/`_staging` | `blueprint` slot state + `var/active-slot` |
| `_set_forwards`, `_ensure_proxy_device`, `_repair_backend_ip`, `cmd_refresh`, promote rollback | `reconcile.Reconcile` |
| `cmd_promote:592` | API `/v1/promote` = `SetActive` + `Reconcile` |
| `_provision_backend:275`, `_provision_proxy:370`, `cmd_up:382` | `pg.Provision` + `reconcile` + `/v1/…`; golden image (slice 3) |
| `_snapshot:739`, `_restore:862`, `_restore_last:1118`, `cmd_snapshot/restore*` | `task` engine + `store` |
| `_slot_recovery_artifact:723` | `task.Recover` (auto-heal) |
| `cmd_status:526`, `_render_table:455`, `cmd_endpoint:564`, `cmd_ip:1227` | `/v1/status` + `pgdev` render |
| `cmd_export:1279`, `cmd_import_last:1427` | journaled tasks (slice 5) |
| `apple-machine-init` (whole) | `store.Bootstrap` + `reconcile` daemon topology (`pgdevd bootstrap`) |
| `host-endpoint` (whole) | `internal/forward` + `pgdev endpoint` |
| `Makefile` `PG_DEV_AUTO`, `name=`/`force=` | cobra flags + `term.IsTerminal` |

---

## 7. Config & compatibility

- `.env` unchanged; all vars in §3 keep their names/defaults.
- Client contract unchanged: `127.0.0.1:5442`/`:5443`, same `.pgpass` lines.
- During migration a ~20-line alias `Makefile` keeps `make pg.*` targets working
  (`pg.promote: ; pgdev promote`), deleted at the end.

---

## 8. Migration slices (strangler; each ships independently)

**Slice 1 — Proof (highest risk first).** `internal/task` + `internal/store` +
`internal/pg.StopClean`. Existing `pg-dev-local` execs the new linux binary
wholesale for `snapshot|restore|restore-last|staging.{snapshot,restore,reset}`.
*Accept:* `make pg.snapshot`/`pg.restore`/`pg.snapshots` behave identically;
killing the binary mid-restore auto-heals on rerun; fault-injection tests pass
in CI; XFS integration test runs on GH Actions Ubuntu (`losetup`+`mkfs.xfs`).

**Slice 2 — Daemonize.** `pgdevd` + systemd unit + `agentapi`; `blueprint` +
`reconcile`; `pgdev` does `status`/`promote`/`refresh`/`snapshots` via API.
*Accept:* Apple `exec` gone from the control path; `pgdev promote` flips roles;
`make pg.ip` still correct; `agent deploy` version handshake works.

**Slice 3 — Provisioning.** `up`/`down` → reconciler + `pg.Provision`; absorb
`apple-machine-init` into `pgdevd bootstrap`. Golden `pg-dev-base` image via
`incus publish` (removes curl|gpg heredoc, minutes → seconds). *Accept:*
`make pg.up`/`pg.down` from empty Incus host reproduce today's result.

- **Action — boot-ordering hardening (make PG depend on its storage, both
  levels).** Today the runtime code (Incus adapter) papers over storage-not-ready
  races with wait/retry/restart loops. Fix it at the source in provisioning:
  1. *Container level:* bake a drop-in on `postgresql@17-main`,
     `RequiresMountsFor=/var/lib/postgresql`, so PostgreSQL waits for the Incus
     idmapped disk-device mount to appear (systemd tracks it as a passive
     `var-lib-postgresql.mount`; the packaged unit doesn't declare this). Closes
     the "PG auto-starts before the data dir is mounted" boot race.
  2. *Outer-machine level:* order `incus.service`
     `After=/var/lib/pg-dev-local` (its XFS loop mount) so a bare machine reboot
     can't start `incusd` before the backends' disk-device *sources* exist. The
     bigger reboot-safety gap; today only `apple-machine-init` enforces this
     order imperatively.
  Once trusted, the adapter's `waitSystemd`/nudge/container-restart recovery in
  `StartContainerAndWait` can be simplified to a plain readiness wait.
  *Accept:* container reboot (`incus restart pg-dev-a`) brings PG up with no
  adapter nudging; outer-machine reboot leaves a working setup without `make start`.

**Slice 4 — Endpoint.** `internal/forward` + `pgdev endpoint`; delete
`scripts/host-endpoint`. *Accept:* reboot-drift test — restart machine, forwarder
self-heals to the new IP with no `make` step.

**Slice 5 — Export/import.** Port move-aside rollback (`cmd_export`,
`cmd_import_last`) as journaled tasks; delete `scripts/pg-dev-local`; Makefile →
aliases/gone. *Accept:* `make pg.export` then `make recreate && make
pg.import-last` round-trips with rollback on failure.

---

## 9. Testing

- **Property/fault-injection (CI):** `task` executor that fails/"crashes" at every
  step index N; assert convergence to before/after, never between.
- **XFS integration (GH Actions Ubuntu):** real `losetup` + `mkfs.xfs -m
  reflink=1`; exercise `StageReflinkClone`/`SwapDirs`/`Snapshots`.
- **E2E on Mac per slice:** the acceptance checks in §8, plus the README
  day-to-day flow (reset staging → `pg_restore` → snapshot → promote) at every
  slice boundary.
- **Gate:** `make check` extended with `go vet` + `go test ./...`.

---

## 10. Rejected options (and why)

Terraform/OpenTofu (promote is by-design drift → split-brain); k3s/Kubernetes
(wrong scale ×1000); Python (no static binary — ships a runtime into the
machine); Ansible (still rides the broken exec transport, larger/slower than a
~200-line reconciler); gRPC/protobuf (single consumer → stdlib JSON/HTTP);
`just`/`Taskfile` as a logic layer (shell in a nicer coat, untyped); mDNS/Bonjour
(the home-mount IP file is deterministic; multicast over vmnet isn't).

---

## 11. Appendix — Option Zero (evict Incus; spike, don't bet)

Incus mostly apt-installs PG into a shell around a data dir and owns two proxy
devices. The data layout `/var/lib/pg-dev-local/{a,b}/{current,snapshots}` is
already container-independent. Option Zero: PG17 in the machine image + two
systemd-templated clusters on the XFS store + `pgdevd` as the active/staging TCP
proxy → **~100 Incus calls → 0**, no IP pinning, no proxy container, no
`incusbr0`. Cost: machine image becomes PG-version-coupled (loses cheap
container re-provisioning on PG upgrades) and `pgdevd` gains a data-plane duty.
Kept a drop-in future by drawing `internal/backend.Backend` around **"slot"**
(`StopPG/StartPG/EnsureRoute/Exec`), not "container", from Slice 1 onward.

---

## 12. The load-bearing insight

The 1,837 lines are long because this is a distributed system glued through a
broken pipe; half the code compensates for three hostile interfaces. Give the
system real interfaces — one resident API, one journal, one reconciler — and
most of that code stops needing to exist. Go is the vehicle; the interfaces are
the point.
