# Spec 0004 — SQLite machine tracking + socat client proxy (retire the Go forwarder)

Status: **done** (2026-07-25) · Owner: andi · Relates to:
`0002-two-machine-disk-reclaim.md` (two-machine model) and spec 0003, the Go
forwarder this retired (its file was deleted with the code; see git history).
Memory: `pgdevd-token-home-mount-cold-cache`, `apple-apiserver-wedged-recovery`.

This is a **living document**: it records the design as it was built and what
remains. The Go forwarder and its codesign ceremony are now **removed** (§5).

---

## 1. Why (course correction from 0003)

Spec 0003 replaced the shell `socat` relay with an in-process Go forwarder
(`internal/forward`, on 127.0.0.1:5442/:5443) to kill socat's process-lifecycle
bugs. It works — but its **own binary** trips macOS Local Network Privacy: it
needs a stable codesign identity plus a manual Local Network grant, and STILL
throws permission prompts on rebuilds (`make` re-signs the running agent's
executable). Homebrew's `socat` needs the Local Network grant too — TCC gates the
subnet, not the program — but it is ONE stable, already-signed binary at a fixed
path that we never rebuild, so the grant is asked **once** and then sticks.

So we re-introduce a socat path **as an experiment**, but tame the lifecycle
races that got it retired, using:

- **SQLite** (`var/pgdev.db`) as the single source of truth for machine
  tracking, so a reconcile is one atomic, transactional decision instead of a
  scatter of racing `os.WriteFile`s.
- an explicit **port-free gate + post-verify** around every socat reload — the
  actual fix for the orphan-listener → EADDRINUSE → silent-stale-mapping bug
  (SQLite alone does NOT fix that; it only serializes reconcilers).

The socat proxy took over the **client ports** (5442 active / 5443 staging), so
existing external configs picked it up with no change. The Go forwarder ran
alongside on a second pair (5444 / 5445) for one integration phase and is now
deleted — socat is the only host-side client path.

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
the DB first and then **mirrors the flat file**. The mirrors were introduced to
keep the untouched Go forwarder correct; they outlived it because `pgdev` itself
(`internal/activeslot`, `machineIP`) and the Makefile's `ACTIVE_SLOT` still READ
those files. Retiring them means moving those readers onto the DB (§5.2).

Structured logging throughout via stdlib **`log/slog`** (text handler → stderr,
`component=track`). `PG_PROXY_DEBUG=1` (the default) makes it chatty.

### 2.2 `internal/socatproxy` — the socat LaunchAgents

Two per-user LaunchAgents, `me.pansen.<prefix>-socat-active` (:5442) and
`-socat-staging` (:5443) — the **client ports**. Each runs:

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
   `lsof` the LISTEN pid and kill it **only if it is one of our own socat
   children** (argv[0] is `socat` and it listens on exactly this port — an
   ERROR, launchd leaked); a foreign holder is reported and the reconcile fails
   rather than killing an unrelated process. A port that never frees is a **hard
   failure** — refuse to bootstrap onto a held port (which is exactly how the
   stale mapping used to persist).
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

`proxy install` is the single command that brings the client path up. Installing
is also what opts in: `promote`/`refresh` stay hands-off (never touch launchd)
until a plist exists (`Reconciler.AnyInstalled`).

A port held by a foreign process hard-fails the gate loudly — we do not kill
arbitrary processes.

## 3. Deliberate decisions / non-goals

- **`proxy_target` is observability, not authority.** The durable record of what
  socat runs is the plist; decisions compare against it.
- **`status` surfaces socat** — one line per role (launchd state + the DB's
  last-applied target), which replaced the old forwarder section.
- **The Local Network grant is a one-time manual step**, not something the CLI
  tries to automate: TCC cannot be granted programmatically. After granting,
  the agents must be restarted (`proxy.uninstall` + `proxy.install`) because the
  decision is cached at process start — a plain `proxy reconcile` is a no-op
  when the mapping is already healthy, so it will NOT pick the grant up.
- **Data loss on schema change is accepted** (IPs re-discover; active carried).
- **Host-only DB.** Never open `var/pgdev.db` from inside a guest over virtiofs
  — SQLite over virtiofs corrupts (cf. the token cold-cache burn in 0002).

## 4. Open questions

- [ ] Behaviour of an in-flight `pg_restore` when a reload kills the socat
      children mid-transfer (expected: dropped; client reconnects). Acceptable
      for dev; confirm.

## 5. Removal of the Go forwarder

1. **Done** — `internal/forward`, `pgdev forward *`, the `endpoint.*` make
   targets, `PG_FORWARD_*`, and the `codesign` step in `pgdev/Makefile` (plus
   `etc/keys`, spec 0003 and the Local-Network screenshot) are deleted.
   `PG_FORWARD_BIND` became `PG_CLIENT_BIND` (it configures socat now).
2. **Open** — the **flat-file mirrors** in `internal/track` (`ActiveMirror`,
   `IPMirror`) and the `activeslot` file reads. These are NOT dead: `pgdev`
   resolves the active slot and machine IPs from `var/active-machine` /
   `var/machine-ip-{a,b}`, and the Makefile reads `ACTIVE_SLOT` from the same
   file at parse time. Retiring them means moving both readers onto the DB —
   including a Makefile that can't shell out to a not-yet-built binary — after
   which "no plain-text tracking" is fully realized.
3. **Done** — `pgdev status` shows the socat proxy where the forwarder section
   used to be.
4. **Done** — README + `.env.example` updated.

Stale runtime leftovers from the forwarder (`var/forward-state.json`,
`var/<prefix>-forward.log`) and its LaunchAgent
(`~/Library/LaunchAgents/me.pansen.<prefix>-forward.plist`) must be booted out
and deleted once on each machine that ran it.
