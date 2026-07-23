-include .env
export

# MACHINE_PREFIX yields the per-slot machine names <prefix>-a / <prefix>-b —
# spec 0002: each slot owns its own Apple machine (own Incus + one PG backend),
# instead of one machine hosting both. The interactive shell/logs targets
# (pg.shell, pg.logs, pg.staging.shell, pg.staging.logs) resolve the active vs
# staging machine host-side from var/active-machine (see ACTIVE_SLOT below) and
# exec into the correct one; the control path (status/promote/refresh/snapshot/
# restore) all go through $(PGDEV), which fans out over both machines itself.
MACHINE_PREFIX ?= vpg
MACHINE_IMAGE ?= local/pg-incus-machine:26.04
MACHINE_CPUS ?= 4
# Two machines now share the Mac (one backend + one Incus daemon each), so the
# default per-machine memory is smaller than the old single-machine 12G.
MACHINE_MEMORY ?= 8G

# Apple container CLI 1.1 does not expose a machine disk-size setting. This is
# the logical size of the sparse XFS loop filesystem used for cheap PostgreSQL
# data snapshots inside the machine's root disk.
PG_DATA_DISK_SIZE ?= 140G

# The machine's root disk (vdb) is a SPARSE image on macOS. It grows as data is
# written inside the guest and — because Apple's container runtime does not
# compact on discard/TRIM — effectively never shrinks. The XFS PostgreSQL store
# lives on it, so a large restore can ratchet the Mac's free space to zero; the
# next guest write then fails and the XFS store shuts down mid-write (EIO),
# dropping PostgreSQL into recovery. `disk.check` refuses write-heavy targets
# below this much free space on the macOS volume backing the VM. Set it to at
# least the size of the dump you are about to restore. See `make disk`.
DISK_MIN_FREE_GB ?= 40

PG_DEV_SCRIPT := ./scripts/pg-dev-local
HOST_ENDPOINT := ./scripts/host-endpoint
PGDEV_DIR := pgdev
# Host CLI (macOS). Since Slice 2 the stateful control path — status / promote /
# refresh / snapshots / ip / snapshot / restore — runs through this binary over
# the resident pgdevd HTTP API, not `container machine run` execs into the shell.
PGDEV := $(PGDEV_DIR)/bin/pgdev

# Host-side active/staging resolution (spec 0002): var/active-machine holds the
# active slot ("a"/"b", default "a"); staging is the other. Read once at parse
# time — the pointer is stable for the duration of a make invocation.
ACTIVE_SLOT := $(shell cat "$(CURDIR)/var/active-machine" 2>/dev/null || echo a)
STAGING_SLOT := $(if $(filter b,$(ACTIVE_SLOT)),a,b)

# Exec scripts/pg-dev-local against ONE slot's machine, passing PG_SLOT so the
# shim targets that machine's single backend (each machine hosts exactly one).
# $(call PG_DEV_IN,<slot>,<subcommand>). TTY-aware: Apple's `--tty` exec fails
# with "Operation not supported by device" when stdin/stdout is not a terminal,
# so scripted callers (CI, pipes) get the plain transport.
define PG_DEV_IN
name="$(MACHINE_PREFIX)-$(1)"; \
if [ -t 0 ] && [ -t 1 ]; then \
	container machine run --name "$$name" --root --interactive --tty --workdir "$(CURDIR)" --env PG_SLOT=$(1) -- $(PG_DEV_SCRIPT) $(2); \
else \
	container machine run --name "$$name" --root --interactive --workdir "$(CURDIR)" --env PG_SLOT=$(1) -- $(PG_DEV_SCRIPT) $(2); \
fi
endef

# Auto-create .env from the template. `-include .env` at the top makes GNU make
# remake a missing .env via this rule (and re-read it) before any goal runs, so
# editing it is optional — the defaults work. Order-only prereq (`|`): an
# existing .env is never clobbered, even if .env.example is newer.
.env: | .env.example
	@cp .env.example $@
	@echo "==> Created $@ from .env.example (defaults work; edit if you like)."

.PHONY: deps
deps: .env
	@command -v container >/dev/null || { \
		echo "Apple's container CLI is required: https://github.com/apple/container/releases" >&2; \
		exit 1; \
	}
	@command -v socat >/dev/null || { \
		echo "socat is required for the stable 127.0.0.1 client endpoint: brew install socat" >&2; \
		exit 1; \
	}
	@version="$$(container --version | awk '{ print $$4 }')"; \
	major="$${version%%.*}"; rest="$${version#*.}"; minor="$${rest%%.*}"; \
	case "$$major.$$minor" in (*[!0-9.]*|.*|*.) \
		echo "Cannot parse Apple container CLI version '$$version'." >&2; exit 1;; esac; \
	if [ "$$major" -lt 1 ] || { [ "$$major" -eq 1 ] && [ "$$minor" -lt 1 ]; }; then \
		echo "Apple container CLI 1.1 or newer is required (found $$version)." >&2; \
		exit 1; \
	fi
	container --version

