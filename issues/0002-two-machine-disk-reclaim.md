# Spec 0002 — Two Apple machines: make `reset` reclaim macOS disk

Status: **adopted, scope refined** (2026-07-22) · Owner: andi · Branch:
`chore/andi/separate_machines` · Relates to:
`issues/0001-pgdev-de-shelling-spec.md`, memory `apple-vm-disk-trap`,
`apple-container-cli-quirks`.

This started as a design proposal (§1–§8 below, kept for rationale). It is now
**adopted with a deliberately minimal scope** — see **§0 Decisions & current
state** for what a new session needs, then read §1–§4 for the "why".

---

## 0. Decisions & current state (read this first)

### 0.1 Adopted scope — minimal, conservative, NOT a topology rewrite

Everything that exists today stays as-is in behavior: **Incus per backend**, the
reflink snapshot/restore engine (`store`/`ops`/`task`/`pg` packages), `promote`
semantics, and the client contract (`127.0.0.1:5442` active / `:5443` staging)
are all **unchanged**.

- **The only structural change:** each backend moves from sharing one machine
  (`vpg`, today: two backends `pg-dev-a`/`pg-dev-b` + a `pg-proxy` container in
  one machine) to **owning its own Apple machine** — `vpg-a` hosts backend `a`,
  `vpg-b` hosts backend `b`, each with its own Incus daemon + one PG backend on
  its own XFS reflink store.
- **The only new behavior:** `make pg.staging.rebuild` = delete + recreate **only
  the staging machine** (reclaiming its grown sparse `vdb` on macOS), then
  re-provision that one backend. **The active machine is never touched.** Today
  the only way to reclaim the sparse disk is `make recreate`, which nukes *both*
  databases and loses the A/B benefit; separation lets you reclaim staging alone
  while active keeps running.
- **Physical consequences of separation that DO force change:** the in-machine
  `pg-proxy` fronting both backends can't span two VMs, so active/staging routing
  moves to the **host forwarder** (`:5442`→active machine, `:5443`→staging
  machine); `promote` becomes a **host pointer-flip + forwarder re-point**; each
  machine exposes its one backend on its own `eth0`.

### 0.2 Open decisions from §7 — resolved

1. **Symmetric-alternating** (both machines swap roles on promote, both
   periodically emptied). Not the asymmetric one-ephemeral-staging variant.
2. **Keep in-machine reflink snapshots** — the Slice 1–3 engine is kept per
   machine; soft reset (reflink) stays the day-to-day default, hard reset
   (machine recreate) is the reclaim tier.
3. Snapshot-store location (home-mount vs accept-loss-on-nuke): **still open**,
   low priority — a hard reset intentionally discards staging snapshots.
4. **Per-machine memory is configurable** (`MACHINE_MEMORY` today is `12G`; two
   machines must each drop, e.g. 6–8G). The 2× RAM concern in §4 is **dismissed**
   as a temporary loading-phase cost.
5. Golden image (keep per-machine vs bake PG into `Dockerfile.machine`): **still
   open**, decide during the daemon slice.
6. **Concurrency smoke test: this was Slice 0 — PASSED 2026-07-22** (see §0.3).
7. Sequencing: adopt now; the host forwarder (Slice 4 of 0001) is pulled in
   because routing must move host-side.

Build with best practices, tests alongside code, and cheaper subagents where
feasible; a Fable subagent serves as architect/second-opinion.

### 0.3 Implementation state & next step

