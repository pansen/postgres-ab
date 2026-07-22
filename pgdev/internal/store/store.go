// Package store owns the XFS reflink data store inside the machine: slot paths,
// snapshot listing, and the small set of *idempotent* filesystem primitives the
// task engine composes into snapshot/restore transactions. It replaces the
// scripts/pg-dev-local rename dances (.tmp.$$ / .replaced.$$ / .restore-*) with
// named, unit-tested functions.
//
// The reflink clone itself is delegated to a Cloner so the transaction logic is
// testable on any filesystem (tests inject a plain recursive copy); production
// uses coreutils `cp --reflink=always` on the real XFS mount.
package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// Store is rooted at the XFS mount (e.g. /var/lib/pg-dev-local).
type Store struct {
	Root  string
	Clone Cloner
}

// Cloner copies srcDir to a fresh dstDir. dst must not exist.
type Cloner func(ctx context.Context, srcDir, dstDir string) error

// NewOSStore returns a Store whose Clone performs a real XFS reflink copy.
func NewOSStore(root string) *Store {
	return &Store{Root: root, Clone: reflinkClone}
}

// ----- paths ---------------------------------------------------------------

func (s *Store) SlotDir(slot string) string      { return filepath.Join(s.Root, slot) }
func (s *Store) Current(slot string) string      { return filepath.Join(s.SlotDir(slot), "current") }
func (s *Store) SnapshotsDir(slot string) string { return filepath.Join(s.SlotDir(slot), "snapshots") }
func (s *Store) Snapshot(slot, name string) string {
	return filepath.Join(s.SnapshotsDir(slot), name)
}
func (s *Store) JournalDir() string { return filepath.Join(s.Root, "journal") }