# `container system start` is not reliably idempotent: with the apiserver
# already running it can fail on a cosmetic file write. Tolerate that as long
# as the API actually answers; fail hard only when it doesn't.
.PHONY: system.start
system.start: deps
	@container system start --enable-kernel-install \
		|| { container machine list >/dev/null 2>&1 \
			&& echo "container system start reported an error, but the API is reachable — continuing."; } \
		|| { echo "ERROR: container system is not reachable." >&2; exit 1; }

.PHONY: machine.image
machine.image: system.start
	container build --tag $(MACHINE_IMAGE) --file Dockerfile.machine .

# Create BOTH persistent Linux machines (vpg-a, vpg-b — spec 0002: one per
# slot) and boot them. The XFS reflink store and the Incus daemon topology are
# no longer bootstrapped here (scripts/apple-machine-init is retired):
# `pgdevd bootstrap` runs them as the daemon unit's ExecStartPre, installed by
# `pgdev agent deploy` (the `deploy` target), once per machine. `machine set` is
# harmless while running; changed resources take effect after the next
# stop/start. Not `--set-default`: with two machines there is no meaningful
# default (mirrors internal/applecli.Create).
.PHONY: machine
machine: system.start
	@needs_image=0; \
	for slot in a b; do \
		container machine inspect "$(MACHINE_PREFIX)-$$slot" >/dev/null 2>&1 || needs_image=1; \
	done; \
	if [ "$$needs_image" = 1 ]; then $(MAKE) machine.image; fi
	@for slot in a b; do \
		name="$(MACHINE_PREFIX)-$$slot"; \
		if ! container machine inspect "$$name" >/dev/null 2>&1; then \
			container machine create \
				--name "$$name" \
				--cpus $(MACHINE_CPUS) \
				--memory $(MACHINE_MEMORY) \
				--home-mount rw \
				$(MACHINE_IMAGE); \
		fi; \
		container machine set --name "$$name" \
			cpus=$(MACHINE_CPUS) memory=$(MACHINE_MEMORY) home-mount=rw; \
		echo "==> Booting $$name..."; \
		deadline=$$(($$(date +%s) + 150)); \
		until container machine run --name "$$name" --root -- true >/dev/null 2>&1; do \
			if [ $$(date +%s) -ge $$deadline ]; then \
				echo "ERROR: $$name never became execable within 150s (Apple boot race)." >&2; exit 1; \
			fi; \
			sleep 3; \
		done; \
	done

# Build + install the in-machine daemon on BOTH machines and confirm the
# version handshake on each. The daemon's ExecStartPre runs `pgdevd bootstrap`
# (XFS store + Incus topology), so this is also what stands up freshly created
# machines. `pgdev agent deploy` defaults to --machine=both.
.PHONY: deploy
deploy: machine pgdevd
	$(PGDEV) agent deploy

# Cheap guard for targets that exec into already-running machines: fail fast
# with a clear message instead of a raw Apple CLI 'notFound' error when a
# machine has never been created.
.PHONY: machine.exists
machine.exists:
	@for slot in a b; do \
		name="$(MACHINE_PREFIX)-$$slot"; \
		container machine inspect "$$name" >/dev/null 2>&1 || { echo "Machine '$$name' does not exist — run 'make start' first." >&2; exit 1; }; \
	done

# ----- disk-space trap (the sparse VM disk grows and does not shrink) ------
# `make disk` inspects real usage on both machines; `make disk.check` is the
# fail-fast guard on write-heavy targets. There is no supported per-machine
# disk cap or compaction in Apple container 1.1: the dependable reclaim is
# deleting a machine (macOS frees the whole sparse image → rebuild) — either
# `make pg.staging.rebuild` (staging only, active untouched — the everyday
# reclaim tier, spec 0002) or `make recreate` (both machines, full nuke).
# `recreate` wipes both data trees, so dump anything you need first (e.g.
# pg_dump over :5442/:5443).

.PHONY: disk
disk:
	@echo "── macOS volume backing the Apple container VM ──"
	@df -h "$(HOME)"
	@echo
	@echo "Apple container storage (physical allocation on macOS):"
	@du -sh "$(HOME)/Library/Containers/com.apple.container" 2>/dev/null || echo "  (path not readable)"
	@container system df 2>/dev/null || true
	@for slot in a b; do \
		name="$(MACHINE_PREFIX)-$$slot"; \
		echo; \
		echo "── guest machine root disk (vdb — sparse, grows only): $$name ──"; \
		container machine run --name "$$name" --root -- df -h / 2>/dev/null || echo "  (machine not running)"; \
		echo; \
		echo "XFS PostgreSQL store on $$name (.xfs actual blocks; logical size $(PG_DATA_DISK_SIZE)):"; \
		container machine run --name "$$name" --root -- du -h /var/lib/pg-dev-local.xfs 2>/dev/null || true; \
		container machine run --name "$$name" --root -- df -h /var/lib/pg-dev-local 2>/dev/null \
			|| echo "  (XFS store unavailable — if it shut down after a full disk, remount with 'make start')"; \
	done

