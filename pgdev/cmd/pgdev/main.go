// Command pgdev is the host (macOS) CLI. Since Slice 2 the stateful control path
// no longer shells scripts into the machine over Apple's broken `container exec`:
// pgdev talks HTTP/JSON to the resident pgdevd daemon (internal/agentapi) at each
// machine's eth0. Since spec 0002 (two machines) there is one daemon PER machine
// (vpg-a/vpg-b, one backend each) and active/staging is a HOST-side concept: a
// pointer file (internal/activeslot, now pointed at the ACTIVE MACHINE) picks
// which machine's client is "active" vs "staging", and the in-process host
// forwarder (internal/forward, run as `pgdev forward serve` under a LaunchAgent)
// maps the stable 127.0.0.1:5442/:5443 client ports onto whichever machine
// currently holds each role, re-pointing itself when the pointer flips. The only
// `container` execs left
// here are IP discovery (internal/applecli, a fallback) and `agent deploy`'s
// one-shot install/restart, plus the hard-reset machine lifecycle used by
// `staging rebuild`.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"pansen.me/pgdev/internal/activeslot"
	"pansen.me/pgdev/internal/agentapi"
	"pansen.me/pgdev/internal/applecli"
	"pansen.me/pgdev/internal/config"
	"pansen.me/pgdev/internal/forward"
)

// version is stamped at build time (see Makefile), matched against each
// daemon's /v1/version during `agent deploy`.
var version = "dev"

// slotsAB is the fixed pair of slots the two-machine model always operates
// over, in a stable order for fan-out loops and rendering.
var slotsAB = []string{"a", "b"}

type app struct {
	cfg config.Config
	// active is the host-side pointer to which MACHINE is active (behind
	// :5442); the other machine is staging (behind :5443). See spec 0002 §0.1.
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
		active: activeslot.Pointer{Path: cfg.ActiveMachinePath(), UID: cfg.HostUID, GID: cfg.HostGID},
	}
}

func rootCmd() *cobra.Command {
	a := newApp()
	root := &cobra.Command{
		Use:           "pgdev",
		Short:         "Host CLI for the two-machine snapshottable-PostgreSQL setup",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
	}

	root.AddCommand(
		a.upCmd(),
		a.downCmd(),
		a.statusCmd(),
		a.promoteCmd(),
		a.refreshCmd(),
		a.snapshotsCmd(),
		a.ipCmd(),
		a.endpointCmd(),
		a.forwardCmd(),
		a.snapshotCmd("active"),
		a.restoreCmd("active"),
		a.restoreLastCmd("active"),
		a.stagingCmd(),
		a.agentCmd(),
	)
	return root
}

// ----- machine + daemon client wiring ---------------------------------------

// apple returns an Apple CLI handle for slot's machine (vpg-a / vpg-b). CLI
// values are stateless besides the machine name — the real `container` exec
// path is serialized globally inside applecli — so a fresh one per call is
// cheap and avoids the host needing to keep one alive.
func (a *app) apple(slot string) *applecli.CLI {
	return applecli.New(a.cfg.MachineNameForSlot(slot))
}

// machineIP resolves slot's machine eth0 address: the daemon-pushed
// var/machine-ip-<slot> file first, then a live `container` exec fallback
// (§5.9). Empty when the machine is down or has never reported an address.
func (a *app) machineIP(ctx context.Context, slot string) string {
	if b, err := os.ReadFile(a.cfg.MachineIPPath(slot)); err == nil {
		if ip := strings.TrimSpace(string(b)); ip != "" {
			return ip
		}
	}
	ip, _ := a.apple(slot).MachineIP(ctx)
	return ip
}

// writeMachineIPFile caches slot's discovered IP host-side so subsequent
// commands (and the host forwarder) don't need a live `container` exec.
func (a *app) writeMachineIPFile(slot, ip string) {
	if ip == "" {
		return
	}
	path := a.cfg.MachineIPPath(slot)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte(ip+"\n"), 0o644)
}

