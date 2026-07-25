// Command pgdev is the host (macOS) CLI. Since Slice 2 the stateful control path
// no longer shells scripts into the machine over Apple's broken `container exec`:
// pgdev talks HTTP/JSON to the resident pgdevd daemon (internal/agentapi) at each
// machine's eth0. Since spec 0002 (two machines) there is one daemon PER machine
// (vpg-a/vpg-b, one backend each) and active/staging is a HOST-side concept: a
// pointer (internal/activeslot, now pointed at the ACTIVE MACHINE) picks which
// machine's client is "active" vs "staging", and the socat client proxy
// (internal/socatproxy, per-user LaunchAgents driven by internal/track) maps the
// stable 127.0.0.1:5442/:5443 client ports onto whichever machine currently holds
// each role, re-pointed by a reconcile when the pointer flips. The only
// `container` execs left
// here are IP discovery (internal/applecli, a fallback) and `agent deploy`'s
// one-shot install/restart, plus the hard-reset machine lifecycle used by
// `staging rebuild`.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"pansen.me/pgdev/internal/activeslot"
	"pansen.me/pgdev/internal/agentapi"
	"pansen.me/pgdev/internal/applecli"
	"pansen.me/pgdev/internal/config"
	"pansen.me/pgdev/internal/track"
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
	// log is the structured (slog) logger threaded into the tracking DB and the
	// socat proxy. Text handler to stderr; debug when cfg.ProxyVerbose.
	log *slog.Logger
	// trackDB is the SQLite tracking store (internal/track), opened lazily and
	// cached for the process. nil until first use; see app.track.
	trackDB *track.DB
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
		log:    newLogger(cfg.ProxyVerbose),
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
		a.proxyCmd(),
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
// commands don't need a live `container` exec. The cache lives in the SQLite
// tracking DB (the source of truth); track mirrors it back to the flat
// var/machine-ip-<slot> file this CLI still reads in machineIP (see
// doc/issues/0004 §5.2 — retiring the mirrors is the remaining step).
// There is deliberately NO file-only fallback when the DB is unavailable:
// a file-only write would diverge the mirror from the (stale) DB, and socat —
// the canonical client path — routes on the DB, so it would silently forward to
// the wrong machine. A DB that won't open is a real fault to surface, not paper
// over; IP caching is best-effort, so we log loudly and skip (next run retries).
func (a *app) writeMachineIPFile(ctx context.Context, slot, ip string) {
	if ip == "" {
		return
	}
	db, err := a.track(ctx)
	if err != nil {
		a.log.Error("cannot cache machine IP: tracking DB unavailable", "slot", slot, "ip", ip, "err", err)
		return
	}
	if err := db.SetMachineIP(ctx, slot, ip); err != nil {
		a.log.Warn("caching machine IP failed", "slot", slot, "err", err)
	}
}

// setActive is the CHOKEPOINT for the active pointer: it writes the SQLite
// tracking DB (source of truth), which mirrors the value back to the flat
// var/active-machine file that this CLI (activeslot) and the Makefile's
// ACTIVE_SLOT still read. If the DB can't be opened this is a HARD error —
// unlike IP caching, a promote must not half-succeed: writing only the flat file
// would leave the DB (and therefore socat, the client path) pointing at the OLD
// active machine, so a client would keep hitting the pre-promote database. Fail
// loudly so the operator knows the flip did not take, rather than silently
// splitting the two paths.
func (a *app) setActive(ctx context.Context, slot string) error {
	db, err := a.track(ctx)
	if err != nil {
		return fmt.Errorf("cannot set active slot %q: tracking DB unavailable (%w)", slot, err)
	}
	return db.SetActive(ctx, slot)
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
