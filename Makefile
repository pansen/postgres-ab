.PHONY: deps
deps:
	HOMEBREW_NO_AUTO_UPDATE=1 brew install incus colima
	HOMEBREW_NO_AUTO_UPDATE=1 brew upgrade incus colima

-include .env
export

.PHONY: start
start:
	colima start \
		--verbose \
		--runtime=incus \
		--memory $(COLIMA_MEMORY) \
		--cpu $(COLIMA_CPU)
	$(MAKE) status

.PHONY: status
status:
	incus version
	incus list
	incus info --resources | head -n5

.PHONY: stop
stop:
	colima stop \
		--verbose

PG_DEV := scripts/pg-dev-local

.PHONY: pg.up
pg.up:
	$(PG_DEV) up
	$(MAKE) pg.logs

.PHONY: pg.down
pg.down:
	$(PG_DEV) down

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

.PHONY: pg.ip
pg.ip:
	$(PG_DEV) ip

.PHONY: pg.psql
pg.psql:
	$(PG_DEV) psql

.PHONY: pg.shell
pg.shell:
	$(PG_DEV) shell

.PHONY: pg.status
pg.status:
	$(PG_DEV) status

.PHONY: pg.export
pg.export:
	$(PG_DEV) export

.PHONY: pg.import-last
pg.import-last:
	$(PG_DEV) import-last

.PHONY: pg.logs
pg.logs:
	incus exec pg-dev -- tail -f /var/log/postgresql/postgresql-17-main.log

.PHONY: delete
delete:
	colima delete --force

.PHONY: recreate
recreate: delete start
