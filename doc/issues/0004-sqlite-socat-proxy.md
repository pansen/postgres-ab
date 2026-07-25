# Spec 0004 — SQLite machine tracking + socat client proxy (retire the Go forwarder)

Status: **in progress** (2026-07-24) · Owner: andi · Relates to:
`0002-two-machine-disk-reclaim.md` (two-machine model),
`0003-internal-forward.md` (the Go forwarder this proposes to retire). Memory:
`forwarder-local-network-privacy`, `pgdevd-token-home-mount-cold-cache`,
`apple-apiserver-wedged-recovery`.

This is a **living document**: it records the design as it is built and tracks
the eventual removal of the Go forwarder. Keep it current as the trial proceeds.

---

## 1. Why (course correction from 0003)

Spec 0003 replaced the shell `socat` relay with an in-process Go forwarder
(`internal/forward`, on 127.0.0.1:5442/:5443) to kill socat's process-lifecycle
bugs. It works — but its **own binary** trips macOS Local Network Privacy: it
needs a stable codesign identity plus a manual Local Network grant, and STILL
throws occasional permission prompts (`make build` strips the identifier; see
memory `forwarder-local-network-privacy`). Homebrew's `socat` — an already-known,
already-trusted binary — does **not** hit this.

So we re-introduce a socat path **as an experiment**, but tame the lifecycle
races that got it retired, using:

- **SQLite** (`var/pgdev.db`) as the single source of truth for machine
  tracking, so a reconcile is one atomic, transactional decision instead of a
  scatter of racing `os.WriteFile`s.
- an explicit **port-free gate + post-verify** around every socat reload — the
  actual fix for the orphan-listener → EADDRINUSE → silent-stale-mapping bug
  (SQLite alone does NOT fix that; it only serializes reconcilers).

The socat proxy takes over the **canonical client ports** (5442 active / 5443
staging), so existing external configs pick it up with no change; the Go
forwarder is **moved to a second pair** (5444 / 5445) and runs alongside it, so
the two can be compared before the forwarder is removed.

## 2. What shipped in this change

### 2.1 `internal/track` — SQLite machine tracking (source of truth)

`var/pgdev.db` (modernc.org/sqlite, **pure Go, no cgo**, WAL, busy_timeout).
Tables:

- `schema_meta(version)` — schema version.
- `active(id=1, slot)` — which machine is active (replaces `var/active-machine`).
- `machine(slot, ip, updated_unix)` — cached drifting eth0 IPs (replaces
  `var/machine-ip-{a,b}`).
- `proxy_target(role, port, target, updated_unix)` — the reconciled socat
  upstreams, **observability only** (see §3).

**Schema-change automation instead of migration tooling.** On `Open`, if the
stored version ≠ `schemaVersion`, `track` drops every table and recreates the
schema (all rows are reconstructible — IPs re-discover). The one decision that
is NOT a cache, the **active slot**, is carried across the reset (old table →
legacy `var/active-machine` file → `"a"`, and only the last is loud). `Open`
never touches launchd; it returns a `reset` bool and the caller reconciles.

**Chokepoint + dual-write.** Every state write goes through `track`, which writes
the DB first and then **mirrors the legacy flat file** the still-running Go
forwarder polls. This is deliberate: rebuilding the Go forwarder to read SQLite
would re-trigger the exact codesign/TCC ceremony we are trying to escape, so we
keep its binary byte-identical during the trial. The mirrors die with the
forwarder (§5).

Structured logging throughout via stdlib **`log/slog`** (text handler → stderr,
`component=track`). `PG_PROXY_DEBUG=1` (the default) makes it chatty.

### 2.2 `internal/socatproxy` — the socat LaunchAgents

Two per-user LaunchAgents, `me.pansen.<prefix>-socat-active` (:5442) and
`-socat-staging` (:5443) — the **canonical client ports**. Each runs:

```
socat -d -d \
  TCP-LISTEN:5442,bind=127.0.0.1,reuseaddr,fork,keepalive,nodelay \
  TCP:<ip>:5432,connect-timeout=5,keepalive,nodelay
```

`connect-timeout` is essential — without it a drifted/stale target hangs psql
~75s per connection. `-d -d` logs connection lifecycle to `var/<prefix>-socat-<role>.log`.

socat cannot re-point itself, so each reconcile **rewrites and reloads** the
plist. The reload is the hardened sequence (this is the anti-orphan core):

1. `launchctl bootout`, **poll until launchd reports it gone** (children exit).
2. **Port-free gate**: attempt `net.Listen` on the port; poll. If still held,
   `lsof` the LISTEN pid and kill it (an ERROR — launchd leaked), re-probe.
   A port that never frees is a **hard failure** — refuse to bootstrap onto a
   held port (which is exactly how the stale mapping used to persist).
