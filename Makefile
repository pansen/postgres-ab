-include .env
export

MACHINE_NAME ?= pg
MACHINE_IMAGE ?= local/pg-incus-machine:26.04
MACHINE_CPUS ?= 4
MACHINE_MEMORY ?= 12G

# Apple container CLI 1.1 does not expose a machine disk-size setting. This is
# the logical size of the sparse XFS loop filesystem used for cheap PostgreSQL
# data snapshots inside the machine's root disk.
PG_DATA_DISK_SIZE ?= 140G

PG_DEV_SCRIPT := ./scripts/pg-dev-local
MACHINE_RUN := container machine run --name $(MACHINE_NAME) --root --interactive \
	--workdir "$(CURDIR)" \
	--env HOST_UID=$(shell id -u) \
	--env HOST_GID=$(shell id -g) \
	--env PG_DATA_DISK_SIZE=$(PG_DATA_DISK_SIZE) --
MACHINE_RUN_TTY := container machine run --name $(MACHINE_NAME) --root --interactive --tty \
	--workdir "$(CURDIR)" \
	--env HOST_UID=$(shell id -u) \
	--env HOST_GID=$(shell id -g) \
	--env PG_DATA_DISK_SIZE=$(PG_DATA_DISK_SIZE) --
PG_DEV := $(MACHINE_RUN) $(PG_DEV_SCRIPT)
PG_DEV_TTY := $(MACHINE_RUN_TTY) $(PG_DEV_SCRIPT)

.PHONY: deps
deps:
	@command -v container >/dev/null || { \
		echo "Apple's container CLI is required: https://github.com/apple/container/releases" >&2; \
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

.PHONY: system.start
system.start: deps
	container system start --enable-kernel-install

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
	container machine set-default $(MACHINE_NAME)
	# Apple 1.1 cannot attach an interactive exec while a machine is making its
	# first transition from stopped to running. Boot it non-interactively, then
	# let the guest bootstrap wait for systemd's bus below.
	container machine run --name $(MACHINE_NAME) --root -- true
	$(MACHINE_RUN) ./scripts/apple-machine-init

.PHONY: machine.shell
machine.shell: machine
	container machine run --name $(MACHINE_NAME) --interactive --tty

.PHONY: machine.shell.root
machine.shell.root: machine
	container machine run --name $(MACHINE_NAME) --root --interactive --tty

.PHONY: machine.status
machine.status: system.start
	container machine inspect $(MACHINE_NAME)

.PHONY: start
start: machine
	$(PG_DEV) refresh
	$(MAKE) status

.PHONY: status/incus
status/incus: machine
	$(MACHINE_RUN) incus version
	$(MACHINE_RUN) incus list
	$(MACHINE_RUN) incus info --resources

# The one status command: pointer + proxy roles, per-backend state/endpoints,
# snapshot counts and snapshot timelines.
.PHONY: status
status: machine
	@$(PG_DEV) status

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
pg.down:
	$(PG_DEV) down

.PHONY: pg.status
pg.status:
	$(PG_DEV) status
	@$(PG_DEV) endpoint

.PHONY: pg.promote
pg.promote:
	$(PG_DEV) promote

.PHONY: pg.refresh
pg.refresh:
	$(PG_DEV) refresh

# ----- active backend -----------------------------------------------------

.PHONY: pg.psql
pg.psql:
	$(PG_DEV_TTY) psql

.PHONY: pg.shell
pg.shell:
	$(PG_DEV_TTY) shell

.PHONY: pg.ip
pg.ip:
	@$(PG_DEV) ip

.PHONY: pg.logs
pg.logs:
	$(PG_DEV_TTY) logs

.PHONY: pg.snapshot
pg.snapshot:
	$(PG_DEV) snapshot $(name) $(if $(force),--force,)

.PHONY: pg.restore
pg.restore:
	$(PG_DEV) restore $(name)

.PHONY: pg.restore-last
pg.restore-last:
	$(PG_DEV) restore-last

.PHONY: pg.snapshots
pg.snapshots:
	$(PG_DEV) snapshots

# ----- staging backend ----------------------------------------------------

.PHONY: pg.staging.psql
pg.staging.psql:
	$(PG_DEV_TTY) staging.psql

.PHONY: pg.staging.shell
pg.staging.shell:
	$(PG_DEV_TTY) staging.shell

.PHONY: pg.staging.logs
pg.staging.logs:
	$(PG_DEV_TTY) staging.logs

.PHONY: pg.staging.snapshot
pg.staging.snapshot:
	$(PG_DEV) staging.snapshot $(name) $(if $(force),--force,)

.PHONY: pg.staging.restore
pg.staging.restore:
	$(PG_DEV) staging.restore $(name)

.PHONY: pg.staging.restore-last
pg.staging.restore-last:
	$(PG_DEV) staging.restore-last

.PHONY: pg.staging.reset
pg.staging.reset:
	$(PG_DEV) staging.reset

.PHONY: pg.staging.stop
pg.staging.stop:
	$(PG_DEV) staging.stop
	$(MAKE) status

.PHONY: pg.staging.start
pg.staging.start:
	$(PG_DEV) staging.start
	sleep 1
	$(PG_DEV) refresh
	$(MAKE) status

# ----- full export / import -----------------------------------------------

.PHONY: pg.export
pg.export:
	$(PG_DEV) export

.PHONY: pg.import-last
pg.import-last:
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
	bash -n scripts/apple-machine-init scripts/pg-dev-local
	@$(MAKE) --no-print-directory -n deps >/dev/null
