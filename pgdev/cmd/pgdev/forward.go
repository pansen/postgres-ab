package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"pansen.me/pgdev/internal/forward"
)

// forwardCmd groups the host-side client forwarder (internal/forward), the Go
// replacement for scripts/host-endpoint (spec 0003). `serve` is the long-running
// relay a LaunchAgent runs; install/uninstall/status manage that agent. Promote
// no longer touches any of this — the running serve re-points itself from the
// pointer file.
func (a *app) forwardCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "forward",
		Short: "Host client forwarder: stable 127.0.0.1:5442/:5443 → the active/staging machine",
	}
	c.AddCommand(
		a.forwardServeCmd(),
		a.forwardInstallCmd(),
		a.forwardRestartCmd(),
		a.forwardUninstallCmd(),
		a.forwardStatusCmd(),
	)
	return c
}

// forwardOptions builds the Server config from the resolved cfg.
func (a *app) forwardOptions() forward.Options {
	return forward.Options{
		Bind:              a.cfg.ForwardBind,
		ActivePort:        a.cfg.ClientActivePort,
		StagingPort:       a.cfg.ClientStagingPort,
		BackendPort:       a.cfg.BackendPort,
		ActiveMachinePath: a.cfg.ActiveMachinePath(),
		MachineIPPath:     a.cfg.MachineIPPath,
		StatePath:         a.cfg.ForwardStatePath(),
		Log:               logf,
	}
}

// launchd builds the LaunchAgent handle for the current binary.
func (a *app) launchd() (*forward.Launchd, error) {
	exe, err := forwardExecutable()
	if err != nil {
		return nil, err
	}
	program := []string{exe, "forward", "serve"}
	ld := forward.NewLaunchd(a.cfg.MachinePrefix, program, a.cfg.ForwardLogPath(), a.cfg.RepoRoot)
	// Migration: on install, kill any orphaned socat still holding the client
	// ports (done inside Install, AFTER bootout, so the retired KeepAlive agent
	// can't respawn them). See Launchd.Install.
	ld.ReapPorts = []int{a.cfg.ClientActivePort, a.cfg.ClientStagingPort}
	return ld, nil
}

func (a *app) forwardServeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the forwarder in the foreground (what the LaunchAgent runs)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts := a.forwardOptions()
			if !isLoopback(opts.Bind) {
				fmt.Fprintf(os.Stderr,
					"WARNING: binding %s exposes the dev-credentialed PostgreSQL backend on every interface (LAN/Wi-Fi).\n",
					opts.Bind)
			}
			return forward.New(opts).Serve(cmd.Context())
		},
	}
}

func (a *app) forwardInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Install (and start) the per-user LaunchAgent that keeps the forwarder alive",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ld, err := a.launchd()
			if err != nil {
				return err
			}
			if err := ld.Install(cmd.Context()); err != nil {
				return err
			}
			fmt.Printf("==> Forwarder '%s' installed and started.\n", ld.Label)
			fmt.Printf("    active  127.0.0.1:%d  staging  127.0.0.1:%d  (re-points itself on promote)\n",
				a.cfg.ClientActivePort, a.cfg.ClientStagingPort)
			return nil
		},
	}
}

// forwardRestartCmd restarts the running agent so a permission granted after it
// started (macOS Local Network Privacy caches the decision at process start) is
// re-evaluated — without this, ticking the Local Network box has no effect until
// the next reboot/reinstall and the forwarder keeps failing with EHOSTUNREACH.
func (a *app) forwardRestartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Restart the running forwarder (apply a freshly-granted macOS Local Network permission)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ld, err := a.launchd()
			if err != nil {
				return err
			}
			if err := ld.Restart(cmd.Context()); err != nil {
				return err
			}
			fmt.Printf("==> Forwarder '%s' restarted.\n", ld.Label)
			return nil
		},
	}
}

func (a *app) forwardUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the forwarder LaunchAgent",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ld, err := a.launchd()
			if err != nil {
				return err
			}
			if err := ld.Uninstall(cmd.Context()); err != nil {
				return err
			}
			fmt.Printf("==> Forwarder '%s' removed.\n", ld.Label)
			return nil
		},
	}
}

func (a *app) forwardStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Forwarder agent state, cached IPs, and the live effective mapping",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ld, err := a.launchd()
			if err != nil {
				return err
			}
			fmt.Printf("plist:          %s\n", ld.Plist)
			fmt.Printf("installed:      %v\n", ld.Installed())
			fmt.Printf("active machine: %s\n", a.cfg.MachineNameForSlot(a.active.Get()))
			fmt.Printf("cached IP a:    %s\n", orNone(readFileTrim(a.cfg.MachineIPPath("a"))))
			fmt.Printf("cached IP b:    %s\n", orNone(readFileTrim(a.cfg.MachineIPPath("b"))))
			a.renderForwarder(cmd.Context())
			return nil
		},
	}
}

// renderForwarder prints the two live forwarder lines — the LaunchAgent's
// launchd state and the effective mapping/heartbeat from the state file. Shared
// by `forward status` and the main `status` command so both agree.
func (a *app) renderForwarder(ctx context.Context) {
	ld, err := a.launchd()
	if err != nil {
		fmt.Printf("forwarder:      %v\n", err)
		return
	}
	statusLine := ld.Status(ctx)
	st, ok := forward.ReadState(a.cfg.ForwardStatePath())
	if !ok {
		fmt.Printf("%s\n", statusLine)
		fmt.Printf("(no state file — forwarder has not run)\n")
		return
	}
	live := "STALE"
	if st.Fresh(10 * time.Second) {
		live = "live"
	}
	age := time.Since(time.Unix(st.UpdatedUnix, 0)).Round(time.Second)
	fmt.Printf("%s (updated %s ago, %s)\n", statusLine, age, live)
	fmt.Printf("active -> %s\n", orNone(st.Active))
	fmt.Printf("staging -> %s\n", orNone(st.Staging))
}

// ----- helpers ---------------------------------------------------------------

// logf is a logx.Func that streams the forwarder's progress lines to stderr.
func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "==> "+format+"\n", args...)
}

// forwardExecutable resolves the absolute path to bake into the plist, refusing
// a `go run` temp binary (which vanishes on exit, crash-looping the agent).
func forwardExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable for LaunchAgent: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(exe, os.TempDir()) {
		return "", fmt.Errorf("refusing to install a LaunchAgent for a temporary binary (%s) — build pgdev first ('make -C pgdev build') and install from bin/pgdev", exe)
	}
	return exe, nil
}

func isLoopback(bind string) bool {
	return bind == "" || bind == "127.0.0.1" || bind == "::1" || bind == "localhost"
}

func readFileTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
