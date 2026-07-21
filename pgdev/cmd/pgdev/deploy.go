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

// unitTemplate is the systemd unit for the resident daemon. It runs the
// machine-local binary (not the home-mount copy) so it survives a boot where the
// mount isn't up yet; the home-mount is the delivery channel, not the run path
// (§5.11). EnvironmentFile is optional (`-`) so a bare boot still starts.
const unitTemplate = `[Unit]
Description=pgdev in-machine control daemon
After=incus.service network-online.target
Wants=incus.service

[Service]
Type=simple
EnvironmentFile=-%s
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
	c := &cobra.Command{Use: "agent", Short: "Manage the in-machine pgdevd daemon"}
	c.AddCommand(a.agentDeployCmd(), a.agentVersionCmd())
	return c
}

func (a *app) agentVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the running daemon's version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cl, err := a.client(ctx)
			if err != nil {
				return err
			}
			v, err := cl.Version(ctx)
			if err != nil {
				return err
			}
			fmt.Printf("pgdevd %s (api v%d)\n", v.Version, v.APIVersion)
			return nil
		},
	}
}

// agentDeployCmd hot-deploys the cross-compiled daemon into the RUNNING machine
// without an image rebuild (§5.11): deliver via the home-mount, atomically
// install to a machine-local path, (re)install the unit, restart, then confirm
// the version handshake so a stale daemon fails loudly.
func (a *app) agentDeployCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "deploy",
		Short: "Install/restart pgdevd in the machine and confirm the version handshake",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			return a.deploy(ctx)
		},
	}
}

func (a *app) deploy(ctx context.Context) error {
	if !a.apple.Exists(ctx) {
		return fmt.Errorf("machine '%s' does not exist — run 'make start' first", a.cfg.MachineName)
	}
	bin := filepath.Join(a.cfg.RepoRoot, "pgdev", "bin", "pgdevd")
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("built daemon not found at %s — run 'make pgdevd' first", bin)
	}
	if _, err := agentapi.EnsureToken(a.cfg.AgentTokenPath); err != nil {
		return fmt.Errorf("agent token: %w", err)
	}
	// Stage the env file and the unit on the home-mount (visible in-machine at
	// the same path). Apple's `container machine run` word-splits a `sh -c`
	// string, so every in-machine step below is a plain multi-token argv with no
	// shell, no redirect, no stdin — files are delivered via the mount and copied.
	envPath := filepath.Join(a.cfg.RepoRoot, "var", "pgdevd.env")
	if err := os.WriteFile(envPath, []byte(a.daemonEnv()), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", envPath, err)
	}
	unitPath := filepath.Join(a.cfg.RepoRoot, "var", "pgdevd.service")
	if err := os.WriteFile(unitPath, []byte(fmt.Sprintf(unitTemplate, envPath)), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", unitPath, err)
	}

	// A freshly created machine may still be finishing its first boot; wait until
	// systemd is up before installing/enabling, or Apple rejects the exec
	// ("Operation not supported by device") or systemctl can't reach the bus.
	fmt.Println("==> Waiting for the machine to be ready...")
	if err := a.apple.WaitReady(ctx, 120*time.Second); err != nil {
		return err
	}
	// A (re)created machine gets a fresh DHCP lease, so the cached var/machine-ip
	// can be stale. Refresh it from the live address before the handshake dials
	// the daemon (this also re-points the endpoint forwarder at the new IP).
	if ip, err := a.apple.MachineIP(ctx); err == nil && ip != "" {
		a.writeMachineIPFile(ip)
	}

	fmt.Println("==> Installing pgdevd into the machine...")
	steps := [][]string{
		// Atomic install to the machine-local run path: stage then rename, never
		// write-in-place → no ETXTBSY on the live binary.
		{"install", "-m", "0755", bin, "/usr/local/bin/pgdevd.new"},
		{"mv", "-f", "/usr/local/bin/pgdevd.new", "/usr/local/bin/pgdevd"},
		// (Re)install the systemd unit from the staged copy.
		{"cp", unitPath, "/etc/systemd/system/pgdevd.service"},
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", "pgdevd"},
	}
	for _, s := range steps {
		if out, err := a.apple.Run(ctx, s...); err != nil {
			return fmt.Errorf("%s: %w\n%s", strings.Join(s, " "), err, out)
		}
	}

	fmt.Println("==> Restarting pgdevd...")
	if out, err := a.apple.Run(ctx, "systemctl", "restart", "pgdevd"); err != nil {
		return fmt.Errorf("restarting daemon: %w\n%s", err, out)
	}

	// Confirm the running daemon is the one we just shipped.
	return a.awaitVersion(ctx)
}

// awaitVersion polls /v1/version until the daemon reports our build stamp (or
// just becomes reachable when this binary is an unstamped `dev` build), failing
// loudly on timeout so deploy never silently leaves old code running.
func (a *app) awaitVersion(ctx context.Context) error {
	cl, err := a.client(ctx)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	var last string
	for {
		v, err := cl.Version(ctx)
		if err == nil {
			last = v.Version
			if version == "dev" || v.Version == version {
				fmt.Printf("==> Deployed. pgdevd %s (api v%d) is live.\n", v.Version, v.APIVersion)
				return nil
			}
		}
		if time.Now().After(deadline) {
			if last != "" {
				return fmt.Errorf("daemon reports %q but expected %q after restart (stale binary?)", last, version)
			}
			return fmt.Errorf("daemon did not answer /v1/version within 30s: %v", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// daemonEnv materializes the subset of config the daemon reads (§3), as a
// systemd EnvironmentFile.
func (a *app) daemonEnv() string {
	var b strings.Builder
	put := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%s=%s\n", k, v)
		}
	}
	put("PG_REPO_ROOT", a.cfg.RepoRoot)
	put("PG_DATA_ROOT", a.cfg.DataRoot)
	put("PG_BACKEND_PREFIX", a.cfg.BackendPrefix)
	put("PG_PROXY_NAME", a.cfg.ProxyName)
	put("PG_ACTIVE_PORT", itoa(a.cfg.ActivePort))
	put("PG_STAGING_PORT", itoa(a.cfg.StagingPort))
	put("PG_AGENT_PORT", itoa(a.cfg.AgentPort))
	put("PG_AGENT_TOKEN_PATH", a.cfg.AgentTokenPath)
	put("PG_BACKEND_A_IP", a.cfg.BackendAIP)
	put("PG_BACKEND_B_IP", a.cfg.BackendBIP)
	put("HOST_UID", a.cfg.HostUID)
	put("HOST_GID", a.cfg.HostGID)
	return b.String()
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
