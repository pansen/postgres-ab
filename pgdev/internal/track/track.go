// Package track is the host-side (macOS) source of truth for the small set of
// "machine tracking" facts the client proxy routes on: which machine is active,
// each machine's drifting eth0 IP, and the reconciled upstream targets the socat
// proxy points at. It replaces the loose flat files (var/active-machine,
// var/machine-ip-{a,b}) with a single SQLite database (var/pgdev.db) so a
// reconcile is one atomic, transactional decision instead of a scatter of racing
// os.WriteFile calls.
//
// Why SQLite and not files: the socat proxy (internal/socatproxy) cannot
// re-point itself — every promote/IP drift must rewrite+reload a launchd job —
// so two concurrent `pgdev` commands reconciling at once is a real race with a
// real hazard (a stale mapping → pg_restore hitting the wrong DB). A transaction
// gives that reconcile exactly one clear outcome.
//
// Flat-file mirrors (see doc/issues/0004 §5.2): every state write here is a
// CHOKEPOINT that writes the DB first and then mirrors the flat file. The
// mirrors outlived the Go forwarder they were introduced for because `pgdev`
// itself (internal/activeslot, machineIP) and the Makefile's ACTIVE_SLOT still
// READ them; moving those readers onto the DB is what retires the files.
package track

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// schemaVersion is bumped whenever the schema below changes. Because all rows
// are reconstructible tracking data (IPs are re-discovered, the active pointer
// is carried over — see migrate), a version bump does NOT need hand-written
// migration SQL: Open detects the mismatch, drops every table, and recreates at
// the current version. That is the "schema change shuts everything down and
// re-derives" contract, minus real migration tooling we don't want to own.
const schemaVersion = 1