- **Slice 0 — concurrency smoke test (the gate before any refactor). ✅ PASSED
  2026-07-22.** Proved the load-bearing assumption: deleting/recreating **one**
  Apple machine must not disturb a **sibling** machine nor wedge the shared
  apiserver (Apple 1.1's control plane is fragile — see
  `apple-container-cli-quirks`). `scripts/two-machine-smoke.sh` ran ITERS=5:
  `vpg-smoke-a` survived all 5 delete/recreate cycles of `vpg-smoke-b`, the
  apiserver stayed responsive throughout, both throwaways were cleaned up, and the
  real `vpg` (stopped) was left untouched. Free space returned to baseline (62
  GiB). The script refuses to touch any non-`vpg-smoke-*` name, so it can never
  delete a real `vpg`. Still **untracked** in git.

  Recovery note: getting here first required a userspace reboot — the Apple
  apiserver was wedged with an orphaned launchd Mach endpoint (`launchctl
  bootstrap` → EIO); only a reboot clears it (storage survives). See memory
  `apple-apiserver-wedged-recovery`. Also fixed a latent smoke-script bug: the
  "vpg untouched" assertion hard-coded `*running*` but the real `vpg` is
  `stopped`, so it now compares to the recorded baseline state instead.

  **To re-run it:**
  ```
  make machine.image          # starts the apiserver + builds local/pg-incus-machine:26.04
  ./scripts/two-machine-smoke.sh   # ITERS=5 by default; SMOKE_CPUS/SMOKE_MEMORY overridable
  ```

- **Slices 1+ (in progress):** the code changes sketched in §5 — `internal/config`
  two machine names, shrink `blueprint`/`reconcile`, `applecli` machine lifecycle,
  host forwarder tracking two drifting IPs, one-backend-per-`pgdevd`, and the
  `cmd/pgdev` promote/rebuild wiring. A Fable architect is producing the detailed
  slice-by-slice plan; the existing `vpg` machine + its data must keep working
  during migration (`vpg-a`/`vpg-b` coexist with `vpg` during dev — do not purge
  `vpg` prematurely).

---

## 1. The problem it solves

The Apple `container` machine's root disk (`/dev/vdb`) is a **sparse image on
macOS that only ever grows**. Blocks written inside the guest are added to the
macOS-side image and are **not returned to macOS when freed** — Apple's runtime
does not compact on discard/TRIM, and there is no supported per-machine size cap
or compaction (see `apple-vm-disk-trap`). Measured 2026-07-21:

- `fstrim -v /` in the guest "trimmed 261.5 GiB" → macOS reclaimed **~6 GiB**.
- `container machine delete vpg` → macOS reclaimed **~239 GiB** (16 → 255 GiB free).

So a large `pg_restore` into staging grows `vdb`, ratchets the Mac's free space
down, and — because deleting the data never gives it back — eventually a guest
write fails and the loop-backed XFS store shuts down mid-write (EIO), dropping
PostgreSQL into recovery. Slice-3's `make disk.check` guards *against starting*
such a restore, but it does not *reclaim* anything.

**The load-bearing fact:** the only reliable macOS reclaim is **deleting the
whole machine**. Recreating the `.xfs` store *inside* a machine does not shrink
`vdb`. Therefore, to make "reset" reclaim space, the disposable unit must be an
**Apple machine**, not a directory or a loop image inside one.

---

## 2. The proposal

Run **two Apple `container` machines** (e.g. `vpg-a`, `vpg-b`), each with its own
Incus daemon and a **single** PostgreSQL backend on its own XFS store. `pgdev`
(host) already acts as a client (HTTP/JSON to `pgdevd`); it now talks to two
daemons and owns the active/staging role at the host level.

```
macOS client ─ 127.0.0.1:5442 (active) / :5443 (staging)
                     │
             host forwarder (internal/forward)
              │                         │
     vpg-a  (machine IP:5432)    vpg-b (machine IP:5432)
       Incus + 1 PG backend        Incus + 1 PG backend
       own XFS reflink store        own XFS reflink store
```

> **Note:** the host forwarder (`internal/forward`) does not exist yet — it is
> Slice 4 of `issues/0001` and is unbuilt. Today the client ports are served by
> `make endpoint.install` (a loopback forwarder) and `promote` re-points the
> *in-machine* Incus proxy devices (`internal/daemon` + `internal/reconcile`).
> The diagram and the `promote` bullet below describe the proposed target state,
> not the current code.

- **Active pointer** (host-side, `var/active-machine`) decides which machine is
  active (behind `:5442`) and which is staging (`:5443`).