# Fast pre-flight for write-heavy targets: no guest calls, just macOS free space.
.PHONY: disk.check
disk.check:
	@avail=$$(df -g "$(HOME)" | awk 'NR==2 {print $$4}'); \
	if [ "$${avail:-0}" -lt "$(DISK_MIN_FREE_GB)" ]; then \
		echo "ERROR: only $${avail} GiB free on the macOS volume backing the VM (need >= $(DISK_MIN_FREE_GB) GiB)." >&2; \
		echo "       The VM root disk is a sparse image that only grows; a large restore can fill" >&2; \
		echo "       macOS and shut the guest PostgreSQL store down mid-write. Free space on macOS," >&2; \
		echo "       reclaim with 'make pg.staging.rebuild' (staging only) or 'make recreate' (both)," >&2; \
		echo "       or lower DISK_MIN_FREE_GB. See 'make disk'." >&2; \
		exit 1; \
	fi; \
	echo "==> macOS free space OK ($${avail} GiB >= $(DISK_MIN_FREE_GB) GiB)."

# Build both Go binaries on the host, stamped from the same git state: the
# in-machine daemon (pgdev/bin/pgdevd, linux/arm64, delivered via the home-mount
# and installed by `pgdev agent deploy`) and the host CLI (pgdev/bin/pgdev,
# macOS). A prerequisite of every control-path and snapshot target so the shell
# shims, the daemon and the CLI never drift.
.PHONY: pgdevd
pgdevd:
	@$(MAKE) -C $(PGDEV_DIR) build

# machine.shell drops you into the ACTIVE machine (override with slot=a|b).
slot ?= $(ACTIVE_SLOT)

.PHONY: machine.shell
machine.shell: machine.exists
	container machine run --name $(MACHINE_PREFIX)-$(slot) --interactive --tty

.PHONY: machine.shell.root
machine.shell.root: machine.exists
	container machine run --name $(MACHINE_PREFIX)-$(slot) --root --interactive --tty

.PHONY: machine.status
machine.status: system.start
	@for slot in a b; do \
		echo "── $(MACHINE_PREFIX)-$$slot ──"; \
		container machine inspect "$(MACHINE_PREFIX)-$$slot" || true; \
	done

.PHONY: start
start: deploy
	$(PGDEV) refresh
	$(MAKE) status

.PHONY: status/incus
status/incus: machine.exists
	@for slot in a b; do \
		name="$(MACHINE_PREFIX)-$$slot"; \
		echo "── $$name ──"; \
		container machine run --name "$$name" --root -- incus list 2>/dev/null || echo "  (Incus not up)"; \
	done

# The one status command: active/staging machine roles, per-machine
# state/endpoints, snapshot counts and snapshot timelines. Served by pgdevd
# (one per machine) over the HTTP API; the active/staging split is a host-side
# pointer (var/active-machine), not part of the daemon contract.
.PHONY: status
status: machine.exists pgdevd
	@$(PGDEV) status

# ----- stable macOS client endpoints --------------------------------------
# Each Apple machine's IP drifts and cannot be pinned, so a host-side socat
# forwarder publishes permanent 127.0.0.1:5442 (active) / :5443 (staging)
# endpoints and relays each to whichever machine currently holds that role (on
# its own eth0:5432). `pgdev refresh`/`pgdev promote` re-point it; `start`
# installs it automatically on first run.

.PHONY: endpoint.install
endpoint.install: machine.exists
	@$(HOST_ENDPOINT) install

.PHONY: endpoint.refresh
endpoint.refresh:
	@$(HOST_ENDPOINT) refresh

.PHONY: endpoint.uninstall
endpoint.uninstall:
	@$(HOST_ENDPOINT) uninstall

.PHONY: endpoint.status
endpoint.status:
	@$(HOST_ENDPOINT) status

.PHONY: stop
stop:
	@for slot in a b; do \
		name="$(MACHINE_PREFIX)-$$slot"; \
		if container machine inspect "$$name" >/dev/null 2>&1; then \
			container machine stop "$$name"; \
		fi; \
	done

.PHONY: system.stop
system.stop: stop
	container system stop

# ----- lifecycle ----------------------------------------------------------

.PHONY: pg.up
pg.up: disk.check deploy
	$(PGDEV) up