// EnsureLayout creates the current/ and snapshots/ directories for a slot.
func (s *Store) EnsureLayout(slot string) error {
	for _, d := range []string{s.Current(slot), s.SnapshotsDir(slot)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// RequireMounted fails unless Root is an XFS mountpoint backed by the data
// image, mirroring _require_data_store. Best-effort: skipped when findmnt is
// unavailable (e.g. host-side tests) so it never blocks the transaction logic.
func (s *Store) RequireMounted() error {
	out, err := exec.Command("findmnt", "-T", s.Root, "-n", "-o", "FSTYPE").Output()
	if err != nil {
		// findmnt not installed (e.g. host-side macOS tests): honour the best-effort
		// contract and skip rather than block the transaction logic. A findmnt that
		// ran but found no mount exits non-zero (an *exec.ExitError), which is a real
		// "not mounted" failure and falls through.
		if errors.Is(err, exec.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("%s is not mounted from the XFS data store; run 'make machine' first", s.Root)
	}
	if fstype := trim(string(out)); fstype != "xfs" {
		return fmt.Errorf("%s is %q, not xfs; run 'make machine' first", s.Root, fstype)
	}
	return nil
}

// ----- snapshot metadata ---------------------------------------------------

// Snapshot describes one checkpoint directory, ordered by modification time. The
// snapshot's mtime is stamped to "now" at creation (see TouchNow) — the reflink
// clone that builds it copies the live data dir with `cp -a`, which would
// otherwise preserve the SOURCE's mtime and make every snapshot share the PGDATA
// top-dir's timestamp — so it is reliable creation-order metadata.
type Snapshot struct {
	Name    string
	ModUnix int64
}

// List returns a slot's snapshots in creation order (oldest first). It sorts on
// the full-resolution mtime (not the truncated ModUnix) so two snapshots taken
// in the same wall-clock second still order deterministically.
func (s *Store) List(slot string) ([]Snapshot, error) {
	entries, err := os.ReadDir(s.SnapshotsDir(slot))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	type item struct {
		name string
		mod  time.Time
	}
	var items []item
	for _, e := range entries {
		if !e.IsDir() || e.Name()[0] == '.' {
			continue // skip staging/hidden transaction artifacts
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		items = append(items, item{name: e.Name(), mod: info.ModTime()})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].mod.Before(items[j].mod) })
	snaps := make([]Snapshot, len(items))
	for i, it := range items {
		snaps[i] = Snapshot{Name: it.name, ModUnix: it.mod.Unix()}
	}
	return snaps, nil
}

// TouchNow stamps a path's mtime to the current time. Snapshot creation calls it
// on the freshly-cloned checkpoint directory so List orders snapshots by real
// creation time rather than the source data dir's mtime that `cp -a` preserved.
func TouchNow(path string) error {
	now := time.Now()
	return os.Chtimes(path, now, now)
}

// After returns the snapshot names created strictly after `name` (the timeline
// that a restore of `name` would discard). Empty if `name` is the newest.
func (s *Store) After(slot, name string) ([]string, error) {
	snaps, err := s.List(slot)
	if err != nil {
		return nil, err
	}
	var after []string
	seen := false
	for _, sn := range snaps {
		if seen {
			after = append(after, sn.Name)
		}
		if sn.Name == name {
			seen = true
		}
	}
	if !seen {
		return nil, nil
	}
	return after, nil
}

// Last returns the most recent snapshot name, or "" if none.
func (s *Store) Last(slot string) (string, error) {
	snaps, err := s.List(slot)
	if err != nil || len(snaps) == 0 {
		return "", err
	}
	return snaps[len(snaps)-1].Name, nil
}

var snapshotName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// ValidateName enforces the same charset as the shell's _validate_snapshot_name.
func ValidateName(name string) error {
	if !snapshotName.MatchString(name) {
		return fmt.Errorf("snapshot names may contain only letters, digits, '.', '_' and '-' (got %q)", name)
	}
	return nil
}

// ----- idempotent filesystem primitives ------------------------------------

// Exists reports whether a path exists (following the top-level entry only).
func Exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// Move renames src to dst, idempotently. If src is gone and dst is present it is
// treated as already-moved (no-op). "both exist" and "neither exists" are
// errors — the transaction steps guard against ever calling Move in those states.
func Move(src, dst string) error {
	srcOK, dstOK := Exists(src), Exists(dst)
	switch {
	case srcOK && !dstOK:
		return os.Rename(src, dst)
	case !srcOK && dstOK:
		return nil // already moved
	case !srcOK && !dstOK:
		return fmt.Errorf("move: neither %s nor %s exists", src, dst)
	default:
		return fmt.Errorf("move: both %s and %s exist", src, dst)
	}
}

// RemoveAll deletes a path, idempotently.
func RemoveAll(path string) error { return os.RemoveAll(path) }

// MatchTopMode sets dst's top-level permission bits to match ref's. Restore uses
// it so a cloned snapshot's data dir is as traversable as the live dir it
// replaces — defending against snapshots captured with a wrong top-level mode by
// earlier buggy versions (the live dir is known-good: PostgreSQL runs on it).
func MatchTopMode(ref, dst string) error {
	fi, err := os.Stat(ref)
	if err != nil {
		return err
	}
	return os.Chmod(dst, fi.Mode().Perm())
}

// StageClone makes a fresh reflink copy of srcDir at dstDir, removing any stale
// dstDir first so a re-run (roll-forward after a crash) is idempotent.
func (s *Store) StageClone(ctx context.Context, srcDir, dstDir string) error {
	if err := os.RemoveAll(dstDir); err != nil {
		return err
	}
	return s.Clone(ctx, srcDir, dstDir)
}

// reflinkClone is the production Cloner: a same-filesystem XFS CoW copy.
func reflinkClone(ctx context.Context, srcDir, dstDir string) error {
	// Copy the directory ITSELF (dst must not exist — StageClone removed it) so
	// cp -a reproduces the source's exact ownership and permissions on the new
	// top-level dir. This matters because the slot's data dir is bind-mounted at
	// /var/lib/postgresql: a top level that isn't traversable by the postgres
	// user makes PostgreSQL fail with "data directory is not accessible".
	// (Pre-creating dst and copying contents into it would stamp it with our
	// mode, not the source's — the bug that broke restores.)
	if err := os.MkdirAll(filepath.Dir(dstDir), 0o755); err != nil {
		return err
	}
	// Why cp and not rsync: --reflink=always is what makes a snapshot cheap — an
	// XFS copy-on-write clone that is near-instant and shares blocks until written,
	// which the whole snapshot store (and its disk-space math / disk.check guard)
	// depends on. Mainline rsync has no reflink support, so `rsync -av` would do a
	// full physical byte copy of the entire PostgreSQL data dir on every snapshot —
	// slow and full-size. It also would NOT fix snapshot ordering: rsync -a implies
	// -t (preserve times), so like cp -a it copies the source's mtime onto the clone
	// (that was the ordering bug); we deliberately re-stamp the top-level dir's mtime
	// afterwards via store.TouchNow rather than switch copy tools and lose CoW.
	cmd := exec.CommandContext(ctx, "cp", "-a", "--reflink=always", "--sparse=auto", srcDir, dstDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("reflink clone %s -> %s: %w: %s", srcDir, dstDir, err, out)
	}
	return nil
}

func trim(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
