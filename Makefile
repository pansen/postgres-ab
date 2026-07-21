-include .env
export

MACHINE_NAME ?= vpg
MACHINE_IMAGE ?= local/pg-incus-machine:26.04
MACHINE_CPUS ?= 4
MACHINE_MEMORY ?= 12G

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
MACHINE_RUN_ARGS := --name $(MACHINE_NAME) --root --interactive \
	--workdir "$(CURDIR)" \
	--env HOST_UID=$(shell id -u) \
	--env HOST_GID=$(shell id -g) \
	--env PG_DATA_DISK_SIZE=$(PG_DATA_DISK_SIZE)
MACHINE_RUN := container machine run $(MACHINE_RUN_ARGS) --
MACHINE_RUN_TTY := container machine run $(MACHINE_RUN_ARGS) --tty --
PG_DEV := $(MACHINE_RUN) $(PG_DEV_SCRIPT)
PG_DEV_TTY := $(MACHINE_RUN_TTY) $(PG_DEV_SCRIPT)

# Run pg-dev-local with a TTY only when the caller actually has one
# (interactive prompts, psql pager, shells). Apple's `--tty` exec fails with
# "Operation not supported by device" when stdin/stdout is not a terminal, so
# scripted callers (CI, pipes, `force=1` restores) get the plain transport.
define PG_DEV_AUTO
if [ -t 0 ] && [ -t 1 ]; then \
	$(PG_DEV_TTY) $(1); \
else \
	$(PG_DEV) $(1); \
fi
endef

.PHONY: deps
deps:
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

# Create the persistent Linux machine once and boot it. The XFS reflink store and
# the Incus daemon topology are no longer bootstrapped here (scripts/apple-machine-
# init is retired): `pgdevd bootstrap` runs them as the daemon unit's ExecStartPre,
# installed by `pgdev agent deploy` (the `deploy` target). `machine set` is
# harmless while running; changed resources take effect after the next stop/start.
.PHONY: machine
machine: system.start
	@if ! container machine inspect $(MACHINE_NAME) >/dev/null 2>&1; then \
		$(MAKE) machine.image; \
		container machine create \
			--name $(MACHINE_NAME) \
			--cpus $(MACHINE_CPUS) \
			--memory $(MACHINE_MEMORY) \
			--home-mount rw \
			--set-default \
			$(MACHINE_IMAGE); \
	fi
	container machine set --name $(MACHINE_NAME) \
		cpus=$(MACHINE_CPUS) memory=$(MACHINE_MEMORY) home-mount=rw
	# Apple 1.1 cannot attach an interactive exec while a machine is making its
	# first transition from stopped to running. Boot it non-interactively; the
	# daemon's ExecStartPre bootstrap (via `make deploy`) does the rest.
	container machine run --name $(MACHINE_NAME) --root -- true

# Build + install the in-machine daemon and confirm the version handshake. The
# daemon's ExecStartPre runs `pgdevd bootstrap` (XFS store + Incus topology), so
# this is also what stands up a freshly created machine.
.PHONY: deploy
deploy: machine pgdevd
	$(PGDEV) agent deploy

# Cheap guard for targets that exec into an already-running machine: fail fast
# with a clear message instead of a raw Apple CLI 'notFound' error when the
# machine has never been created.
.PHONY: machine.exists
machine.exists:
	@container machine inspect $(MACHINE_NAME) >/dev/null 2>&1 || { echo "Machine '$(MACHINE_NAME)' does not exist — run 'make start' first." >&2; exit 1; }

# ----- disk-space trap (the sparse VM disk grows and does not shrink) ------
# `make disk` inspects real usage; `make disk.check` is the fail-fast guard on
# write-heavy targets. There is no supported per-machine disk cap or compaction
# in Apple container 1.1: the dependable reclaim is `make recreate` (delete the
# machine → macOS frees the whole sparse image → rebuild). `recreate` wipes the
# data trees, so dump anything you need first (e.g. pg_dump over :5442/:5443).

.PHONY: disk
disk:
	@echo "── macOS volume backing the Apple container VM ──"
	@df -h "$(HOME)"
	@echo
	@echo "Apple container storage (physical allocation on macOS):"
	@du -sh "$(HOME)/Library/Containers/com.apple.container" 2>/dev/null || echo "  (path not readable)"
	@container system df 2>/dev/null || true
	@echo
	@echo "── guest machine root disk (vdb — sparse, grows only) ──"
	@container machine run --name $(MACHINE_NAME) --root -- df -h / 2>/dev/null || echo "  (machine not running)"
	@echo
	@echo "XFS PostgreSQL store (.xfs actual blocks; logical size $(PG_DATA_DISK_SIZE)):"
	@container machine run --name $(MACHINE_NAME) --root -- du -h /var/lib/pg-dev-local.xfs 2>/dev/null || true
	@container machine run --name $(MACHINE_NAME) --root -- df -h /var/lib/pg-dev-local 2>/dev/null \
		|| echo "  (XFS store unavailable — if it shut down after a full disk, remount with 'make start')"