3. Write plist (atomic), `launchctl bootstrap` (retry transient EIO).
4. **Post-verify**: poll until the port is LISTENing AND the live socat's argv
   contains the intended target (`lsof` → pid → `ps`). Because socat's parent
   never dials, this argv check is the ONLY stale-mapping detector, so it is
   mandatory; a timeout fails the reconcile loudly.

### 2.3 Reconcile flow — locking discipline

The reconcile is guarded by an **flock** (`var/reconcile.flock`), NOT by holding a
SQLite transaction across `launchctl`. Rationale: a wedged `launchctl` (a known
macOS fragility on this host — see `apple-apiserver-wedged-recovery`) holding a
DB write lock would brick every other `pgdev` command. So:

- **flock** = the reconcile mutex (dies with the process; no stale-lock recovery).
- **Tx 1 (ms)**: `DesiredTargets` reads active+IPs in one snapshot → can't mix a
  pre-promote slot with post-promote IPs.
- Apply to launchd (outside any tx), comparing desired against the **on-disk
  plist** (the durable truth of what launchd runs), not the DB — so a crash
  between apply and record self-heals next pass.
- **Tx 2 (ms)**: `RecordApplied` stamps `proxy_target` for `proxy status`.

Triggered from: `pgdev proxy reconcile`/`install`, and automatically (only if the
proxy is already installed) after `promote` and `refresh`.

### 2.4 CLI + Make

- `pgdev proxy install | reconcile | status | uninstall`
- `make proxy.install | proxy.reconcile | proxy.status | proxy.uninstall`

`proxy install` is the **single reference command for "install anything proxy"**:
it brings up BOTH the socat proxy (canonical 5442/5443) and the Go forwarder
(5444/5445, best-effort, honoring `PG_ENDPOINT_AUTOINSTALL=0`), so there is no
separate `endpoint.install` to remember.

It also **frees the canonical ports as part of install**: step 1 does a FULL
(re)install of the Go forwarder (bootout + bootstrap), which boots any pre-swap
forwarder off 5442/5443 (where a process that started before the swap is still
bound, and which a gentle `Ensure` would leave alone since its argv is unchanged)
and brings it back on 5444/5445 — so socat can bind the canonical ports in step
2. Killing that forwarder's PID would not suffice: it has `KeepAlive`, so launchd
resurrects it; only booting out the *job* frees the port. (A genuinely foreign
holder still hard-fails the gate loudly — we do not kill arbitrary processes.)

Opt-in: `promote`/`refresh` stay hands-off (never touch launchd) until
`proxy install` has created a plist (`Reconciler.AnyInstalled`).

## 3. Deliberate decisions / non-goals

- **`proxy_target` is observability, not authority.** The durable record of what
  socat runs is the plist; decisions compare against it.
- **`status` does not surface socat** (per the brief — still an experiment).
- **Data loss on schema change is accepted** (IPs re-discover; active carried).
- **Host-only DB.** Never open `var/pgdev.db` from inside a guest over virtiofs
  — SQLite over virtiofs corrupts (cf. the token cold-cache burn in 0002).

## 4. Open questions / to validate during the trial

- [ ] Does socat under a LaunchAgent avoid the Local Network prompt in practice,
      or does TCC attribute the connection to the launchd job's binary anyway?
      **This is the whole hypothesis — validate first.** Because socat now holds
      the canonical 5442/5443, `make proxy.install` puts it on the client path
      directly — running any existing client against 5442/5443 exercises it.
- [ ] Since socat serves the canonical ports, `make endpoint.install` (the Go
      forwarder, now 5444/5445) and `make proxy.install` (socat, 5442/5443) can
      both run; make sure they never both try to bind the same port.
- [ ] Behaviour of an in-flight `pg_restore` when a reload kills the socat
      children mid-transfer (expected: dropped; client reconnects). Acceptable
      for dev; confirm.

## 5. Removal plan for the Go forwarder (the end state)

Socat already holds the canonical client ports, so removal is now purely a
deletion — no client repoint needed:

1. Delete `internal/forward` and `pgdev forward *` + the `endpoint.*` make
   targets that wrap it (and free `PG_FORWARD_ACTIVE_PORT`/`STAGING_PORT`).
2. Delete the **legacy flat-file mirrors** in `internal/track` (`ActiveMirror`,
   `IPMirror`) and `activeslot` file reads — the DB becomes the sole store, so
   "no plain text/JSON tracking anymore" is fully realized (`var/active-machine`,
   `var/machine-ip-{a,b}`, `var/forward-state.json` all deleted).
3. Move `pgdev status`'s forwarder section onto the socat proxy.
4. Update README + `.env.example` (drop `PG_FORWARD_*`, keep `PG_PROXY_*`).

Until then, both paths coexist and the mirrors keep the Go forwarder correct.