// schemaSQL is the whole schema, (re)applied verbatim after a reset. Slots are
// constrained to a/b at the storage layer so a bad write can never smuggle a
// third role past the router.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS schema_meta (
    id      INTEGER PRIMARY KEY CHECK (id = 1),
    version INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS active (
    id   INTEGER PRIMARY KEY CHECK (id = 1),
    slot TEXT NOT NULL CHECK (slot IN ('a','b'))
);
CREATE TABLE IF NOT EXISTS machine (
    slot         TEXT PRIMARY KEY CHECK (slot IN ('a','b')),
    ip           TEXT NOT NULL,
    updated_unix INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS proxy_target (
    role         TEXT PRIMARY KEY CHECK (role IN ('active','staging')),
    port         INTEGER NOT NULL,
    target       TEXT NOT NULL,
    updated_unix INTEGER NOT NULL
);
`

// Options configures Open. The mirror paths keep the flat files the CLI and the
// Makefile still read in step with the DB; leave them empty to disable mirroring
// (the DB-only end state, or tests).
type Options struct {
	Path         string                   // var/pgdev.db
	Logger       *slog.Logger             // structured sink; nil = slog.Default()
	ActiveMirror string                   // var/active-machine flat file ("" disables)
	IPMirror     func(slot string) string // slot -> var/machine-ip-<slot> ("" disables)
	MirrorUID    string                   // decimal HOST_UID to chown mirrors back to ("" = skip)
	MirrorGID    string                   // decimal HOST_GID
}

// DB is the opened tracking store. It is safe for concurrent use by one process
// (database/sql pools) and, via SQLite's file locking + busy_timeout, across
// processes — which is the point: several `pgdev` invocations can touch it at
// once and the transactions serialize them.
type DB struct {
	sql  *sql.DB
	log  *slog.Logger
	opts Options
}

// Open opens (creating if absent) the tracking DB, brings the schema to
// schemaVersion — dropping and recreating every table on a version mismatch,
// carrying the active slot across the reset — and returns whether such a reset
// happened. A true reset is the caller's cue to reconcile the proxy (the
// launchd side is intentionally NOT touched here: track must not import
// socatproxy or exec launchctl; Open only fixes data).
func Open(ctx context.Context, opts Options) (db *DB, reset bool, err error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "track", "db", opts.Path)

	if err := os.MkdirAll(filepath.Dir(opts.Path), 0o755); err != nil {
		return nil, false, fmt.Errorf("track: mkdir for db: %w", err)
	}

	// WAL + a generous busy_timeout so a concurrent writer waits rather than
	// erroring; foreign_keys is harmless here but conventional. journal_mode is
	// persistent, the rest are per-connection, so set them via the DSN which the
	// pool applies to every connection it opens.
	dsn := "file:" + opts.Path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, false, fmt.Errorf("track: open %s: %w", opts.Path, err)
	}
	// SQLite is single-writer; a small pool avoids "database is locked" churn
	// from database/sql racing its own connections for the write lock.
	sqldb.SetMaxOpenConns(1)
	if err := sqldb.PingContext(ctx); err != nil {
		sqldb.Close()
		return nil, false, fmt.Errorf("track: ping %s: %w", opts.Path, err)
	}

	d := &DB{sql: sqldb, log: log, opts: opts}
	log.Debug("opened tracking database", "schema_want", schemaVersion)

	reset, err = d.migrate(ctx)
	if err != nil {
		sqldb.Close()
		return nil, false, err
	}
	// Backfill IPs from the legacy mirror files when the table is empty, so a
	// proxy install/reconcile right after this open already has targets.
	d.seedMachineIPsIfEmpty(ctx)
	return d, reset, nil
}

// Close releases the underlying pool.
func (d *DB) Close() error { return d.sql.Close() }

// migrate ensures the schema is at schemaVersion. On a mismatch (or a
// first-ever open) it: reads the surviving active slot, drops every table,
// recreates the schema, stamps the version, and re-seeds active — so a schema
// bump never silently repoints traffic at machine "a". Returns whether a
// destructive reset occurred.
func (d *DB) migrate(ctx context.Context) (bool, error) {
	have, err := d.readVersion(ctx)
	if err != nil {
		return false, err
	}
	if have == schemaVersion {
		d.log.Debug("schema up to date", "version", have)
		return false, nil
	}

	// Preserve the one bit of state that is a decision, not a cache: which
	// machine is active. Prefer the (about-to-be-dropped) table, then the legacy
	// mirror file, then default to "a". On a real reset a missing slot is LOUD
	// (a wrong default is the silent-wrong-DB hazard the project exists to
	// prevent); on a first-ever init it is expected and quiet.
	isReset := have != 0
	carried := d.carryActive(ctx, isReset)

	if have == 0 {
		d.log.Info("initializing tracking schema", "version", schemaVersion, "active", carried)
	} else {
		d.log.Warn("schema version changed — dropping all tracking tables and recreating (data loss is expected; IPs re-discover, active carried over)",
			"from", have, "to", schemaVersion, "carried_active", carried)
	}

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("track: begin migrate: %w", err)
	}
	defer tx.Rollback()

	for _, t := range []string{"proxy_target", "machine", "active", "schema_meta"} {
		if _, err := tx.ExecContext(ctx, "DROP TABLE IF EXISTS "+t); err != nil {
			return false, fmt.Errorf("track: drop %s: %w", t, err)
		}
	}
	if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
		return false, fmt.Errorf("track: create schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO schema_meta(id, version) VALUES (1, ?)", schemaVersion); err != nil {
		return false, fmt.Errorf("track: stamp version: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO active(id, slot) VALUES (1, ?)", carried); err != nil {
		return false, fmt.Errorf("track: seed active: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("track: commit migrate: %w", err)
	}

	// Keep the mirror consistent with the carried value so the flat-file readers
	// agree with the freshly-reset DB.
	d.mirrorActive(carried)
	reset := have != 0
	return reset, nil
}

// readVersion returns the stored schema version, or 0 when schema_meta is absent
// (a fresh or pre-schema database). Any other error is real and returned.
func (d *DB) readVersion(ctx context.Context) (int, error) {
	var v int
	err := d.sql.QueryRowContext(ctx, "SELECT version FROM schema_meta WHERE id = 1").Scan(&v)
	switch {
	case err == nil:
		return v, nil
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case isNoSuchTable(err):
		return 0, nil
	default:
		return 0, fmt.Errorf("track: read schema version: %w", err)
	}
}

// carryActive finds the active slot to seed the fresh schema: current table →
// legacy mirror file → "a". isReset tunes the "nothing found, defaulting" log:
// LOUD on a destructive reset (we may have just lost a real decision), quiet on
// a first-ever init (starting at "a" is the documented default). Never returns
// anything but "a"/"b".
func (d *DB) carryActive(ctx context.Context, isReset bool) string {
	var slot string
	if err := d.sql.QueryRowContext(ctx, "SELECT slot FROM active WHERE id = 1").Scan(&slot); err == nil {
		if slot == "a" || slot == "b" {
			d.log.Debug("carrying active slot from table", "slot", slot)
			return slot
		}
	}
	if p := d.opts.ActiveMirror; p != "" {
		if b, err := os.ReadFile(p); err == nil {
			if s := trim(string(b)); s == "a" || s == "b" {
				d.log.Info("adopting active slot from existing deployment's legacy file", "slot", s, "file", p)
				return s
			}
		}
	}
	if isReset {
		d.log.Warn("no prior active slot found across schema reset — defaulting to 'a' (verify this is correct before promoting)")
	} else {
		d.log.Info("no prior active slot — starting at 'a' (the default)")
	}
	return "a"
}

func isNoSuchTable(err error) bool {
	return err != nil && containsFold(err.Error(), "no such table")
}

func trim(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	return s
}

func containsFold(s, sub string) bool {
	// tiny case-insensitive substring; avoids a strings import churn and the
	// error text from SQLite is stable ASCII.
	if len(sub) == 0 {
		return true
	}
	lower := func(b byte) byte {
		if b >= 'A' && b <= 'Z' {
			return b + 32
		}
		return b
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		ok := true
		for j := 0; j < len(sub); j++ {
			if lower(s[i+j]) != lower(sub[j]) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// now is a tiny seam so tests can pin timestamps; production uses the wall clock.
func nowUnix() int64 { return time.Now().Unix() }
