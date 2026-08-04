package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"pansen.me/pgdev/internal/agentapi"
	"pansen.me/pgdev/internal/pg"
	"pansen.me/pgdev/internal/store"
	"pansen.me/pgdev/internal/task"
)

// reloadSystemd asks the machine's system manager to re-read unit drop-ins.
func reloadSystemd() error {
	return exec.Command("systemctl", "daemon-reload").Run()
}

// Apple's container runtime hardens the machine's kernel filesystems, and both
// halves of that hardening break Incus (see ensureKernelMounts):
//
//   - procSys is mounted READ-ONLY, so bringing a managed bridge up fails on
//     "open /proc/sys/net/ipv6/conf/incusbr0/disable_ipv6: read-only file system".
//   - /proc and /sys carry masking overmounts (/proc/keys, /proc/timer_list,
//     /sys/firmware), which makes the kernel refuse a fresh proc/sysfs mount
//     inside a user namespace — every nested container then dies in LXC's
//     first automatic mounts. visibleProc/visibleSysfs are the unmasked mounts
//     that give the kernel's visibility check something to say yes to.
const (
	procSys      = "/proc/sys"
	visibleProc  = "/run/pgdev/proc"
	visibleSysfs = "/run/pgdev/sys"
)

// bootstrapStatePath records the last bootstrap outcome (empty file = success).
// The daemon's unit runs bootstrap as `ExecStartPre=-`, deliberately tolerating
// failure so `pgdev status` stays usable; without this file that failure would
// be invisible and resurface minutes later as a confusing downstream error (a
// half-configured Incus reports "No root device could be found" on launch).
// /run is tmpfs, so the record is per-boot, which is the right lifetime.
const bootstrapStatePath = "/run/pgdevd-bootstrap.err"

// Bootstrap ensures the machine is ready to host its one backend: the XFS reflink
// store mounted, this slot's layout present, the Incus daemon topology configured,
// and the boot-ordering drop-in for incusd installed. Runs as the systemd unit's
// ExecStartPre (`pgdevd bootstrap`) so every daemon start re-asserts it.
func (s *Service) Bootstrap(ctx context.Context) error {
	err := s.bootstrap(ctx)
	recordBootstrap(err)
	return err
}

func (s *Service) bootstrap(ctx context.Context) error {
	s.Log("bootstrapping XFS data store at %s...", s.Cfg.DataRoot)
	if err := store.Bootstrap(ctx, s.Cfg.DataImage, s.Cfg.DataRoot, s.Cfg.DataDiskSize, s.Log); err != nil {
		return err
	}
	if err := s.Store.EnsureLayout(s.slot()); err != nil {
		return err
	}
	if err := s.ensureIncusOrdering(); err != nil {
		return err
	}
	remounted, err := s.ensureKernelMounts(ctx)
	if err != nil {
		return err
	}
	s.Log("waiting for the Incus daemon...")
	if err := s.Incus.WaitReady(ctx, 90*time.Second); err != nil {
		return err
	}
	if remounted {
		// incusd has been running against a read-only /proc/sys, so any bridge it
		// tried to bring up is defined but absent. Now that the tunables are
		// writable, get it to re-create them.
		if err := s.Incus.RepairNetworks(ctx, 90*time.Second); err != nil {
			return err
		}
	}
	s.Log("configuring Incus storage, network and profile...")
	return s.Incus.EnsureTopology(ctx, s.Cfg)
}