- **`promote`** = flip the pointer and re-point the two host-forwarder targets.
  No data moves; no in-machine proxy device. (Client contract unchanged:
  `127.0.0.1:5442/:5443`.)
- **Two reset tiers:**
  - *soft reset* — reflink-restore the staging backend to a snapshot **inside**
    its machine (instant; the Slice 1–3 engine, unchanged). Does not reclaim Mac
    space.
  - *hard reset* — host **deletes and recreates the staging machine**, then
    re-provisions a fresh backend. Slow (machine boot + provision), but **frees
    that machine's entire sparse image on macOS**.
- **Steady state:** the usual loop is `hard-reset staging → pg_restore → verify →
  promote`. Because roles alternate on promote, whichever machine becomes staging
  is hard-reset before the next import — so **both machines are periodically
  emptied**, bounding total growth to ≈ 2× live data instead of ratcheting to
  full.

---

## 3. Why this is attractive

- **It actually reclaims.** The dominant bloat source (importing a fresh prod
  dump into staging each cycle) now ends with a machine-recreate that returns the
  space to macOS. Unbounded growth becomes bounded.
- **It simplifies the topology.** One backend per machine removes: the `pg-proxy`
  container, the two Incus `proxy` devices, the `incusbr0` `.11/.12` static-IP
  pinning, and most of `internal/reconcile` + `internal/blueprint`. `promote`
  collapses to "swap two forwarder targets" on the host.
- **It can remove the golden-image step.** With exactly one backend per machine,
  PostgreSQL can be baked into the **machine image** (`Dockerfile.machine`), so
  provisioning is "attach store + create cluster + role/db" — no
  `incus publish` / golden-container dance (Slice-3 §Golden becomes moot).
- **Snapshots on the home-mount are reclaimable.** If the snapshot store lives on
  the home-mount (macOS APFS, persistent across a machine nuke), its files are
  ordinary files you can delete to free Mac space directly — no sparse-file trap
  for that data.

---

## 4. What it costs (honest accounting)

- **2× base overhead.** Two machines each reserve CPU/RAM and carry their own
  systemd + Incus + rootfs + image store. `MACHINE_MEMORY=12G × 2` is heavy; each
  machine hosts one PG + Incus, so per-machine sizing must drop (e.g. 6–8 GB).
- **Hard reset is not instant.** It re-provisions (machine boot + cluster create,
  ~40–90 s) before the import. The import itself dominates cost, so this is
  usually acceptable — but soft reset (reflink) must remain the default for
  day-to-day.
- **Snapshot timelines don't survive a nuke.** A hard reset discards the staging
  machine's in-machine reflink snapshots. Fine for "reset to clean before
  import," but the two tiers must be explicit so nobody hard-resets away a
  checkpoint they wanted. (Mitigation: persist snapshots on the home-mount.)
- **Two drifting IPs.** Each machine's eth0 lease drifts independently
  (`apple-machine-ip-unstable`); the forwarder must discover and track both
  (extends Slice 4).
- **Host-orchestrated lifecycle.** A daemon cannot delete/recreate its own Apple
  machine, so hard-reset, promote-forwarding, and deploy are host operations via
  `internal/applecli` — which inherits Apple's flaky exec/XPC behavior
  (`apple-container-cli-quirks`).
- **Unproven: two concurrent machines.** Apple 1.1's exec transport is fragile
  (concurrent-exec has taken a VM down before). HTTP to two `pgdevd`s is fine;
  concurrent *lifecycle* execs against two machines should be serialized and
  needs a smoke test before committing.

---

## 5. Impact on the current codebase (sketch, if adopted)

