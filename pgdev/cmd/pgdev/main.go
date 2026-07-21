// Command pgdev is the host (macOS) CLI. Since Slice 2 the stateful control path
// no longer shells scripts into the machine over Apple's broken `container exec`:
// pgdev talks HTTP/JSON to the resident pgdevd daemon (internal/agentapi) at the
// machine's eth0. The only `container` execs left here are IP discovery
// (internal/applecli, a fallback) and `agent deploy`'s one-shot install/restart.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"pansen.me/pgdev/internal/activeslot"
	"pansen.me/pgdev/internal/agentapi"
	"pansen.me/pgdev/internal/applecli"
	"pansen.me/pgdev/internal/config"
)

// version is stamped at build time (see Makefile), matched against the daemon's
// /v1/version during `agent deploy`.
var version = "dev"

type app struct {
	cfg    config.Config
	apple  *applecli.CLI
	active activeslot.Pointer
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := rootCmd().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR: "+err.Error())
		os.Exit(1)
	}
}

func newApp() *app {
	cfg := config.Load()
	return &app{
		cfg:    cfg,
		apple:  applecli.New(cfg.MachineName),
		active: activeslot.Pointer{Path: cfg.ActiveSlotPath(), UID: cfg.HostUID, GID: cfg.HostGID},
	}
}

func rootCmd() *cobra.Command {
	a := newApp()
	root := &cobra.Command{
		Use:           "pgdev",
		Short:         "Host CLI for the snapshottable-PostgreSQL Apple machine",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
	}

	root.AddCommand(
		a.statusCmd(),
		a.promoteCmd(),
		a.refreshCmd(),
		a.snapshotsCmd(),
		a.ipCmd(),
		a.endpointCmd(),
		a.snapshotCmd("active"),
		a.restoreCmd("active"),
		a.restoreLastCmd("active"),
		a.stagingCmd(),
		a.agentCmd(),
	)
	return root
}

// ----- daemon client wiring ------------------------------------------------

// machineIP resolves the machine's eth0 address: an explicit override, then the
// daemon-pushed var/machine-ip file, then a live `container` exec fallback
// (§5.9). Empty when the machine is down.
func (a *app) machineIP(ctx context.Context) string {
	if a.cfg.MachineIP != "" {
		return a.cfg.MachineIP
	}
	if b, err := os.ReadFile(a.machineIPFile()); err == nil {
		if ip := strings.TrimSpace(string(b)); ip != "" {
			return ip
		}
	}
	ip, _ := a.apple.MachineIP(ctx)
	return ip
}

func (a *app) machineIPFile() string { return filepath.Join(a.cfg.RepoRoot, "var", "machine-ip") }

// client builds a typed daemon client against the current machine IP. It ensures
// the shared bearer token exists (generating it on first use over the home-mount).
func (a *app) client(ctx context.Context) (*agentapi.Client, error) {
	ip := a.machineIP(ctx)
	if ip == "" {
		return nil, fmt.Errorf("cannot reach the machine: no IP (is '%s' running? run 'make start')", a.cfg.MachineName)
	}
	token, err := agentapi.EnsureToken(a.cfg.AgentTokenPath)
	if err != nil {
		return nil, fmt.Errorf("agent token: %w", err)
	}
	base := fmt.Sprintf("http://%s:%d", ip, a.cfg.AgentPort)
	return agentapi.NewClient(base, token), nil
}

// slot resolves a role ("active"/"staging") to a concrete slot via the pointer.
func (a *app) slot(role string) string {
	if role == "staging" {
		return a.active.Staging()
	}
	return a.active.Get()
}
