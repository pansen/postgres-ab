package daemon

import (
	"testing"

	"pansen.me/pgdev/internal/config"
)

// Each daemon serves exactly one backend, chosen by PG_SLOT.
func TestSlotAndContainer(t *testing.T) {
	sb := &Service{Cfg: config.Config{BackendPrefix: "pg-dev", Slot: "b"}}
	if sb.slot() != "b" || sb.container() != "pg-dev-b" {
		t.Fatalf("slot=%q container=%q", sb.slot(), sb.container())
	}
	// Unset slot defaults to "a" so a legacy/single-machine deploy still resolves.
	sa := &Service{Cfg: config.Config{BackendPrefix: "pg-dev", Slot: ""}}
	if sa.slot() != "a" || sa.container() != "pg-dev-a" {
		t.Fatalf("default slot=%q container=%q", sa.slot(), sa.container())
	}
}

// New fails fast on a misconfigured slot instead of silently defaulting to "a".
func TestNewRejectsInvalidSlot(t *testing.T) {
	if _, err := New(config.Config{Slot: "x"}, "v", nil); err == nil {
		t.Fatal("New should reject an invalid PG_SLOT")
	}
	// Unset and a/b are accepted (unset → legacy default).
	for _, s := range []string{"", "a", "b"} {
		if _, err := New(config.Config{Slot: s, DataRoot: t.TempDir()}, "v", nil); err != nil {
			t.Fatalf("New(slot=%q): %v", s, err)
		}
	}
}

// Apple's container runtime mounts /proc/sys read-only inside the machine, which
// breaks `incus network create` outright. mountedWritable is what detects it, so
// it has to read the real /proc/mounts shape — including a stacked mount, where
// the LAST entry for a path is the one in effect.
func TestMountedWritable(t *testing.T) {
	const base = "proc /proc proc rw,relatime 0 0\n" +
		"none /proc/keys devtmpfs rw,nosuid,relatime 0 0\n"

	if mountedWritable(base+"proc /proc/sys proc ro,relatime 0 0\n", "/proc/sys") {
		t.Fatal("ro /proc/sys reported writable")
	}
	if !mountedWritable(base+"proc /proc/sys proc rw,relatime 0 0\n", "/proc/sys") {
		t.Fatal("rw /proc/sys reported read-only")
	}
	// Our remount stacks on the read-only one: the later entry wins.
	stacked := base + "proc /proc/sys proc ro,relatime 0 0\nproc /proc/sys proc rw,relatime 0 0\n"
	if !mountedWritable(stacked, "/proc/sys") {
		t.Fatal("remounted /proc/sys still reported read-only")
	}
	// Not a mount point of its own: nothing says read-only, so it is writable.
	if !mountedWritable(base, "/proc/sys") {
		t.Fatal("unlisted path reported read-only")
	}
}

// The unmasked proc/sysfs mounts must be detected as already present, or every
// daemon restart would stack another mount on the same path.
func TestMountedAs(t *testing.T) {
	const mounts = "proc /proc proc rw,relatime 0 0\n" +
		"proc /run/pgdev/proc proc rw,relatime 0 0\n" +
		"tmpfs /run/pgdev/sys tmpfs rw,relatime 0 0\n"

	if !mountedAs(mounts, "/run/pgdev/proc", "proc") {
		t.Fatal("existing proc mount not detected")
	}
	// Right path, wrong filesystem: not the mount we need.
	if mountedAs(mounts, "/run/pgdev/sys", "sysfs") {
		t.Fatal("tmpfs accepted as the unmasked sysfs mount")
	}
	if mountedAs(mounts, "/run/pgdev/missing", "proc") {
		t.Fatal("absent mount reported as present")
	}
}
