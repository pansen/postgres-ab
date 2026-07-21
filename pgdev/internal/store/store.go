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
		return fmt.Errorf("%s is not mounted from the XFS data store; run 'make machine' first", s.Root)
	}
	if fstype := trim(string(out)); fstype != "xfs" {
		return fmt.Errorf("%s is %q, not xfs; run 'make machine' first", s.Root, fstype)
	}
	return nil
}

// ----- snapshot metadata ---------------------------------------------------

// Snapshot describes one checkpoint directory, ordered by modification time
// (touched only after a successful copy, so it is reliable ordering metadata).
type Snapshot struct {
	Name    string
	ModUnix int64
}

// List returns a slot's snapshots in creation order (oldest first).
func (s *Store) List(slot string) ([]Snapshot, error) {
	entries, err := os.ReadDir(s.SnapshotsDir(slot))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var snaps []Snapshot
	for _, e := range entries {
		if !e.IsDir() || e.Name()[0] == '.' {
			continue // skip staging/hidden transaction artifacts
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		snaps = append(snaps, Snapshot{Name: e.Name(), ModUnix: info.ModTime().Unix()})
	}
	sort.SliceStable(snaps, func(i, j int) bool { return snaps[i].ModUnix < snaps[j].ModUnix })
	return snaps, nil
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
	// Trailing "/." copies directory *contents* into an existing dst, matching
	// the shell (cp -a --reflink=always --sparse=auto "$current/." "$tmp/").
	if err := os.MkdirAll(dstDir, 0o700); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "cp", "-a", "--reflink=always", "--sparse=auto",
		filepath.Join(srcDir, "."), dstDir+string(os.PathSeparator))
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
