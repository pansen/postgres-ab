# Spec 0003 — `internal/forward`: replace the shell socat forwarder with Go

Status: **proposed** (2026-07-23) · Owner: andi · Relates to:
`0001-pgdev-de-shelling-spec.md` (Slice 4 `internal/forward`, deferred),
`0002-two-machine-disk-reclaim.md` (two-machine model). Memory:
`pgdevd-token-home-mount-cold-cache`, `apple-container-cli-quirks`.

This is a self-contained brief for a fresh session. Goal: retire
`scripts/host-endpoint` (+ the `socat` dependency) and move the host-side client
forwarder into a small in-process Go component. Everything else stays.

---

## 1. Why

The client forwarder is the **last shell holdover** and the source of every
recent host-side footgun — and they are all *process-lifecycle* bugs that only
exist because the relay is an external process (`socat`) supervised by another
process (`launchd`) over signals:

- **Orphaned socats / rebind race.** `launchctl kickstart -k` restarts `_serve`
  with **SIGKILL**, which bypasses the bash cleanup trap and orphans the socat
  children. They keep holding `:5442`/`:5443`, so the re-pointed `_serve` fails
  to bind (`Address already in use`) and the **stale mapping persists** — making
  `promote` look like a no-op. (Worked around in host-endpoint by reaping stale
  port holders on `_serve` start — a hack this spec removes.)
- **Swap-on-promote hazard.** When re-point silently fails, `:5442`/`:5443` keep
  the pre-promote mapping, so a `pg_restore -p 5443` (expected: staging) can hit
  the **active** DB. Client data path → correctness-critical.
- **launchd fragility** generally (EIO on bootstrap, sandbox plist-write limits).

A Go forwarder holds the two listeners **in-process** and re-points by swapping
the dial target — nothing to orphan, no rebind, no SIGKILL trap-bypass. The
whole class disappears.

## 2. What the forwarder does today (to replicate)

`scripts/host-endpoint` (macOS host only; not in-machine):

- Reads `MACHINE_PREFIX`, `PG_BACKEND_PORT` (5432), `PG_CLIENT_ACTIVE_PORT`
  (5442), `PG_CLIENT_STAGING_PORT` (5443), and the state files
  `var/active-machine` (`a`/`b`, default `a`), `var/machine-ip-a`,
  `var/machine-ip-b`.
- Maps `127.0.0.1:5442` → `<active-machine-ip>:5432` and `:5443` →
  `<staging-machine-ip>:5432`, where active = the pointer and staging = the
  other. A missing IP for one role must not block the other.
- Runs under a per-user launchd LaunchAgent (`me.pansen.<prefix>-forward`,
  `RunAtLoad`+`KeepAlive`) so it survives logins. Subcommands: `ip`, `install`,
  `refresh`, `uninstall`, `status`, `_serve`.
- `refresh` re-reads IPs and `kickstart`s the agent (the fragile re-point path).
- `make start` auto-installs it; `pgdev promote`/`refresh` call `host-endpoint
  refresh`; `PG_ENDPOINT_AUTOINSTALL=0` opts out.

## 3. Design — `internal/forward`

A long-running host process (macOS) that owns the listeners for their whole
lifetime and re-points targets in place:

- On start, `net.Listen("tcp", "<bind>:5442")` and `:5443` **once**; never
  rebind.
- Hold a mutex-guarded routing table: `{active → "<ip>:5432", staging →
  "<ip>:5432"}`, computed from the pointer + IP files.
- **Watch** `var/active-machine` and `var/machine-ip-{a,b}` — poll (~1–2 s) is
  simplest and robust on macOS; fsnotify optional. On change, recompute and swap
  the two targets atomically. Listeners stay bound.
- Per accepted conn: read the current target for that listener's role, `net.Dial`
  it, `io.Copy` both directions, close on either side. If the target IP is empty
  (machine down), close the accepted conn with a logged reason.
- Bind address: default `127.0.0.1`. Add an optional wider bind (e.g. `0.0.0.0`)
  behind a config knob so `PG_PROXY_HOSTNAME=host.docker.internal` is reachable
  from sibling containers/k3d without relying on Docker Desktop forwarding to
  loopback. (Today socat binds loopback only.)
- In-flight conns on a target change: leave established ones alone; new conns use
  the new target. Matches the existing "promote may drop sessions; reconnect"
  contract. (Optional: drop in-flight on promote to force reconnect — decide.)

### CLI / integration

- Add `pgdev forward serve | install | uninstall | status` (host CLI). `serve`
  is what the LaunchAgent runs. `install`/`uninstall`/`status` manage the plist
  (port the host-endpoint launchd logic to Go, or keep a thin installer).
- **`promote` collapses to a pointer write.** `cmd/pgdev` `refreshForwarder()`
  becomes a no-op (or is removed): promote already does `active.Set(to)`, and the
  running forwarder re-points itself within its poll interval — **no shell exec,
  no kickstart, no launchd round-trip.** `refresh` keeps re-discovering IPs
  (writing `var/machine-ip-{a,b}`); the forwarder picks those up too.
- `make start`/`endpoint.*` targets call the Go subcommands instead of the
  script.

### Reuse (already present)

- `config.Config`: `ClientActivePort`, `ClientStagingPort`, `BackendPort`,
  `ProxyHostname`, `MachineIPPath(slot)`, `ActiveMachinePath()`,
  `MachineNameForSlot(slot)`.
- `internal/activeslot.Pointer` — read `var/active-machine`.
- `internal/applecli` — machine-IP discovery (keep in `pgdev ip`/`refresh`).

### launchd (stays, but dumb)

Persistence across login still wants a LaunchAgent — but it just runs `pgdev
forward serve` and **never needs restarting to re-point**. So promote no longer
touches launchd, and the SIGKILL/orphan/rebind problems can't recur. Plist-write
still needs `~/Library/LaunchAgents` (nono-sandbox caveat unchanged — orthogonal
to language).

## 4. Retire on completion

- `scripts/host-endpoint` (delete once `pgdev forward` covers install/status).
- `socat` from `make deps` and the README requirements.
- The host-endpoint reap-on-start hack and pointer-flip poll (obviated).

## 5. Acceptance / tests

- **Unit:** role→target computation from (pointer, IP files); a pointer flip
  updates both targets; missing-IP handling; bind-address selection.
- **Live:** bring up `vpg-a`/`vpg-b`; connect via `:5442`/`:5443`; `pgdev
  promote`; assert `:5442` follows to the new active **without any restart**
  (verify by `SELECT system_identifier FROM pg_control_system()` vs each machine
  IP — the check used during 0002 validation). Repeat promotes leave **no
  orphaned processes** and never hit "Address already in use".
- `make check` green; `bash -n` no longer needed for host-endpoint once deleted.

## 6. Open decisions

1. Poll vs fsnotify for the watch (recommend: poll ~1–2 s).
2. Bind `127.0.0.1` only vs configurable wider bind for container access.
3. Drop in-flight conns on promote, or let them finish (recommend: let finish).
4. Fold the launchd installer into Go now, or keep a minimal shim temporarily.