# Fast pre-flight for write-heavy targets: no guest calls, just macOS free space.
.PHONY: disk.check
disk.check:
	@avail=$$(df -g "$(HOME)" | awk 'NR==2 {print $$4}'); \
	if [ "$${avail:-0}" -lt "$(DISK_MIN_FREE_GB)" ]; then \
		echo "ERROR: only $${avail} GiB free on the macOS volume backing the VM (need >= $(DISK_MIN_FREE_GB) GiB)." >&2; \
		echo "       The VM root disk is a sparse image that only grows; a large restore can fill" >&2; \
		echo "       macOS and shut the guest PostgreSQL store down mid-write. Free space on macOS," >&2; \
		echo "       reclaim with 'make recreate', or lower DISK_MIN_FREE_GB. See 'make disk'." >&2; \
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

.PHONY: machine.shell
machine.shell: machine.exists
	container machine run --name $(MACHINE_NAME) --interactive --tty

.PHONY: machine.shell.root
machine.shell.root: machine.exists
	container machine run --name $(MACHINE_NAME) --root --interactive --tty

.PHONY: machine.status
machine.status: system.start
	container machine inspect $(MACHINE_NAME)

.PHONY: start
start: deploy
	$(PGDEV) refresh
	@$(HOST_ENDPOINT) refresh
	$(MAKE) status

.PHONY: status/incus
status/incus: machine.exists
	$(MACHINE_RUN) incus version
	$(MACHINE_RUN) incus list
	$(MACHINE_RUN) incus info --resources

# The one status command: pointer + proxy roles, per-backend state/endpoints,
# snapshot counts and snapshot timelines. Served by pgdevd over the HTTP API.
.PHONY: status
status: machine.exists pgdevd
	@$(PGDEV) status

# ----- stable macOS client endpoint --------------------------------------
# The Apple machine's IP drifts and cannot be pinned, so a host-side socat
# forwarder publishes a permanent 127.0.0.1:5442/:5443 endpoint and relays it
# to the machine's current IP (on its :5432/:5433). `start` installs and
# re-points it automatically.

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
	@if container machine inspect $(MACHINE_NAME) >/dev/null 2>&1; then \
		container machine stop $(MACHINE_NAME); \
	fi

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

.PHONY: pg.psql
pg.psql: machine.exists
	@$(call PG_DEV_AUTO,psql)

.PHONY: pg.shell
pg.shell: machine.exists
	@$(call PG_DEV_AUTO,shell)

.PHONY: pg.ip
pg.ip: machine.exists pgdevd
	@$(PGDEV) ip

.PHONY: pg.logs
pg.logs: machine.exists
	@$(call PG_DEV_AUTO,logs)

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

.PHONY: pg.staging.psql
pg.staging.psql: machine.exists
	@$(call PG_DEV_AUTO,staging.psql)

.PHONY: pg.staging.shell
pg.staging.shell: machine.exists
	@$(call PG_DEV_AUTO,staging.shell)

.PHONY: pg.staging.logs
pg.staging.logs: machine.exists
	@$(call PG_DEV_AUTO,staging.logs)

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
pg.staging.stop: machine.exists
	$(PG_DEV) staging.stop
	$(MAKE) status

.PHONY: pg.staging.start
pg.staging.start: machine.exists pgdevd
	$(PG_DEV) staging.start
	$(PGDEV) refresh
	$(MAKE) status

# ----- destructive outer-machine lifecycle -------------------------------

# Apple 1.1 frequently returns an XPC timeout when deleting a RUNNING machine
# (especially one with wedged I/O), even though the delete actually completes and
# the apiserver then drops out. So: stop first (the documented workaround), then
# tolerate the delete error and judge success by whether the machine is actually
# gone — not by the exit code.
.PHONY: delete
delete: system.start
	@if container machine inspect $(MACHINE_NAME) >/dev/null 2>&1; then \
		echo "==> Stopping $(MACHINE_NAME) before delete..."; \
		container machine stop $(MACHINE_NAME) 2>/dev/null || true; \
		container machine delete $(MACHINE_NAME) || true; \
	fi
	@if container machine inspect $(MACHINE_NAME) >/dev/null 2>&1; then \
		echo "ERROR: $(MACHINE_NAME) is still present after delete. The Apple apiserver may be wedged — retry, or reset it with 'make system.stop && make system.start'." >&2; \
		exit 1; \
	fi
	@echo "==> $(MACHINE_NAME) deleted (macOS reclaims its sparse disk image)."

.PHONY: recreate
recreate: delete start

.PHONY: check
check:
	bash -n scripts/pg-dev-local scripts/host-endpoint
	@$(MAKE) --no-print-directory -n deps >/dev/null
	$(MAKE) -C $(PGDEV_DIR) vet test build
