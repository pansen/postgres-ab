package track

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
)

// ----- active pointer --------------------------------------------------------

// ActiveSlot returns the active machine slot, defaulting to "a" when the row is
// somehow absent (mirrors the historical `cat … || echo a` contract so callers
// never have to special-case a fresh store).
func (d *DB) ActiveSlot(ctx context.Context) string {
	var slot string
	err := d.sql.QueryRowContext(ctx, "SELECT slot FROM active WHERE id = 1").Scan(&slot)
	if err != nil || (slot != "a" && slot != "b") {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			d.log.Warn("reading active slot failed — assuming 'a'", "err", err)
		}
		return "a"
	}
	return slot
}

// StagingSlot returns the non-active slot.
func (d *DB) StagingSlot(ctx context.Context) string {
	if d.ActiveSlot(ctx) == "a" {
		return "b"
	}
	return "a"
}

// SetActive is the CHOKEPOINT for the active pointer: it writes the DB (source
// of truth) and then mirrors the legacy var/active-machine file the resident Go
// forwarder still polls. Rejects anything but a/b.
func (d *DB) SetActive(ctx context.Context, slot string) error {
	if slot != "a" && slot != "b" {
		return fmt.Errorf("track: active slot must be a or b (got %q)", slot)
	}
	_, err := d.sql.ExecContext(ctx,
		"INSERT INTO active(id, slot) VALUES (1, ?) ON CONFLICT(id) DO UPDATE SET slot = excluded.slot",
		slot)
	if err != nil {
		return fmt.Errorf("track: set active=%s: %w", slot, err)
	}
	d.log.Info("active slot set", "slot", slot)
	d.mirrorActive(slot)
	return nil
}

// seedMachineIPsIfEmpty backfills the machine table from the legacy
// var/machine-ip-{a,b} mirror files when it has NO rows — so an
// install/reconcile right after a fresh init, a schema reset, or (for an
// already-created empty DB) any open has targets immediately, without waiting
// for the next `refresh` to re-discover IPs. IPs are a cache; the mirror files
// are the last-known-good the Go forwarder already routes on. A no-op once any
// IP exists (refresh then overwrites with live values). Best-effort: a failure
// here never blocks Open.
func (d *DB) seedMachineIPsIfEmpty(ctx context.Context) {
	if d.opts.IPMirror == nil {
		return
	}
	var n int
	if err := d.sql.QueryRowContext(ctx, "SELECT COUNT(*) FROM machine").Scan(&n); err != nil || n > 0 {
		return
	}
	for _, slot := range []string{"a", "b"} {
		b, err := os.ReadFile(d.opts.IPMirror(slot))
		if err != nil {
			continue
		}
		ip := trim(string(b))
		if ip == "" {
			continue
		}
		if _, err := d.sql.ExecContext(ctx,
			"INSERT INTO machine(slot, ip, updated_unix) VALUES (?, ?, ?) ON CONFLICT(slot) DO NOTHING",
			slot, ip, nowUnix()); err != nil {
			d.log.Warn("seeding machine ip from legacy file failed", "slot", slot, "err", err)
			continue
		}
		d.log.Info("adopted machine ip from existing deployment's legacy file", "slot", slot, "ip", ip)
	}
}

// ----- machine IPs -----------------------------------------------------------

// MachineIP returns the cached eth0 IP for a slot, or "" when unknown (machine
// down / never reported).
func (d *DB) MachineIP(ctx context.Context, slot string) string {
	var ip string
	err := d.sql.QueryRowContext(ctx, "SELECT ip FROM machine WHERE slot = ?", slot).Scan(&ip)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			d.log.Warn("reading machine ip failed", "slot", slot, "err", err)
		}
		return ""
	}
	return ip
}

