package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"pansen.me/pgdev/internal/agentapi"
)

// localEnvPath is where the daemon's EnvironmentFile lives INSIDE the machine.
// It is a machine-local copy (delivered by a fresh `cp` exec, which reads the
// home-mount reliably) rather than the home-mount path itself: the long-running
// daemon/systemd must not read its config over the mount, whose virtiofs cache
// can serve a stale empty read right after boot (spec 0002 deploy note — the
// agent token in particular was read back as 0 bytes, 401-ing every request).
const localEnvPath = "/usr/local/etc/pgdevd.env"

// unitTemplate is the systemd unit for the resident daemon. It runs the
// machine-local binary (not the home-mount copy) so it survives a boot where the
// mount isn't up yet; the home-mount is the delivery channel, not the run path
// (§5.11). EnvironmentFile is the machine-local copy (localEnvPath), optional
// (`-`) so a bare boot still starts.
const unitTemplate = `[Unit]
Description=pgdev in-machine control daemon
After=incus.service network-online.target
Wants=incus.service

[Service]
Type=simple
EnvironmentFile=-` + localEnvPath + `
# Non-fatal (the '-'): bootstrap still creates/mounts the store and topology on
# success, but if it can't (e.g. a full disk shut the XFS store down), the daemon
# still starts so 'pgdev status' stays usable to diagnose, instead of the whole
# unit failing and bricking 'make start'.
ExecStartPre=-/usr/local/bin/pgdevd bootstrap
ExecStart=/usr/local/bin/pgdevd serve
Restart=on-failure
RestartSec=2
TimeoutStartSec=300

[Install]
WantedBy=multi-user.target
`

func (a *app) agentCmd() *cobra.Command {
	c := &cobra.Command{Use: "agent", Short: "Manage the in-machine pgdevd daemons"}
	c.AddCommand(a.agentDeployCmd(), a.agentVersionCmd())
	return c
}

func (a *app) agentVersionCmd() *cobra.Command {
	var machine string
	c := &cobra.Command{
		Use:   "version",
		Short: "Print one or both machines' running daemon version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			slots, err := slotsFor(machine)
			if err != nil {
				return err
			}
			for _, slot := range slots {
				name := a.cfg.MachineNameForSlot(slot)
				cl, err := a.clientFor(ctx, slot)
				if err != nil {
					fmt.Printf("[%s] %v\n", name, err)
					continue
				}
				v, err := cl.Version(ctx)
				if err != nil {
					fmt.Printf("[%s] %v\n", name, err)
					continue
				}
				fmt.Printf("[%s] pgdevd %s (api v%d)\n", name, v.Version, v.APIVersion)
			}
			return nil
		},
	}
	c.Flags().StringVar(&machine, "machine", "both", "which machine to query: a|b|both")
	return c
}

// agentDeployCmd hot-deploys the cross-compiled daemon into one or both RUNNING
// machines without an image rebuild (§5.11): deliver via the home-mount,
// atomically install to a machine-local path, (re)install the unit, restart,
// then confirm the version handshake so a stale daemon fails loudly.
func (a *app) agentDeployCmd() *cobra.Command {
	var machine string
	c := &cobra.Command{
		Use:   "deploy",
		Short: "Install/restart pgdevd in one or both machines and confirm the version handshake",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			slots, err := slotsFor(machine)
			if err != nil {
				return err
			}
			for _, slot := range slots {
				if err := a.deploy(ctx, slot); err != nil {
					return err
				}
			}
			return nil
		},
	}
	c.Flags().StringVar(&machine, "machine", "both", "which machine to deploy to: a|b|both")
	return c
}

// slotsFor resolves the --machine flag ("a"/"b"/"both") to the slots to act on.
func slotsFor(machine string) ([]string, error) {
	switch machine {
	case "both", "":
		return slotsAB, nil
	case "a", "b":
		return []string{machine}, nil
	default:
		return nil, fmt.Errorf("--machine must be a, b, or both (got %q)", machine)
	}
}

