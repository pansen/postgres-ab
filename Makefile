# Native Incus on Linux — no colima wrapper. The incus daemon runs directly on
# the host; `make start` just makes sure it's up, initialised, and that the
# Postgres ports are reachable from the LAN. See README.md.

# Ports published on the host (LAN-wide). Keep in sync with pg-dev-local's
# ACTIVE_PORT / STAGING_PORT.
PG_PORTS := 5432-5433

-include .env
export

# Size (GiB) of the default btrfs storage pool. It is a single loop-backed image
# shared by all containers, so it must hold every backend's PGDATA + WAL + temp +
# snapshots at once — a restore briefly needs old + new data side by side. The
# Incus default (~30GiB) is far too small for real dumps and silently fills up
# mid-restore (a full disk shows up as a Postgres "crash of another server
# process", not a clean error). Override in .env if you need more/less.
POOL_SIZE ?= 240

# Install the host prerequisites and grant this user Incus access. Fedora/dnf;
# adjust the package line for other distros. btrfs-progs lets `incus admin init`
# build a copy-on-write pool (cheap snapshots) — the ZFS stand-in on Linux.
.PHONY: deps
deps:
	sudo dnf install -y incus jq btrfs-progs
	@# incusd runs as root and maps container UIDs/GIDs from root's subordinate
	@# ranges. Fedora ships /etc/sub{u,g}id without a root entry, so unprivileged
	@# containers fail to create ("System doesn't have a functional idmap setup").
	@# Grant root a large range (idempotent).
	@for f in /etc/subuid /etc/subgid; do \
		if ! grep -q '^root:' $$f 2>/dev/null; then \
			echo "==> Adding root subordinate-id range to $$f"; \
			echo 'root:1000000:1000000000' | sudo tee -a $$f >/dev/null; \
		fi; \
	done
	sudo systemctl enable --now incus.socket incus.service incus-startup.service
	sudo systemctl restart incus.service
	@if id -nG "$$USER" | tr ' ' '\n' | grep -qx incus-admin; then \
		echo "==> $$USER is already in the incus-admin group."; \
	else \
		echo "==> Adding $$USER to incus-admin — log out and back in for it to take effect."; \
		sudo usermod -aG incus-admin "$$USER"; \
	fi

# `start` is a thin gate: bring the daemon up, make sure we can talk to it,
# then hand off to `_start` for the real work. Right after `make deps` the
# incus-admin group is assigned but not yet active in existing login shells
# (even a GUI re-login often isn't enough — the display manager keeps the
# session alive). Rather than force a reboot we run the worker under `sg`, which
# activates the group with no logout. Keeping this in ONE recipe line matters:
# an `exec` inside a make recipe only replaces that line's subshell, not make,
# so a multi-line delegation would leak back into the ungrouped parent.
.PHONY: start
start:
	sudo systemctl start incus.socket incus.service
	@if incus info >/dev/null 2>&1; then \
		$(MAKE) _start; \
	elif id -nG "$$USER" | grep -qw incus-admin; then \
		echo "==> Activating the incus-admin group for this run (sg; no logout needed)..."; \
		sg incus-admin -c "$(MAKE) _start"; \
	else \
		echo "ERROR: '$$USER' can't reach the incus daemon (/run/incus/unix.socket)."; \
		echo "  Run 'make deps' to join the incus-admin group, then reboot once"; \
		echo "  (or 'newgrp incus-admin' in this shell) and retry."; \
		exit 1; \
	fi