.PHONY: pg.down
pg.down: deploy
	$(PGDEV) down

.PHONY: pg.status
pg.status: machine.exists pgdevd
	$(PGDEV) status
	@$(PGDEV) endpoint

.PHONY: pg.promote
pg.promote: machine.exists pgdevd
	$(PGDEV) promote

.PHONY: pg.refresh
pg.refresh: machine.exists pgdevd
	$(PGDEV) refresh

# ----- active backend -----------------------------------------------------

.PHONY: pg.shell
pg.shell: machine.exists
	@$(call PG_DEV_IN,$(ACTIVE_SLOT),shell)

.PHONY: pg.ip
pg.ip: machine.exists pgdevd
	@$(PGDEV) ip

.PHONY: pg.logs
pg.logs: machine.exists
	@$(call PG_DEV_IN,$(ACTIVE_SLOT),logs)

.PHONY: pg.snapshot
pg.snapshot: machine.exists pgdevd
	$(PGDEV) snapshot $(name) $(if $(force),--force,)

.PHONY: pg.restore
pg.restore: machine.exists pgdevd
	$(PGDEV) restore $(name) $(if $(force),--force,)

.PHONY: pg.restore-last
pg.restore-last: machine.exists pgdevd
	$(PGDEV) restore-last $(if $(force),--force,)

.PHONY: pg.snapshots
pg.snapshots: machine.exists pgdevd
	$(PGDEV) snapshots

# ----- staging backend ----------------------------------------------------

.PHONY: pg.staging.shell
pg.staging.shell: machine.exists
	@$(call PG_DEV_IN,$(STAGING_SLOT),shell)

.PHONY: pg.staging.logs
pg.staging.logs: machine.exists
	@$(call PG_DEV_IN,$(STAGING_SLOT),logs)

.PHONY: pg.staging.snapshot
pg.staging.snapshot: machine.exists pgdevd
	$(PGDEV) staging snapshot $(name) $(if $(force),--force,)

.PHONY: pg.staging.restore
pg.staging.restore: disk.check machine.exists pgdevd
	$(PGDEV) staging restore $(name) $(if $(force),--force,)

.PHONY: pg.staging.restore-last
pg.staging.restore-last: disk.check machine.exists pgdevd
	$(PGDEV) staging restore-last $(if $(force),--force,)

.PHONY: pg.staging.reset
pg.staging.reset: disk.check machine.exists pgdevd
	$(PGDEV) staging reset $(if $(force),--force,)

.PHONY: pg.staging.stop
pg.staging.stop: machine.exists pgdevd
	$(PGDEV) staging stop
	$(MAKE) status

.PHONY: pg.staging.start
pg.staging.start: machine.exists pgdevd
	$(PGDEV) staging start
	$(PGDEV) refresh
	$(MAKE) status

# Hard reset (spec 0002's headline feature): delete + recreate ONLY the
# staging machine, reclaiming its grown sparse macOS disk, then re-provision a
# fresh backend on it. The active machine — and its data — is never touched.
# Unlike the soft resets above this is slow (machine boot + provision) but
# actually returns space to macOS; see 'make disk'/§1 of issues/0002.
.PHONY: pg.staging.rebuild
pg.staging.rebuild: disk.check machine.exists pgdevd
	$(PGDEV) staging rebuild $(if $(force),--force,)

# ----- destructive outer-machine lifecycle -------------------------------

# Apple 1.1 frequently returns an XPC timeout when deleting a RUNNING machine
# (especially one with wedged I/O), even though the delete actually completes and
# the apiserver then drops out. So: stop first (the documented workaround), then
# tolerate the delete error and judge success by whether the machine is actually
# gone — not by the exit code. Loops over BOTH machines; unlike
# 'pg.staging.rebuild' this nukes everything, active included.
.PHONY: delete
delete: system.start
	@for slot in a b; do \
		name="$(MACHINE_PREFIX)-$$slot"; \
		if container machine inspect "$$name" >/dev/null 2>&1; then \
			echo "==> Stopping $$name before delete..."; \
			container machine stop "$$name" 2>/dev/null || true; \
			container machine delete "$$name" || true; \
		fi; \
		if container machine inspect "$$name" >/dev/null 2>&1; then \
			echo "ERROR: $$name is still present after delete. The Apple apiserver may be wedged — retry, or reset it with 'make system.stop && make system.start'." >&2; \
			exit 1; \
		fi; \
		echo "==> $$name deleted (macOS reclaims its sparse disk image)."; \
	done

.PHONY: recreate
recreate: delete start

.PHONY: check
check:
	bash -n scripts/pg-dev-local scripts/host-endpoint
	@$(MAKE) --no-print-directory -n deps >/dev/null
	$(MAKE) -C $(PGDEV_DIR) vet test build
