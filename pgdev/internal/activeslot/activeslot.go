// Package activeslot owns the active/staging pointer — the one bit of durable
// role state the reconciler is a pure function of. It ports the shell's
// _active / _set_active (scripts/pg-dev-local:87-102): a single file at
// <repo>/var/active-slot holding "a" or "b", written atomically (temp+rename)
// and chowned back to the invoking macOS user so the host can read it too.
//
// The file lives on the home-mount so it stays host-visible (status can report
// the slot even with the machine down); "a" is the default when it is absent.
package activeslot

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Pointer is the active-slot file plus the ownership to stamp on writes.
type Pointer struct {
	Path     string
	UID, GID string // decimal HOST_UID/HOST_GID; empty = don't chown
}

// Get returns the active slot, defaulting to "a" when the file is missing or
// unreadable (matching the shell's `cat … || echo a`).
func (p Pointer) Get() string {
	b, err := os.ReadFile(p.Path)
	if err != nil {
		return "a"
	}
	switch s := strings.TrimSpace(string(b)); s {
	case "a", "b":
		return s
	default:
		return "a"
	}
}

// Staging returns the non-active slot.
func (p Pointer) Staging() string {
	if p.Get() == "a" {
		return "b"
	}
	return "a"
}

// Set writes the active slot atomically and best-effort chowns it back to the
// host user. It rejects anything but "a"/"b".
func (p Pointer) Set(slot string) error {
	if slot != "a" && slot != "b" {
		return fmt.Errorf("active slot must be a or b (got %q)", slot)
	}
	dir := filepath.Dir(p.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "active-slot.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.WriteString(slot + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, p.Path); err != nil {
		return err
	}
	p.chown(dir)
	p.chown(p.Path)
	return nil
}

func (p Pointer) chown(path string) {
	if p.UID == "" || p.GID == "" {
		return
	}
	uid, err1 := strconv.Atoi(p.UID)
	gid, err2 := strconv.Atoi(p.GID)
	if err1 != nil || err2 != nil {
		return
	}
	_ = os.Chown(path, uid, gid) // best-effort, like the shell's `|| true`
}