// SetMachineIP is the CHOKEPOINT for a machine's IP cache: DB first, then the
// legacy var/machine-ip-<slot> mirror. An empty IP is ignored (a failed
// discovery must not clobber a good cached value — the same guard the old
// writeMachineIPFile had).
func (d *DB) SetMachineIP(ctx context.Context, slot, ip string) error {
	if ip == "" {
		d.log.Debug("skipping empty machine ip write", "slot", slot)
		return nil
	}
	_, err := d.sql.ExecContext(ctx,
		"INSERT INTO machine(slot, ip, updated_unix) VALUES (?, ?, ?) "+
			"ON CONFLICT(slot) DO UPDATE SET ip = excluded.ip, updated_unix = excluded.updated_unix",
		slot, ip, nowUnix())
	if err != nil {
		return fmt.Errorf("track: set machine ip slot=%s: %w", slot, err)
	}
	d.log.Info("machine ip cached", "slot", slot, "ip", ip)
	d.mirrorIP(slot, ip)
	return nil
}

// ForgetMachine clears a slot's tracked IP — the DB row AND the legacy mirror
// file — used when the machine is deleted (staging purge) so neither forwarder
// keeps routing at its now-dead address. The DB delete is authoritative; the
// mirror removal is best-effort. After this, DesiredTargets yields an empty
// target for the slot, so a reconcile tears the socat listener down (endpoint
// honestly "down" rather than dialing a corpse).
func (d *DB) ForgetMachine(ctx context.Context, slot string) error {
	if _, err := d.sql.ExecContext(ctx, "DELETE FROM machine WHERE slot = ?", slot); err != nil {
		return fmt.Errorf("track: forget machine slot=%s: %w", slot, err)
	}
	d.log.Info("forgot machine ip (machine deleted)", "slot", slot)
	if d.opts.IPMirror != nil {
		if p := d.opts.IPMirror(slot); p != "" {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				d.log.Warn("removing legacy ip mirror failed", "slot", slot, "file", p, "err", err)
			}
		}
	}
	return nil
}

// ----- proxy targets (reconcile support) -------------------------------------

// Target is one role's desired socat upstream: the client port it listens on
// and the "ip:port" it forwards to ("" = unroutable, machine down).
type Target struct {
	Role   string // "active" | "staging"
	Port   int    // host client port (5444 / 5445)
	Target string // "ip:port" or ""
}

// DesiredTargets computes, inside a single read snapshot, what the socat proxy
// SHOULD point at right now: the active machine's IP behind the active port and
// the staging machine's IP behind the staging port. This is the reconciler's
// input — one atomic read of active+IPs so it can never mix a pre-promote slot
// with post-promote IPs.
func (d *DB) DesiredTargets(ctx context.Context, activePort, stagingPort, backendPort int) ([]Target, error) {
	tx, err := d.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("track: begin desired-targets: %w", err)
	}
	defer tx.Rollback()

	var activeSlot string
	if err := tx.QueryRowContext(ctx, "SELECT slot FROM active WHERE id = 1").Scan(&activeSlot); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("track: read active for targets: %w", err)
		}
		activeSlot = "a"
	}
	stagingSlot := "b"
	if activeSlot == "b" {
		stagingSlot = "a"
	}
	// A REAL error (busy, I/O) must fail the reconcile — never be flattened to
	// "" ("machine down"), which the reconciler acts on by tearing the listener
	// down. Only a genuinely absent row means unroutable.
	ip := func(slot string) (string, error) {
		var v string
		err := tx.QueryRowContext(ctx, "SELECT ip FROM machine WHERE slot = ?", slot).Scan(&v)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("track: read ip slot=%s: %w", slot, err)
		}
		return v, nil
	}
	mk := func(role, slot string, port int) (Target, error) {
		host, err := ip(slot)
		if err != nil {
			return Target{}, err
		}
		t := ""
		if host != "" {
			t = net.JoinHostPort(host, strconv.Itoa(backendPort))
		}
		return Target{Role: role, Port: port, Target: t}, nil
	}
	active, err := mk("active", activeSlot, activePort)
	if err != nil {
		return nil, err
	}
	staging, err := mk("staging", stagingSlot, stagingPort)
	if err != nil {
		return nil, err
	}
	return []Target{active, staging}, nil
}