// ensureKernelMounts repairs the two ways Apple's container runtime makes the
// machine's /proc and /sys unusable for Incus: a read-only /proc/sys (no managed
// bridge can be brought up) and masked /proc + /sys (no nested container can
// start, because the kernel only permits a fresh proc/sysfs mount inside a user
// namespace when an unmasked mount of that filesystem already exists).
//
// Applied twice over: as an incus.service drop-in, so every future boot has them
// in place before incusd starts (and before it autostarts instances), and right
// now, for the current boot where incusd is already running. It reports whether
// /proc/sys had to be remounted — the signal that incusd has been running
// against a read-only one and its networks may need repairing.
func (s *Service) ensureKernelMounts(ctx context.Context) (bool, error) {
	if err := s.ensureIncusDropIn("20-kernel-mounts.conf", kernelMountsDropIn,
		"installed incus.service drop-in (writable "+procSys+", unmasked proc/sysfs)"); err != nil {
		return false, err
	}
	if err := s.ensureVisibleMount(ctx, "proc", visibleProc); err != nil {
		return false, err
	}
	if err := s.ensureVisibleMount(ctx, "sysfs", visibleSysfs); err != nil {
		return false, err
	}
	if procSysWritable() {
		return false, nil
	}
	s.Log("%s is mounted read-only (Apple container runtime); remounting read-write for Incus...", procSys)
	if out, err := exec.CommandContext(ctx, "mount", "-o", "remount,rw", procSys).CombinedOutput(); err != nil {
		return false, fmt.Errorf("remount %s read-write: %w: %s", procSys, err, strings.TrimSpace(string(out)))
	}
	if !procSysWritable() {
		return false, fmt.Errorf("%s is still read-only after remounting it read-write", procSys)
	}
	return true, nil
}

// kernelMountsDropIn is the incus.service half of ensureKernelMounts. Every line
// is tolerant (`-`) and guarded, so a boot where one of them is already in place
// — or where a future runtime stops needing them — still starts incusd.
const kernelMountsDropIn = "[Service]\n" +
	"ExecStartPre=-/bin/mount -o remount,rw " + procSys + "\n" +
	"ExecStartPre=-/bin/sh -c 'mountpoint -q " + visibleProc + " || { mkdir -p " + visibleProc + " && mount -t proc proc " + visibleProc + "; }'\n" +
	"ExecStartPre=-/bin/sh -c 'mountpoint -q " + visibleSysfs + " || { mkdir -p " + visibleSysfs + " && mount -t sysfs sysfs " + visibleSysfs + "; }'\n"

