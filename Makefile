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

PG_DEV_SCRIPT := ./scripts/pg-dev-local
HOST_ENDPOINT := ./scripts/host-endpoint
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

# Create the persistent Linux machine once, then bootstrap its Incus daemon and
# XFS reflink store on every run. `machine set` is intentionally harmless while
# running; changed resources take effect after the next stop/start.
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
	# first transition from stopped to running. Boot it non-interactively, then
	# let the guest bootstrap wait for systemd's bus below.
	container machine run --name $(MACHINE_NAME) --root -- true
	$(MACHINE_RUN) ./scripts/apple-machine-init

# Cheap guard for targets that exec into an already-running machine: fail fast
# with a clear message instead of a raw Apple CLI 'notFound' error when the
# machine has never been created.
.PHONY: machine.exists
machine.exists:
	@container machine inspect $(MACHINE_NAME) >/dev/null 2>&1 || { echo "Machine '$(MACHINE_NAME)' does not exist — run 'make start' first." >&2; exit 1; }

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
start: machine
	$(PG_DEV) refresh
	@$(HOST_ENDPOINT) refresh
	$(MAKE) status

.PHONY: status/incus
status/incus: machine.exists
	$(MACHINE_RUN) incus version
	$(MACHINE_RUN) incus list
	$(MACHINE_RUN) incus info --resources

# The one status command: pointer + proxy roles, per-backend state/endpoints,
# snapshot counts and snapshot timelines.
.PHONY: status
status: machine.exists
	@$(PG_DEV) status

# ----- stable macOS client endpoint --------------------------------------
# The Apple machine's IP drifts and cannot be pinned, so a host-side socat
# forwarder publishes a permanent 127.0.0.1:5432/:5433 endpoint and relays it
# to the machine's current IP. `start` re-points it automatically; install once.

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
pg.up: machine
	$(PG_DEV) up

.PHONY: pg.down
pg.down: machine.exists
	$(PG_DEV) down

.PHONY: pg.status
pg.status: machine.exists
	$(PG_DEV) status
	@$(PG_DEV) endpoint

.PHONY: pg.promote
pg.promote: machine.exists
	$(PG_DEV) promote

.PHONY: pg.refresh
pg.refresh: machine.exists
	$(PG_DEV) refresh

# ----- active backend -----------------------------------------------------

.PHONY: pg.psql
pg.psql: machine.exists
	@$(call PG_DEV_AUTO,psql)

.PHONY: pg.shell
pg.shell: machine.exists
	@$(call PG_DEV_AUTO,shell)

.PHONY: pg.ip
pg.ip: machine.exists
	@$(PG_DEV) ip

.PHONY: pg.logs
pg.logs: machine.exists
	@$(call PG_DEV_AUTO,logs)

.PHONY: pg.snapshot
pg.snapshot: machine.exists
	$(PG_DEV) snapshot $(name) $(if $(force),--force,)

.PHONY: pg.restore
pg.restore: machine.exists
	@$(call PG_DEV_AUTO,restore $(name) $(if $(force),--force,))

.PHONY: pg.restore-last
pg.restore-last: machine.exists
	@$(call PG_DEV_AUTO,restore-last $(if $(force),--force,))

.PHONY: pg.snapshots
pg.snapshots: machine.exists
	$(PG_DEV) snapshots

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
pg.staging.snapshot: machine.exists
	$(PG_DEV) staging.snapshot $(name) $(if $(force),--force,)

.PHONY: pg.staging.restore
pg.staging.restore: machine.exists
	@$(call PG_DEV_AUTO,staging.restore $(name) $(if $(force),--force,))

.PHONY: pg.staging.restore-last
pg.staging.restore-last: machine.exists
	@$(call PG_DEV_AUTO,staging.restore-last $(if $(force),--force,))

.PHONY: pg.staging.reset
pg.staging.reset: machine.exists
	@$(call PG_DEV_AUTO,staging.reset $(if $(force),--force,))

.PHONY: pg.staging.stop
pg.staging.stop: machine.exists
	$(PG_DEV) staging.stop
	$(MAKE) status

.PHONY: pg.staging.start
pg.staging.start: machine.exists
	$(PG_DEV) staging.start
	sleep 1
	$(PG_DEV) refresh
	$(MAKE) status

# ----- full export / import -----------------------------------------------

.PHONY: pg.export
pg.export: machine.exists
	$(PG_DEV) export

.PHONY: pg.import-last
pg.import-last: machine.exists
	$(PG_DEV) import-last

# ----- destructive outer-machine lifecycle -------------------------------

.PHONY: delete
delete: system.start
	@if container machine inspect $(MACHINE_NAME) >/dev/null 2>&1; then \
		container machine delete $(MACHINE_NAME); \
	fi

.PHONY: recreate
recreate: delete start

.PHONY: check
check:
	bash -n scripts/apple-machine-init scripts/pg-dev-local scripts/host-endpoint
	@$(MAKE) --no-print-directory -n deps >/dev/null
