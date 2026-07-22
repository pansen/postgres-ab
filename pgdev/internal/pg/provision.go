// Package pg holds the PostgreSQL-specific provisioning content and scripts the
// daemon runs inside a backend container (§5.7 of issues/0001): the embedded
// config templates (ported from scripts/pg-dev-local:317-352), the boot-ordering
// systemd drop-in, and the shell scripts for building the golden image, creating
// a slot's cluster, and provisioning the role + database.
//
// The scripts are pure string builders — the daemon executes them via
// backend.ExecScript (an `incus exec … bash -s`, a nested call with none of
// Apple's exec quirks). Keeping them here makes the SQL/quoting testable and the
// daemon orchestration thin.
package pg

import (
	"fmt"
	"strings"
)

// PGVersion is the PostgreSQL major version and cluster name.
const (
	Version = "17"
	Cluster = "main"
	Unit    = "postgresql@17-main"
	// DataPath is where a slot's data dir is bind-mounted inside the container.
	DataPath = "/var/lib/postgresql"
)

// confDev is the performance/logging tuning drop-in (conf.d/99-dev.conf).
const confDev = `listen_addresses = '*'
dynamic_shared_memory_type = posix

shared_buffers = 2GB
effective_cache_size = 6GB
work_mem = 32MB
maintenance_work_mem = 1GB
max_parallel_maintenance_workers = 4

max_wal_size = 16GB
checkpoint_timeout = 30min
checkpoint_completion_target = 0.8
wal_level = minimal
max_wal_senders = 0

fsync = off
full_page_writes = off
synchronous_commit = off
autovacuum = off

log_destination = stderr
logging_collector = off
log_min_duration_statement = 0
log_checkpoints = on
log_lock_waits = on
deadlock_timeout = 100
debug_pretty_print = on
`

// pgHBA is the host-based auth config (open, single-user dev machine).
const pgHBA = `local   all   postgres                peer
local   all   all                     md5
host    all   all   0.0.0.0/0         scram-sha-256
host    all   all   ::/0              scram-sha-256
`

// dropIn is the boot-ordering hardening (Slice 3 Action, level 1): PostgreSQL
// waits for the idmapped disk-device mount so it can't auto-start before its
// data dir is mounted. systemd tracks the bind mount as a passive
// var-lib-postgresql.mount; the packaged unit doesn't declare this dependency.
const dropIn = `[Unit]
RequiresMountsFor=/var/lib/postgresql
`

// confDir is the per-cluster config directory.
func confDir() string {
	return fmt.Sprintf("/etc/postgresql/%s/%s", Version, Cluster)
}

// GoldenBuildScript installs PostgreSQL 17 from the PGDG apt repo, writes the
// boot-ordering drop-in, and drops the auto-created cluster so the published
// image ships PG *binaries + drop-in only* — each backend then creates its own
// cluster on its XFS slot (ClusterScript). This is the curl|gpg|apt cost paid
// ONCE per image instead of once per backend (minutes → seconds).
func GoldenBuildScript() string {
	return `set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

apt-get update -q
apt-get install -y curl gnupg lsb-release

curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc \
    | gpg --dearmor -o /etc/apt/trusted.gpg.d/pgdg.gpg
echo "deb https://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" \
    > /etc/apt/sources.list.d/pgdg.list
apt-get update -q
apt-get install -y --no-install-recommends postgresql-` + Version + `

# Boot-ordering drop-in: PG waits for its data mount (idmapped disk device).
mkdir -p /etc/systemd/system/` + Unit + `.service.d
cat > /etc/systemd/system/` + Unit + `.service.d/10-require-data-mount.conf <<'DROPIN'
` + dropIn + `DROPIN

# Ship binaries only: each backend initdb's its own cluster onto its XFS slot.
if pg_lsclusters -h | grep -q '^` + Version + `  *` + Cluster + `'; then
    pg_dropcluster --stop ` + Version + ` ` + Cluster + `
fi
systemctl disable ` + Unit + ` >/dev/null 2>&1 || true
`
}

// ClusterScript creates the slot's cluster (initdb onto the bind-mounted XFS
// data dir), writes the dev config + pg_hba, and starts PostgreSQL. It is
// idempotent: if the cluster already exists it only re-asserts config and start.
func ClusterScript() string {
	dir := confDir()
	return `set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

# The data dir is a fresh (idmapped) bind mount; make its top traversable so the
# postgres user can reach the cluster it is about to create.
chmod 0755 ` + DataPath + `

if ! pg_lsclusters -h | awk '{print $1" "$2}' | grep -qx '` + Version + ` ` + Cluster + `'; then
    pg_createcluster ` + Version + ` ` + Cluster + `
fi

mkdir -p ` + dir + `/conf.d
cat > ` + dir + `/conf.d/99-dev.conf <<'PGCONF'
` + confDev + `PGCONF
cat > ` + dir + `/pg_hba.conf <<'PGHBA'
` + pgHBA + `PGHBA

systemctl enable ` + Unit + ` >/dev/null 2>&1 || true
systemctl restart ` + Unit + `
`
}

// RoleDBScript creates the login role and its database. Passwords are escaped for
// the SQL string literal (doubling single quotes).
func RoleDBScript(user, db, password string) string {
	return `set -euo pipefail
su - postgres -c 'psql -v ON_ERROR_STOP=1' <<'SQL'
DO $$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = ` + sqlLit(user) + `) THEN
    CREATE ROLE ` + ident(user) + ` WITH LOGIN PASSWORD ` + sqlLit(password) + ` SUPERUSER;
  END IF;
END $$;
SELECT 'CREATE DATABASE ` + ident(db) + ` OWNER ` + ident(user) + `'
 WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = ` + sqlLit(db) + `)\gexec
SQL
`
}

// sqlLit renders a single-quoted SQL string literal.
func sqlLit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// ident renders a double-quoted SQL identifier.
func ident(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
