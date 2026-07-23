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

// Bootstrap ensures the machine is ready to host its one backend: the XFS reflink
// store mounted, this slot's layout present, the Incus daemon topology configured,
// and the boot-ordering drop-in for incusd installed. Runs as the systemd unit's
// ExecStartPre (`pgdevd bootstrap`) so every daemon start re-asserts it.
func (s *Service) Bootstrap(ctx context.Context) error {
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
	s.Log("waiting for the Incus daemon...")
	if err := s.Incus.WaitReady(ctx, 90*time.Second); err != nil {
		return err
	}
	s.Log("configuring Incus storage, network and profile...")
	return s.Incus.EnsureTopology(ctx, s.Cfg)
}

// ensureIncusOrdering is boot-ordering hardening level 2: incus.service must not
// start before the XFS loop mount exists, or a machine reboot could bring incusd
// (and the backend whose disk source lives on that mount) up too early. Written
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