| Area | Change |
|---|---|
| `internal/config` | Two machine names; per-machine ports/token; host-side active-*machine* pointer; smaller default memory. |
| `internal/blueprint` + `internal/reconcile` | Shrink drastically — no proxy container, no proxy devices, no `incusbr0` pinning. Reconcile ≈ "ensure the one backend + its store." |
| `internal/applecli` | Machine lifecycle for two machines: create/delete/recreate/boot; used by hard-reset. |
| `internal/forward` (Slice 4) | Track **two** drifting machine IPs; `promote` re-points which machine each client port maps to. |
| `internal/daemon` (`pgdevd`) | One backend per instance; `Up`/`Down` per machine; no proxy provisioning. Golden-image build likely deleted (bake PG into the image). |
| `cmd/pgdev` | `promote` = host pointer flip + forwarder re-point; `staging.reset` grows a `--hard` (recreate machine) vs default soft (reflink). `up`/`down`/`status`/`deploy` fan out over both machines. |
| `Dockerfile.machine` | Optionally bake PostgreSQL 17 + the boot-ordering drop-in, removing the golden image. |

Net: the snapshot/restore engine (Slices 1–3) is **kept, per machine**; the
Incus proxy/networking machinery (much of Slices 2–4) **shrinks or disappears**.

---

## 6. Alternatives considered

- **A. Status quo — single machine + `disk.check` guard + periodic `make
  recreate`.** Zero new complexity; the guard prevents corruption. But reclaim is
  all-or-nothing (nuke everything, lose both slots) and manual. Doesn't bound
  growth, just warns.
- **B. Relocate the `.xfs` store to a host-path / quota'd APFS volume.** Put the
  loop image on macOS APFS (a real file you can `rm` to reclaim) or on a
  `diskutil apfs addVolume … -quota` volume that caps consumption. Directly
  reclaimable and caps growth — but loop-mounting a file over virtiofs is of
  uncertain performance/correctness, and reflink requires the loop'd XFS anyway.
  Worth a spike; could combine with the single-machine model.
- **C. Recreate only the `.xfs` image inside one machine on hard-reset.** Does
  **not** work — freeing blocks inside `vdb` never shrinks the macOS image. (This
  is exactly why the disposable unit must be the machine.)
- **D. Wait for Apple to support discard/compaction** (upstream apple/container
  #518). Not actionable on our timeline.

---

## 7. Open decisions (resolve before slicing)

> **Resolved 2026-07-22 — see §0.2.** Kept here as the original framing. Short
> version: symmetric-alternating; keep reflink snapshots; per-machine memory
> configurable; concurrency smoke test is Slice 0 (in progress). Items 3 (snapshot
> store location) and 5 (golden image) remain open, both low-priority.

1. **Symmetric-alternating vs asymmetric** roles: two machines that swap on
   promote (both periodically emptied) — or one long-lived active + one ephemeral
   staging recreated per import (simpler, but promote semantics differ)?
2. **Do we keep in-machine reflink snapshots at all**, or is a slot just "current
   data + hard-reset"? (Keeping them preserves cheap intra-cycle rollback.)
3. **Where does the snapshot store live** so it survives a nuke — home-mount
   (reclaimable APFS) or accept loss on hard-reset?
4. **Per-machine resource sizing** that fits two machines on the target Mac.
5. **Golden image**: keep per-machine, or bake PG into `Dockerfile.machine`.
6. **Concurrency smoke test**: two machines up + interleaved lifecycle execs —
   does Apple 1.1 stay stable?
7. **Sequencing vs Slices 4–5**: this partly supersedes them (no proxy →
   forwarder tracks two IPs; the `pg_restore`-into-staging loop gains a "reclaim
   by recreate" path). Adopt now and re-scope 4–5, or finish 4–5 on one machine
   first?

---

## 8. Recommendation

Adopt the **two-tier reset** framing: keep the single-machine reflink engine for
everyday work, and add machine-level hard-reset for reclaim. Whether that lands
as *two permanent alternating machines* (§2) or *one active + one ephemeral
staging* (§7.1) is the first decision to make; both realize the core win
(reclaim = machine delete). Before committing, spike **B** (store on a quota'd
APFS host volume) — if a loop-over-APFS store is viable, it delivers reclaim +
cap **without** the 2× machine overhead, and may be the better answer. Do a
two-machine concurrency smoke test either way.