// ensureVisibleMount parks one pristine mount of fstype at target, where the
// kernel's "fully visible" check can find it. Idempotent: mounting again would
// stack a second mount on every daemon restart, so an existing one is left be.
func (s *Service) ensureVisibleMount(ctx context.Context, fstype, target string) error {
	if mountedAs(readMounts(), target, fstype) {
		return nil
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	s.Log("mounting an unmasked %s at %s (nested containers cannot mount their own otherwise)...", fstype, target)
	if out, err := exec.CommandContext(ctx, "mount", "-t", fstype, fstype, target).CombinedOutput(); err != nil {
		return fmt.Errorf("mount %s at %s: %w: %s", fstype, target, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// readMounts returns /proc/mounts, or "" when it cannot be read.
func readMounts() string {
	b, _ := os.ReadFile("/proc/mounts")
	return string(b)
}

// procSysWritable reports whether /proc/sys is currently mounted read-write. An
// unreadable /proc/mounts counts as not-writable: attempting the remount is
// harmless, and skipping it would leave Incus broken.
func procSysWritable() bool {
	mounts := readMounts()
	if mounts == "" {
		return false
	}
	return mountedWritable(mounts, procSys)
}

// mountedWritable parses /proc/mounts for target's mount options. Later entries
// shadow earlier ones (a mount stacked on the same path wins), so the last match
// decides; a target that is not a mount point of its own is writable exactly
// when nothing says otherwise.
func mountedWritable(mounts, target string) bool {
	writable := true
	for _, f := range mountFields(mounts, target) {
		writable = slices.Contains(strings.Split(f[3], ","), "rw")
	}
	return writable
}

// mountedAs reports whether target is currently mounted with the given fstype.
func mountedAs(mounts, target, fstype string) bool {
	for _, f := range mountFields(mounts, target) {
		if f[2] == fstype {
			return true
		}
	}
	return false
}

// mountFields yields the /proc/mounts entries whose mount point is target, in
// file order (device, mount point, fstype, options, …).
func mountFields(mounts, target string) [][]string {
	var out [][]string
	for _, line := range strings.Split(mounts, "\n") {
		f := strings.Fields(line)
		if len(f) >= 4 && f[1] == target {
			out = append(out, f)
		}
	}
	return out
}

// ensureIncusOrdering is boot-ordering hardening level 2: incus.service must not
// start before the XFS loop mount exists, or a machine reboot could bring incusd
// (and the backend whose disk source lives on that mount) up too early. Written
// as a drop-in so systemd orders incusd after var-lib-pg\x2ddev\x2dlocal.mount.
func (s *Service) ensureIncusOrdering() error {
	return s.ensureIncusDropIn("10-after-pgstore.conf",
		"[Unit]\nRequiresMountsFor="+s.Cfg.DataRoot+"\n",
		"installed incus.service ordering drop-in (After "+s.Cfg.DataRoot+" mount)")
}

// ensureIncusDropIn writes one incus.service drop-in idempotently, reloading
// systemd only when the content actually changed.
func (s *Service) ensureIncusDropIn(name, content, logLine string) error {
	dir := "/etc/systemd/system/incus.service.d"
	path := dir + "/" + name
	if b, err := os.ReadFile(path); err == nil && string(b) == content {
		return nil // already in place; skip the daemon-reload
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return err
	}
	s.Log("%s", logLine)
	// Best-effort: a reload lets it take effect without waiting for the next boot.
	_ = reloadSystemd()
	return nil
}

// recordBootstrap persists the bootstrap outcome for Status to report. Failing
// to write it is not itself an error: the record is a diagnostic, and losing it
// must not turn a healthy bootstrap into a failed one.
func recordBootstrap(err error) {
	if err == nil {
		_ = os.Remove(bootstrapStatePath)
		return
	}
	_ = os.WriteFile(bootstrapStatePath, []byte(err.Error()), 0o644)
}

// LastBootstrapError returns the error the most recent bootstrap failed with, or
// "" if it succeeded (or never ran this boot).
func LastBootstrapError() string {
	b, err := os.ReadFile(bootstrapStatePath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Up provisions this machine's one backend from an empty Incus host: golden
// image, the backend + its XFS slot + cluster + role/db + `initial` snapshot,
// then the eth0 forward device (via reconcile). Refuses if the backend already
// exists (run Down first). There is no proxy container and no active pointer —
// role is a host concern.
func (s *Service) Up(ctx context.Context) (agentapi.StatusResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.Store.RequireMounted(); err != nil {
		return agentapi.StatusResponse{}, err
	}
	c := s.container()
	if s.Incus.Exists(ctx, c) {
		return agentapi.StatusResponse{}, fmt.Errorf("container %q already exists — run down first", c)
	}

	// Re-assert the Incus topology instead of trusting bootstrap: its failures are
	// tolerated by the unit (ExecStartPre=-), and a half-configured daemon fails
	// here with a symptom that names none of the cause ("No root device could be
	// found"). Idempotent, so on a healthy machine this is four no-op queries.
	if err := s.Incus.EnsureTopology(ctx, s.Cfg); err != nil {
		return agentapi.StatusResponse{}, fmt.Errorf("incus topology (bootstrap left it incomplete): %w", err)
	}
	if err := s.ensureGolden(ctx); err != nil {
		return agentapi.StatusResponse{}, fmt.Errorf("golden image: %w", err)
	}
	if err := s.provisionBackend(ctx, s.slot()); err != nil {
		return agentapi.StatusResponse{}, fmt.Errorf("provision %s: %w", c, err)
	}
	if _, err := s.reconcile(ctx); err != nil {
		return agentapi.StatusResponse{}, err
	}
	s.Log("pg-dev backend %s ready.", c)
	return s.Status(ctx)
}

// ensureGolden builds the pg-dev-base image once if it is missing: a throwaway
// container installs PostgreSQL 17 (the slow curl|gpg|apt path) and is published.
func (s *Service) ensureGolden(ctx context.Context) error {
	if s.Incus.ImageExists(ctx, s.Cfg.GoldenImage) {
		return nil
	}
	build := "pg-golden-build"
	s.Log("building golden image %s (installs PostgreSQL 17; ~minutes, once)...", s.Cfg.GoldenImage)
	_ = s.Incus.Delete(ctx, build) // clear any stale build container
	if err := s.Incus.Launch(ctx, build, s.Cfg.BaseImage); err != nil {
		return err
	}
	defer func() { _ = s.Incus.Delete(context.WithoutCancel(ctx), build) }()
	if err := s.Incus.WaitIPv4(ctx, build, "", 2*time.Minute); err != nil {
		return err
	}
	if err := s.Incus.WaitSystemd(ctx, build); err != nil {
		return err
	}
	if _, err := s.Incus.ExecScript(ctx, build, pg.GoldenBuildScript()); err != nil {
		return err
	}
	if err := s.Incus.StopContainer(ctx, build); err != nil {
		return err
	}
	return s.Incus.Publish(ctx, build, s.Cfg.GoldenImage)
}

// provisionBackend launches the backend from the golden image, attaches its XFS
// slot, creates the cluster + role/db, and snapshots `initial`. No static-IP
// pin: the backend is reached through the eth0 forward device (added by
// reconcile), which connects to PostgreSQL on the container's loopback, so the
// container's own bridge address is irrelevant and may drift freely.
func (s *Service) provisionBackend(ctx context.Context, slot string) error {
	c := s.Cfg.Container(slot)
	if err := s.Store.EnsureLayout(slot); err != nil {
		return err
	}
	// Make the data dir top traversable so the postgres user can reach the
	// cluster it will create on this bind-mounted slot.
	_ = os.Chmod(s.Store.Current(slot), 0o755)

	if err := s.Incus.Launch(ctx, c, s.Cfg.GoldenImage); err != nil {
		return err
	}
	if err := s.Incus.AddDiskDevice(ctx, c, "pgdata", s.Store.Current(slot), pg.DataPath); err != nil {
		return err
	}
	if err := s.Incus.WaitIPv4(ctx, c, "", time.Minute); err != nil {
		return err
	}
	if err := s.Incus.WaitSystemd(ctx, c); err != nil {
		return err
	}
	s.Log("creating PostgreSQL cluster on %s...", c)
	if _, err := s.Incus.ExecScript(ctx, c, pg.ClusterScript()); err != nil {
		return err
	}
	if err := s.Incus.EnsurePGRunning(ctx, c); err != nil {
		return err
	}
	s.Log("creating role %q and database %q on %s...", s.Cfg.PGUser, s.Cfg.PGDB, c)
	if _, err := s.Incus.ExecScript(ctx, c, pg.RoleDBScript(s.Cfg.PGUser, s.Cfg.PGDB, s.Cfg.PGPassword)); err != nil {
		return err
	}

	s.Log("snapshotting %s @ initial...", c)
	t, err := s.Ops.Snapshot(ctx, slot, "initial", false)
	if err != nil {
		return err
	}
	return task.Run(ctx, s.Journal, t)
}

// Down deletes this machine's backend, then removes its XFS data tree. Data is
// only removed once the container is gone and the store is mounted from the
// expected image (the cmd_down safety guard).
func (s *Service) Down(ctx context.Context) (agentapi.OpResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c := s.container()
	if err := s.Incus.Delete(ctx, c); err != nil {
		return agentapi.OpResponse{}, fmt.Errorf("delete %s: %w (refusing to remove data)", c, err)
	}
	if s.Store.RequireMounted() == nil {
		if err := s.Store.RemoveSlotData(s.slot()); err != nil {
			return agentapi.OpResponse{}, err
		}
	} else {
		s.Log("WARNING: %s is not mounted from the XFS store; left the data tree alone", s.Cfg.DataRoot)
	}
	return agentapi.OpResponse{Message: fmt.Sprintf("%s deleted and its data tree removed", c)}, nil
}