# The real bring-up. Assumes the incus daemon is reachable (see `start`).
.PHONY: _start
_start:
	@# First-run init: create a storage pool + incusbr0 if none exist yet. Size
	@# the btrfs loop pool to POOL_SIZE (the ~30GiB default fills up mid-restore).
	@# Mark the disks dir NOCOW first: the loop image inherits it at creation, so
	@# the host btrfs doesn't CoW+checksum every random write the pool makes into
	@# the image (double-CoW halves Postgres bulk-load speed). Harmless on
	@# non-btrfs hosts (chattr just fails); no effect on an image that already
	@# exists — that needs an offline copy into a fresh +C file.
	@if [ -z "$$(incus storage list --format csv 2>/dev/null)" ]; then \
		sudo chattr +C /var/lib/incus/disks 2>/dev/null || true; \
		echo "==> Initialising Incus (btrfs storage pool $(POOL_SIZE)GiB + incusbr0)..."; \
		incus admin init --auto --storage-backend btrfs --storage-create-loop $(POOL_SIZE) \
			|| incus admin init --auto; \
	fi
	@# Disable CoW *inside* the pool too (same rationale, inner layer; snapshots
	@# still work — post-snapshot writes CoW once). Data checksums are lost, fine
	@# for throwaway dev DBs. Applies on next pool mount; idempotent.
	@if [ "$$(incus storage get default btrfs.mount_options 2>/dev/null)" != "user_subvol_rm_allowed,nodatacow" ]; then \
		incus storage set default btrfs.mount_options=user_subvol_rm_allowed,nodatacow 2>/dev/null || true; \
	fi
	@# Self-heal an existing loop pool that predates the sizing above (or an
	@# earlier, smaller POOL_SIZE). Grow-only: btrfs loop pools can't shrink, so
	@# a POOL_SIZE below the current size is left untouched. GiB → bytes for the
	@# comparison ($(POOL_SIZE) * 2^30).
	@cur=$$(incus storage get default size 2>/dev/null); \
	if [ -n "$$cur" ] && [ "$${cur%GiB}" != "$$cur" ]; then \
		curg=$${cur%GiB}; \
		if [ "$$curg" -lt "$(POOL_SIZE)" ] 2>/dev/null; then \
			echo "==> Growing default pool $${curg}GiB → $(POOL_SIZE)GiB..."; \
			incus storage set default size=$(POOL_SIZE)GiB || true; \
		fi; \
	fi
	@# Open the Postgres ports so other LAN hosts can connect.
	@$(MAKE) firewall
	@# Start any project containers that already exist (a no-op on a fresh host).
	@for c in pg-dev-a pg-dev-b pg-proxy; do incus start $$c >/dev/null 2>&1 || true; done
	@# If the proxy came up, re-assert its host forwards + backend IP pins.
	@if [ "$$(incus list pg-proxy --format csv -c s 2>/dev/null | head -1)" = "RUNNING" ]; then \
		sleep 2 && $(MAKE) pg.refresh; \
	fi
	$(MAKE) status

# Two firewalld jobs (Fedora's default firewall; skipped when it isn't running):
#  1. Open PG_PORTS so LAN hosts can reach the published Postgres ports.
#  2. Put incusbr0 in the `trusted` zone. Without this, firewalld's default zone
#     drops DHCP/DNS on the bridge, so containers come up with only an IPv6
#     address and `make pg.up` hangs forever at "Waiting for IPv4". This is the
#     incus-documented fix. Both settings are --permanent, so they persist
#     across reboots; safe to re-run.
.PHONY: firewall
firewall:
	@if command -v firewall-cmd >/dev/null 2>&1 && sudo firewall-cmd --state >/dev/null 2>&1; then \
		echo "==> Opening $(PG_PORTS)/tcp on firewalld..."; \
		sudo firewall-cmd --permanent --add-port=$(PG_PORTS)/tcp >/dev/null; \
		echo "==> Trusting the incusbr0 bridge (DHCP/DNS for containers)..."; \
		sudo firewall-cmd --permanent --zone=trusted --change-interface=incusbr0 >/dev/null 2>&1 || true; \
		sudo firewall-cmd --reload >/dev/null; \
	else \
		echo "==> firewalld not active — skipping (open $(PG_PORTS)/tcp + trust incusbr0 yourself if you filter)."; \
	fi

.PHONY: status/incus
status/incus:
	incus version
	incus list
	incus info --resources | head -n5

.PHONY: status
status: status/incus pg.ip pg.snapshots

# Stop the project containers, leaving the incus daemon (and any other
# workloads on it) running. The counterpart to `make start`.
.PHONY: stop
stop:
	@for c in pg-proxy pg-dev-a pg-dev-b; do incus stop $$c 2>/dev/null || true; done
	$(MAKE) status

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

.PHONY: pg.endpoint
pg.endpoint:
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
	$(PG_DEV) refresh
	$(MAKE) status

# ----- export / import (active backend) -----------------------------------

.PHONY: pg.export
pg.export:
	$(PG_DEV) export

.PHONY: pg.import-last
pg.import-last:
	$(PG_DEV) import-last

# ----- teardown ----------------------------------------------------------

# Destroy the three project containers (and their snapshots). On native Incus
# there is no VM to throw away, so this is exactly `pg.down`. Irreversible —
# export first (make pg.export) if you want to keep the active backend.
.PHONY: delete
delete:
	$(PG_DEV) down

.PHONY: recreate
recreate: delete start