// RecordApplied stamps proxy_target with what the reconciler VERIFIED it applied
// to launchd. It is observability only — the durable record of what socat runs
// is the plist on disk — so this runs AFTER the launchd mutation succeeds, never
// as the decision input (a crash between apply and record leaves the plist
// correct and this table merely stale, which the next reconcile self-heals).
func (d *DB) RecordApplied(ctx context.Context, t Target) error {
	_, err := d.sql.ExecContext(ctx,
		"INSERT INTO proxy_target(role, port, target, updated_unix) VALUES (?, ?, ?, ?) "+
			"ON CONFLICT(role) DO UPDATE SET port = excluded.port, target = excluded.target, updated_unix = excluded.updated_unix",
		t.Role, t.Port, t.Target, nowUnix())
	if err != nil {
		return fmt.Errorf("track: record applied target role=%s: %w", t.Role, err)
	}
	return nil
}

// AppliedTargets returns the last-recorded proxy targets for `pgdev proxy
// status`. Absent roles are simply missing from the map.
func (d *DB) AppliedTargets(ctx context.Context) (map[string]Target, error) {
	rows, err := d.sql.QueryContext(ctx, "SELECT role, port, target FROM proxy_target")
	if err != nil {
		return nil, fmt.Errorf("track: read applied targets: %w", err)
	}
	defer rows.Close()
	out := map[string]Target{}
	for rows.Next() {
		var t Target
		if err := rows.Scan(&t.Role, &t.Port, &t.Target); err != nil {
			return nil, err
		}
		out[t.Role] = t
	}
	return out, rows.Err()
}

// ----- legacy flat-file mirrors ----------------------------------------------
//
// These keep the still-running Go forwarder (which reads files, not the DB)
// consistent with the authoritative DB during the integration phase. They are
// best-effort: a mirror failure is logged but never fails the DB write, because
// the DB is the source of truth and the mirror is a courtesy to a component we
// are removing. All of this deletes with the Go forwarder (doc/issues/0004).

func (d *DB) mirrorActive(slot string) {
	if d.opts.ActiveMirror == "" {
		return
	}
	if err := d.writeMirror(d.opts.ActiveMirror, slot+"\n"); err != nil {
		d.log.Warn("mirroring active slot to legacy file failed", "file", d.opts.ActiveMirror, "err", err)
		return
	}
	d.log.Debug("mirrored active slot to legacy file", "file", d.opts.ActiveMirror, "slot", slot)
}

func (d *DB) mirrorIP(slot, ip string) {
	if d.opts.IPMirror == nil {
		return
	}
	path := d.opts.IPMirror(slot)
	if path == "" {
		return
	}
	if err := d.writeMirror(path, ip+"\n"); err != nil {
		d.log.Warn("mirroring machine ip to legacy file failed", "file", path, "err", err)
		return
	}
	d.log.Debug("mirrored machine ip to legacy file", "file", path, "slot", slot, "ip", ip)
}

// writeMirror does the atomic temp+rename the old writers did, then best-effort
// chowns the result back to the invoking macOS user (the daemon path wrote these
// as root; keeping the chown preserves host-readability parity).
func (d *DB) writeMirror(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "track-mirror.*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op after a successful rename
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// CreateTemp makes 0600; the forwarder polls these from the user session, so
	// a root-run pgdev must leave them world-readable or the forwarder can no
	// longer read the pointer/IP and silently keeps routing on stale values.
	if err := os.Chmod(name, 0o644); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	d.chown(path)
	return nil
}

func (d *DB) chown(path string) {
	if d.opts.MirrorUID == "" || d.opts.MirrorGID == "" {
		return
	}
	uid, err1 := strconv.Atoi(d.opts.MirrorUID)
	gid, err2 := strconv.Atoi(d.opts.MirrorGID)
	if err1 != nil || err2 != nil {
		return
	}
	_ = os.Chown(path, uid, gid) // best-effort, like the shell's `|| true`
}
