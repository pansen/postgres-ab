package store

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"pansen.me/pgdev/internal/logx"
)

// Bootstrap ensures the sparse XFS reflink data store exists, is mounted at Root
// via a loop device, has reflinks enabled, and is grown to `size` if it was
// raised. It ports scripts/apple-machine-init:101-186 (§5.6 of issues/0001) into
// a typed, idempotent function the daemon runs at start (pgdevd bootstrap).
//
// The stock Apple kernel has XFS + loop + reflink but no Btrfs/ZFS/dm-thin, so a
// sparse XFS loop image supplies the CoW reflinks the snapshot engine relies on.
// Runs as root inside the machine; a no-op once everything is already in place.
func Bootstrap(ctx context.Context, image, root, size string, log logx.Func) error {
	l := logx.Or(log)

	if !exists(image) {
		if err := createImage(ctx, image, size, l); err != nil {
			return err
		}
	}

	l("mounting and verifying %s...", root)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	if err := ensureFstab(image, root); err != nil {
		return err
	}
	if !mountpoint(ctx, root) {
		if err := mountChecked(ctx, image, root); err != nil {
			return err
		}
	}
	if err := verifyReflink(ctx, root); err != nil {
		return err
	}
	return grow(ctx, image, root, size, l)
}

// createImage stages a sparse image on a loop device, makes an XFS filesystem
// with reflinks, then atomically moves it into place (ports 101-121).
func createImage(ctx context.Context, image, size string, l logx.Func) error {
	l("creating sparse %s XFS data store at %s...", size, image)
	tmp := image + ".creating"
	_ = os.Remove(tmp)
	if err := runv(ctx, "truncate", "-s", size, tmp); err != nil {
		return err
	}
	loop, err := outv(ctx, "losetup", "--find", "--show", tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	loop = strings.TrimSpace(loop)
	if err := runv(ctx, "mkfs.xfs", "-m", "reflink=1", loop); err != nil {
		_ = runv(ctx, "losetup", "--detach", loop)
		_ = os.Remove(tmp)
		return err
	}
	if err := runv(ctx, "losetup", "--detach", loop); err != nil {
		return err
	}
	if err := os.Rename(tmp, image); err != nil {
		return err
	}
	return nil
}

// mountChecked validates the image is XFS via a read-only loop probe before
// mounting it, refusing to mount a corrupt/foreign image (ports 128-138).
func mountChecked(ctx context.Context, image, root string) error {
	loop, err := outv(ctx, "losetup", "--find", "--show", "--read-only", image)
	if err != nil {
		return err
	}
	loop = strings.TrimSpace(loop)
	typ, _ := outv(ctx, "blkid", "--probe", "--output", "value", "--match-tag", "TYPE", loop)
	_ = runv(ctx, "losetup", "--detach", loop)
	if strings.TrimSpace(typ) != "xfs" {
		return fmt.Errorf("existing %s is not a valid XFS filesystem; refusing to overwrite it", image)
	}
	return runv(ctx, "mount", root)
}

func verifyReflink(ctx context.Context, root string) error {
	out, err := outv(ctx, "xfs_info", root)
	if err != nil || !strings.Contains(out, "reflink=1") {
		return fmt.Errorf("%s is not an XFS filesystem with reflinks enabled", root)
	}
	// Fail early if the running kernel/FS cannot actually perform a CoW copy.
	src := root + "/.reflink-probe-source"
	dst := root + "/.reflink-probe-copy"
	defer func() { _ = os.Remove(src); _ = os.Remove(dst) }()
	if err := os.WriteFile(src, []byte("reflink probe\n"), 0o600); err != nil {
		return err
	}
	if err := runv(ctx, "cp", "--reflink=always", src, dst); err != nil {
		return fmt.Errorf("XFS reflink copies are unavailable on %s: %w", root, err)
	}
	return nil
}

// grow raises the backing image and XFS filesystem in place when size was
// increased (ports 157-186). Shrinking is unsupported; it warns and stays.
func grow(ctx context.Context, image, root, size string, l logx.Func) error {
	want, err := parseIEC(size)
	if err != nil {
		l("WARNING: could not parse data disk size %q; skipping resize check", size)
		return nil
	}
	fi, err := os.Stat(image)
	if err != nil {
		return err
	}
	have := fi.Size()
	switch {
	case want > have:
		l("growing data store to %s...", size)
		if err := runv(ctx, "truncate", "-s", size, image); err != nil {
			return err
		}
		loop, err := outv(ctx, "losetup", "-j", image)
		if err != nil {
			return err
		}
		dev, _, _ := strings.Cut(strings.TrimSpace(loop), ":")
		if dev == "" {
			return fmt.Errorf("could not find the loop device backing %s", image)
		}
		if err := runv(ctx, "losetup", "-c", dev); err != nil {
			return err
		}
		if err := runv(ctx, "xfs_growfs", root); err != nil {
			return err
		}
	case want < have:
		l("WARNING: %s is smaller than the current data store; shrinking is unsupported, staying at the current size", size)
	}
	return nil
}

// EnsureLayout creates both slots' current/ and snapshots/ directories, matching
// apple-machine-init:206-207.
func (s *Store) EnsureSlots() error {
	for _, slot := range []string{"a", "b"} {
		if err := s.EnsureLayout(slot); err != nil {
			return err
		}
	}
	return nil
}

// RemoveSlotData deletes a slot's whole tree (current + snapshots), guarding the
// path so a bug can never remove something outside <root>/<slot> (ports the
// cmd_down safety check).
func (s *Store) RemoveSlotData(slot string) error {
	if slot != "a" && slot != "b" {
		return fmt.Errorf("refusing to remove unexpected data path for slot %q", slot)
	}
	return os.RemoveAll(s.SlotDir(slot))
}

// ----- helpers -------------------------------------------------------------

func exists(path string) bool { _, err := os.Lstat(path); return err == nil }

func mountpoint(ctx context.Context, path string) bool {
	return exec.CommandContext(ctx, "mountpoint", "-q", path).Run() == nil
}

// ensureFstab appends the loop,nofail fstab entry if absent (so a reboot
// re-mounts it and systemd tracks it as a mount unit for ordering).
func ensureFstab(image, root string) error {
	entry := fmt.Sprintf("%s %s xfs loop,nofail 0 0", image, root)
	b, err := os.ReadFile("/etc/fstab")
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == entry {
			return nil
		}
	}
	f, err := os.OpenFile("/etc/fstab", os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(entry + "\n")
	return err
}

// parseIEC parses IEC sizes like "140G", "180G", "1024M" into bytes.
func parseIEC(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'K', 'k':
		mult, s = 1<<10, s[:len(s)-1]
	case 'M', 'm':
		mult, s = 1<<20, s[:len(s)-1]
	case 'G', 'g':
		mult, s = 1<<30, s[:len(s)-1]
	case 'T', 't':
		mult, s = 1<<40, s[:len(s)-1]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, err
	}
	return n * mult, nil
}

func runv(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func outv(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return string(out), nil
}
