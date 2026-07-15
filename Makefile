packages := incus colima jq

.PHONY: deps
deps:
	HOMEBREW_NO_AUTO_UPDATE=1 brew install $(packages)
	HOMEBREW_NO_AUTO_UPDATE=1 brew upgrade $(packages)

-include .env
export

# colima grows an existing VM disk on restart but never shrinks it, so this is
# a safe floor: bumping it and re-running `make start` enlarges in place. The
# default (colima's 60 GiB) is too small for a multi-GB database plus repeat
# restores, 16 GB of transient WAL, and snapshots — a disk-full there surfaces
# as a mysterious cluster crash, not a clean error. Override in .env if needed.
COLIMA_DISK ?= 140   # GiB

.PHONY: start
start:
	colima start \
		--verbose \
		--runtime=incus \
		--memory $(COLIMA_MEMORY) \
		--cpu $(COLIMA_CPU) \
		--disk $(COLIMA_DISK)
	@if [ "$$(incus list pg-bouncer --format csv -c s 2>/dev/null | head -1)" = "RUNNING" ]; then \
		sleep 2 && $(MAKE) pg.bouncer.reload; \
	fi
	$(MAKE) status

.PHONY: status/incus
status/incus:
	incus version
	incus list
	incus info --resources | head -n5

.PHONY: status
status: status/incus pg.ip pg.snapshots

.PHONY: stop
stop:
	colima stop --verbose

PG_DEV := scripts/pg-dev-local

# ----- lifecycle ----------------------------------------------------------

.PHONY: pg.up
pg.up:
	$(PG_DEV) up

.PHONY: pg.down
pg.down:
	$(PG_DEV) down

.PHONY: pg.status
pg.status:
	$(PG_DEV) status
	@$(PG_DEV) endpoint

.PHONY: pg.backend.endpoint
pg.backend.endpoint:
	@$(PG_DEV) backend-endpoint

.PHONY: pg.promote
pg.promote:
	$(PG_DEV) promote

# ----- active backend -----------------------------------------------------

.PHONY: pg.psql
pg.psql:
	$(PG_DEV) psql

.PHONY: pg.shell
pg.shell:
	$(PG_DEV) shell

.PHONY: pg.ip
pg.ip:
	@$(PG_DEV) ip

.PHONY: pg.logs
pg.logs:
	$(PG_DEV) logs

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
	$(PG_DEV) staging.psql

.PHONY: pg.staging.shell
pg.staging.shell:
	$(PG_DEV) staging.shell

.PHONY: pg.staging.logs
pg.staging.logs:
	$(PG_DEV) staging.logs

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
	$(PG_DEV) bouncer.reload
	$(MAKE) status

# ----- bouncer ------------------------------------------------------------

.PHONY: pg.bouncer.logs
pg.bouncer.logs:
	$(PG_DEV) bouncer.logs

.PHONY: pg.bouncer.reload
pg.bouncer.reload:
	$(PG_DEV) bouncer.reload

# ----- export / import (active backend) -----------------------------------

.PHONY: pg.export
pg.export:
	$(PG_DEV) export

.PHONY: pg.import-last
pg.import-last:
	$(PG_DEV) import-last

# ----- colima ------------------------------------------------------------

.PHONY: delete
delete:
	colima delete --force

.PHONY: recreate
recreate: delete start