// deploy installs/restarts pgdevd on slot's machine. Each machine gets its own
// env/unit files (var/pgdevd-<slot>.env, var/pgdevd-<slot>.service) so the two
// deploys never collide on disk.
func (a *app) deploy(ctx context.Context, slot string) error {
	machine := a.cfg.MachineNameForSlot(slot)
	cli := a.apple(slot)
	if !cli.Exists(ctx) {
		return fmt.Errorf("machine '%s' does not exist — run 'make start' first", machine)
	}
	bin := filepath.Join(a.cfg.RepoRoot, "pgdev", "bin", "pgdevd")
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("built daemon not found at %s — run 'make pgdevd' first", bin)
	}
	token, err := agentapi.EnsureToken(a.cfg.AgentTokenPath)
	if err != nil {
		return fmt.Errorf("agent token: %w", err)
	}
	// Stage the env file and the unit on the home-mount (visible in-machine at
	// the same path). Apple's `container machine run` word-splits a `sh -c`
	// string, so every in-machine step below is a plain multi-token argv with no
	// shell, no redirect, no stdin — files are delivered via the mount and copied
	// to a machine-LOCAL path by a fresh exec (which reads the mount reliably).
	envPath := filepath.Join(a.cfg.RepoRoot, "var", fmt.Sprintf("pgdevd-%s.env", slot))
	if err := os.WriteFile(envPath, []byte(a.daemonEnv(slot, token)), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", envPath, err)
	}
	unitPath := filepath.Join(a.cfg.RepoRoot, "var", fmt.Sprintf("pgdevd-%s.service", slot))
	if err := os.WriteFile(unitPath, []byte(unitTemplate), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", unitPath, err)
	}

	// A freshly created machine may still be finishing its first boot; wait until
	// systemd is up before installing/enabling, or Apple rejects the exec
	// ("Operation not supported by device") or systemctl can't reach the bus.
	fmt.Printf("==> [%s] Waiting for the machine to be ready...\n", machine)
	if err := cli.WaitReady(ctx, 120*time.Second); err != nil {
		return err
	}
	// A (re)created machine gets a fresh DHCP lease, so the cached
	// var/machine-ip-<slot> can be stale. Refresh it from the live address
	// before the handshake dials the daemon (this also re-points the endpoint
	// forwarder at the new IP once refreshForwarder next runs).
	if ip, err := cli.MachineIP(ctx); err == nil && ip != "" {
		a.writeMachineIPFile(slot, ip)
	}

	fmt.Printf("==> [%s] Installing pgdevd into the machine...\n", machine)
	steps := [][]string{
		// Atomic install to the machine-local run path: stage then rename, never
		// write-in-place → no ETXTBSY on the live binary.
		{"install", "-m", "0755", bin, "/usr/local/bin/pgdevd.new"},
		{"mv", "-f", "/usr/local/bin/pgdevd.new", "/usr/local/bin/pgdevd"},
		// Copy the env file to a machine-LOCAL path (this fresh `cp` reads the
		// home-mount reliably; the daemon then never touches the mount for its
		// config/token). install sets the mode in one step.
		{"install", "-m", "0600", "-D", envPath, localEnvPath},
		// (Re)install the systemd unit from the staged copy.
		{"cp", unitPath, "/etc/systemd/system/pgdevd.service"},
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", "pgdevd"},
	}
	for _, s := range steps {
		if out, err := cli.Run(ctx, s...); err != nil {
			return fmt.Errorf("%s: %w\n%s", strings.Join(s, " "), err, out)
		}
	}

	fmt.Printf("==> [%s] Restarting pgdevd...\n", machine)
	if out, err := cli.Run(ctx, "systemctl", "restart", "pgdevd"); err != nil {
		return fmt.Errorf("restarting daemon: %w\n%s", err, out)
	}

	// Confirm the running daemon is the one we just shipped.
	return a.awaitVersion(ctx, slot)
}

// awaitVersion polls slot's /v1/version until the daemon reports our build stamp
// (or just becomes reachable when this binary is an unstamped `dev` build),
// failing loudly on timeout so deploy never silently leaves old code running.
func (a *app) awaitVersion(ctx context.Context, slot string) error {
	machine := a.cfg.MachineNameForSlot(slot)
	cl, err := a.clientFor(ctx, slot)
	if err != nil {
		return err
	}
	// 90s (not 30): on a freshly created machine the daemon's ExecStartPre
	// bootstrap (XFS store + Incus topology, up to ~90s) delays serve, and the
	// home-mount token can take a beat to warm before auth succeeds.
	deadline := time.Now().Add(90 * time.Second)
	var last string
	for {
		v, err := cl.Version(ctx)
		if err == nil {
			last = v.Version
			if version == "dev" || v.Version == version {
				fmt.Printf("==> [%s] Deployed. pgdevd %s (api v%d) is live.\n", machine, v.Version, v.APIVersion)
				return nil
			}
		}
		if time.Now().After(deadline) {
			if last != "" {
				return fmt.Errorf("%s: daemon reports %q but expected %q after restart (stale binary?)", machine, last, version)
			}
			return fmt.Errorf("%s: daemon did not answer /v1/version within 90s: %v", machine, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// daemonEnv materializes the config the daemon needs, delivered as its
// machine-local systemd EnvironmentFile. Two-machine model (spec 0002): no
// proxy, no client ports, no pinned backend IPs — each daemon serves exactly one
// slot on its own machine's eth0. Everything the daemon needs at runtime is
// baked in here (token value, PG credentials, disk size) so it never has to read
// the home-mount .env or token file, whose virtiofs cache can serve a stale
// empty read right after boot. Values in the process env win over any .env, so a
// cold mount read is harmless.
func (a *app) daemonEnv(slot, token string) string {
	var b strings.Builder
	put := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%s=%s\n", k, v)
		}
	}
	put("PG_REPO_ROOT", a.cfg.RepoRoot)
	put("PG_DATA_ROOT", a.cfg.DataRoot)
	put("PG_DATA_DISK_SIZE", a.cfg.DataDiskSize)
	put("PG_BACKEND_PREFIX", a.cfg.BackendPrefix)
	put("PG_SLOT", slot)
	put("PG_BACKEND_PORT", itoa(a.cfg.BackendPort))
	put("PG_AGENT_PORT", itoa(a.cfg.AgentPort))
	put("PG_AGENT_TOKEN", token)
	// PostgreSQL credentials the daemon uses to create the role/db during Up.
	put("PG_USER", a.cfg.PGUser)
	put("PG_DB", a.cfg.PGDB)
	put("PG_PASSWORD", a.cfg.PGPassword)
	put("HOST_UID", a.cfg.HostUID)
	put("HOST_GID", a.cfg.HostGID)
	return b.String()
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