// clientFor builds a typed daemon client against slot's machine. It ensures
// the shared bearer token exists (generating it on first use over the
// home-mount).
func (a *app) clientFor(ctx context.Context, slot string) (*agentapi.Client, error) {
	ip := a.machineIP(ctx, slot)
	if ip == "" {
		return nil, fmt.Errorf("machine %s unreachable — no IP (run 'make start')", a.cfg.MachineNameForSlot(slot))
	}
	token, err := agentapi.EnsureToken(a.cfg.AgentTokenPath)
	if err != nil {
		return nil, fmt.Errorf("agent token: %w", err)
	}
	base := fmt.Sprintf("http://%s:%d", ip, a.cfg.AgentPort)
	return agentapi.NewClient(base, token), nil
}

// longClientFor is clientFor with a generous timeout for multi-minute
// mutations (up/down provisioning holds the HTTP request open while the
// daemon installs PostgreSQL and creates a cluster).
func (a *app) longClientFor(ctx context.Context, slot string) (*agentapi.Client, error) {
	cl, err := a.clientFor(ctx, slot)
	if err != nil {
		return nil, err
	}
	cl.HTTP.Timeout = 30 * time.Minute
	return cl, nil
}

// roleSlot resolves a role ("active"/"staging") to a concrete slot via the
// active-machine pointer.
func (a *app) roleSlot(role string) string {
	if role == "staging" {
		return a.active.Staging()
	}
	return a.active.Get()
}

func (a *app) clientForRole(ctx context.Context, role string) (*agentapi.Client, error) {
	return a.clientFor(ctx, a.roleSlot(role))
}

func (a *app) longClientForRole(ctx context.Context, role string) (*agentapi.Client, error) {
	return a.longClientFor(ctx, a.roleSlot(role))
}

// ensureForwarder makes the client forwarder hands-off: on `make start` it
// validates the LaunchAgent against the current build and self-heals a missing,
// unloaded, stale, or crashed one, so 127.0.0.1:5442/:5443 come up (and stay
// correct) automatically. This is deliberately stronger than the old bare
// "is a plist on disk?" check: after the repo moves or is renamed, a plist is
// still on disk but points at a deleted binary — it loads, exits EX_CONFIG, and
// leaves the ports dead. Ensure catches that program drift. On the healthy
// common path it is a single `launchctl print` + compare, no launchd writes, so
// the resident serve keeps re-pointing itself from the pointer file untouched
// (no kickstart on promote/refresh; spec 0003 §3). It NEVER fails the caller:
// PG_ENDPOINT_AUTOINSTALL=0 opts out, and a sandbox that can't write
// ~/Library/LaunchAgents just warns.
func (a *app) ensureForwarder(ctx context.Context) {
	if os.Getenv("PG_ENDPOINT_AUTOINSTALL") == "0" {
		return
	}
	ld, err := a.launchd()
	if err != nil {
		// e.g. running via `go run` (temp binary) — can't self-install; leave the
		// forwarder to an explicit `pgdev forward install` from a built binary.
		fmt.Fprintf(os.Stderr, "WARNING: skipping forwarder auto-install: %v\n", err)
		return
	}
	res, err := ld.Ensure(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: forwarder auto-install failed (run 'pgdev forward install'): %v\n", err)
		return
	}
	switch res.Action {
	case "installed":
		fmt.Fprintf(os.Stderr, "==> Endpoint forwarder '%s' installed (%s).\n", ld.Label, res.Reason)
	case "reinstalled":
		fmt.Fprintf(os.Stderr, "==> Endpoint forwarder '%s' re-synced: %s.\n", ld.Label, res.Reason)
	}
}

// awaitForwarderRepoint gives the resident forwarder up to a couple of poll
// intervals to re-point onto the newly-active slot after a promote, then reports
// whether it confirmably did. It reads internal/forward's state file rather than
// touching launchd, so promote stays a pure pointer write. A false return is a
// warning, not a hard failure: the pointer IS the source of truth and the
// forwarder converges once it is running — but we surface an unconfirmed
// re-point so a following `pg_restore -p 5443` isn't silently misrouted.
func (a *app) awaitForwarderRepoint(ctx context.Context, slot string) bool {
	deadline := time.Now().Add(4 * time.Second)
	for {
		if st, ok := forward.ReadState(a.cfg.ForwardStatePath()); ok && st.ActiveSlot == slot && st.Fresh(10*time.Second) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(250 * time.Millisecond):
		}
	}
}
