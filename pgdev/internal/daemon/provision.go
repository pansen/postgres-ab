package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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

// Bootstrap ensures the machine is ready to host backends: the XFS reflink store
// mounted, the Incus daemon topology configured, and the boot-ordering drop-in
// for incusd installed. It ports scripts/apple-machine-init and is run as the
// systemd unit's ExecStartPre (`pgdevd bootstrap`) so every daemon start
// re-asserts it, idempotently.
func (s *Service) Bootstrap(ctx context.Context) error {
	s.Log("bootstrapping XFS data store at %s...", s.Cfg.DataRoot)
	if err := store.Bootstrap(ctx, s.Cfg.DataImage, s.Cfg.DataRoot, s.Cfg.DataDiskSize, s.Log); err != nil {
		return err
	}
	if err := s.Store.EnsureSlots(); err != nil {
		return err
	}
	if err := s.ensureIncusOrdering(); err != nil {
		return err
	}
	s.Log("waiting for the Incus daemon...")
	if err := s.Incus.WaitReady(ctx, 90*time.Second); err != nil {
		return err
	}
	s.Log("configuring Incus storage, network and profile...")
	return s.Incus.EnsureTopology(ctx, s.Cfg)
}

// ensureIncusOrdering is boot-ordering hardening level 2: incus.service must not
// start before the XFS loop mount exists, or a machine reboot could bring incusd
// (and the backends whose disk sources live on that mount) up too early. Written
// as a drop-in so systemd orders incusd after var-lib-pg\x2ddev\x2dlocal.mount.
func (s *Service) ensureIncusOrdering() error {
	dir := "/etc/systemd/system/incus.service.d"
	path := dir + "/10-after-pgstore.conf"
	content := "[Unit]\nRequiresMountsFor=" + s.Cfg.DataRoot + "\n"
	if b, err := os.ReadFile(path); err == nil && string(b) == content {
		return nil // already in place; skip the daemon-reload
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return err
	}
	s.Log("installed incus.service ordering drop-in (After %s mount)", s.Cfg.DataRoot)
	// Best-effort: a reload lets it take effect without waiting for the next boot.
	_ = reloadSystemd()
	return nil
}

// Up provisions the full topology from an empty Incus host: golden image, both
// backends, the proxy, then activate slot a and reconcile the forwards. It ports
// cmd_up. Refuses if any container already exists (run Down first).
func (s *Service) Up(ctx context.Context) (agentapi.StatusResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.Store.RequireMounted(); err != nil {
		return agentapi.StatusResponse{}, err
	}
	for _, c := range []string{s.Cfg.Container("a"), s.Cfg.Container("b"), s.Cfg.ProxyName} {
		if s.Incus.Exists(ctx, c) {
			return agentapi.StatusResponse{}, fmt.Errorf("container %q already exists — run down first", c)
		}
	}
	aIP, bIP, err := s.resolveBackendIPs(ctx)
	if err != nil {
		return agentapi.StatusResponse{}, err
	}
	s.Log("static backend IPs: %s=%s  %s=%s", s.Cfg.Container("a"), aIP, s.Cfg.Container("b"), bIP)

	if err := s.ensureGolden(ctx); err != nil {
		return agentapi.StatusResponse{}, fmt.Errorf("golden image: %w", err)
	}
	if err := s.provisionBackend(ctx, "a", aIP); err != nil {
		return agentapi.StatusResponse{}, fmt.Errorf("provision %s: %w", s.Cfg.Container("a"), err)
	}
	if err := s.provisionBackend(ctx, "b", bIP); err != nil {
		return agentapi.StatusResponse{}, fmt.Errorf("provision %s: %w", s.Cfg.Container("b"), err)
	}
	if err := s.provisionProxy(ctx); err != nil {
		return agentapi.StatusResponse{}, fmt.Errorf("provision %s: %w", s.Cfg.ProxyName, err)
	}
	if err := s.Active.Set("a"); err != nil {
		return agentapi.StatusResponse{}, err
	}
	if _, err := s.reconcile(ctx, "a"); err != nil {
		return agentapi.StatusResponse{}, err
	}
	s.Log("pg-dev ready.")
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

// provisionBackend launches a backend from the golden image, attaches its XFS
// slot, pins its static IP, creates the slot's cluster + role/db, and snapshots
// `initial`. Ports _provision_backend, minus the apt heredoc (now in the image).
func (s *Service) provisionBackend(ctx context.Context, slot, ip string) error {
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
	s.Log("pinning %s eth0 -> %s...", c, ip)
	if err := s.Incus.SetEth0Static(ctx, c, ip); err != nil {
		return err
	}
	if err := s.Incus.Restart(ctx, c); err != nil {
		return err
	}
	if err := s.Incus.WaitIPv4(ctx, c, ip, time.Minute); err != nil {
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

// provisionProxy launches the bare proxy container (no software; it only owns the
// two proxy devices, added by reconcile). Ports _provision_proxy.
func (s *Service) provisionProxy(ctx context.Context) error {
	if err := s.Incus.Launch(ctx, s.Cfg.ProxyName, s.Cfg.BaseImage); err != nil {
		return err
	}
	return s.Incus.WaitIPv4(ctx, s.Cfg.ProxyName, "", time.Minute)
}

// Down deletes the proxy and both backends, then removes both XFS data trees and
// the active-slot pointer. Ports cmd_down's best-effort teardown with the same
// guard: data is only removed once every container is gone and the store is
// mounted from the expected image.
func (s *Service) Down(ctx context.Context) (agentapi.OpResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, c := range []string{s.Cfg.ProxyName, s.Cfg.Container("a"), s.Cfg.Container("b")} {
		if err := s.Incus.Delete(ctx, c); err != nil {
			return agentapi.OpResponse{}, fmt.Errorf("delete %s: %w (refusing to remove data)", c, err)
		}
	}
	if s.Store.RequireMounted() == nil {
		for _, slot := range []string{"a", "b"} {
			if err := s.Store.RemoveSlotData(slot); err != nil {
				return agentapi.OpResponse{}, err
			}
		}
	} else {
		s.Log("WARNING: %s is not mounted from the XFS store; left the data trees alone", s.Cfg.DataRoot)
	}
	_ = os.Remove(s.Cfg.ActiveSlotPath())
	return agentapi.OpResponse{Message: "containers deleted and data trees removed"}, nil
}
