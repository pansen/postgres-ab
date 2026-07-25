package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"pansen.me/pgdev/internal/socatproxy"
	"pansen.me/pgdev/internal/track"
)

// newLogger builds the process's structured logger. Text handler to stderr so
// the lines interleave readably with the existing "==> " progress output;
// verbose flips the level to Debug, which the tracking + socat paths use to be
// deliberately chatty (the experiment wants a fat forensic trail). Format is
// key=value slog text, timestamped, so a promote/reconcile can be reconstructed
// entirely from stderr.
func newLogger(verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	return slog.New(h)
}

// track lazily opens (and caches) the SQLite tracking DB. First open brings the
// schema to the current version — dropping+recreating on a version change while
// carrying the active slot across — and a reset is the cue to reconcile the
// socat proxy (only if the experiment is installed; otherwise a no-op).
func (a *app) track(ctx context.Context) (*track.DB, error) {
	if a.trackDB != nil {
		return a.trackDB, nil
	}
	db, reset, err := track.Open(ctx, track.Options{
		Path:         a.cfg.TrackDBPath(),
		Logger:       a.log,
		ActiveMirror: a.cfg.ActiveMachinePath(),
		IPMirror:     a.cfg.MachineIPPath,
		MirrorUID:    a.cfg.HostUID,
		MirrorGID:    a.cfg.HostGID,
	})
	if err != nil {
		return nil, err
	}
	a.trackDB = db
	if reset {
		a.log.Warn("tracking schema was reset — reconciling socat proxy if installed")
		a.reconcileProxyIfInstalled(ctx, "schema reset")
	}
	return db, nil
}

// reconciler builds the socat proxy reconciler from config. socat is resolved
// from PATH; an absolute path is baked into the plist so launchd (cwd "/") finds
// it. A missing socat is a clear, actionable error rather than a launchd EX_*.
func (a *app) reconciler() (*socatproxy.Reconciler, error) {
	socat, err := exec.LookPath("socat")
	if err != nil {
		return nil, fmt.Errorf("socat not found on PATH (brew install socat): %w", err)
	}
	return &socatproxy.Reconciler{
		Prefix:   a.cfg.MachinePrefix,
		Bind:     a.cfg.ForwardBind,
		Socat:    socat,
		LockPath: a.cfg.ReconcileLockPath(),
		LogPath:  a.cfg.SocatLogPath,
		UID:      a.cfg.HostUID,
		Log:      a.log,
	}, nil
}

// proxyPorts is the role->port map the reconciler/status need. socat binds the
// CANONICAL client ports (5442/5443) so existing external configs hit it.
func (a *app) proxyPorts() map[string]int {
	return map[string]int{"active": a.cfg.ClientActivePort, "staging": a.cfg.ClientStagingPort}
}

// desiredProxyTargets reads the current active pointer + machine IPs from the DB
// in one atomic snapshot and maps them onto the socat ports.
func (a *app) desiredProxyTargets(ctx context.Context, db *track.DB) ([]socatproxy.Target, error) {
	ts, err := db.DesiredTargets(ctx, a.cfg.ClientActivePort, a.cfg.ClientStagingPort, a.cfg.BackendPort)
	if err != nil {
		return nil, err
	}
	out := make([]socatproxy.Target, 0, len(ts))
	for _, t := range ts {
		out = append(out, socatproxy.Target{Role: t.Role, Port: t.Port, Target: t.Target})
	}
	return out, nil
}

// reconcileProxy is the full guarded reconcile: read desired targets from the DB
// (atomic snapshot), apply them to the socat LaunchAgents under the flock, and
// record what verified-applied back into the DB (observability). Returns the
// per-role outcome.
func (a *app) reconcileProxy(ctx context.Context, reason string) ([]socatproxy.Applied, error) {
	a.log.Info("proxy reconcile requested", "reason", reason)
	db, err := a.track(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := a.reconciler()
	if err != nil {
		return nil, err
	}
	// Targets are fetched INSIDE Reconcile, under the flock, so a stale snapshot
	// can never be applied last (see socatproxy.Reconcile).
	return rec.Reconcile(ctx,
		func(ctx context.Context) ([]socatproxy.Target, error) {
			return a.desiredProxyTargets(ctx, db)
		},
		func(t socatproxy.Target) error {
			return db.RecordApplied(ctx, track.Target{Role: t.Role, Port: t.Port, Target: t.Target})
		})
}

// appliedErr turns per-role convergence failures into a command error so
// `proxy reconcile`/`install` (and the make targets chaining on them) exit
// non-zero instead of printing FAILED and returning success.
func appliedErr(applied []socatproxy.Applied) error {
	var bad []string
	for _, ap := range applied {
		if ap.Err != nil {
			bad = append(bad, ap.Role)
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("socat proxy: %s did not converge (see log above)", strings.Join(bad, ", "))
	}
	return nil
}

// reconcileProxyIfInstalled reconciles ONLY when the socat experiment is already
// installed (at least one plist on disk). This keeps promote/refresh completely
// hands-off for anyone not opted into the socat trial — they never touch
// launchd — while keeping an installed proxy in step with the active pointer and
// IP drift. Never fails the caller: a reconcile problem is logged, not fatal.
func (a *app) reconcileProxyIfInstalled(ctx context.Context, reason string) {
	rec, err := a.reconciler()
	if err != nil {
		a.log.Debug("socat proxy not reconciled (no socat)", "err", err)
		return
	}
	if !rec.AnyInstalled() {
		a.log.Debug("socat proxy not installed — skipping reconcile", "reason", reason)
		return
	}
	applied, err := a.reconcileProxy(ctx, reason)
	if err != nil {
		a.log.Error("socat proxy reconcile failed", "reason", reason, "err", err)
		return
	}
	for _, ap := range applied {
		if ap.Err != nil {
			a.log.Error("socat proxy role did not converge", "role", ap.Role, "err", ap.Err)
		}
	}
}

// ----- `pgdev proxy` command -------------------------------------------------

// proxyCmd groups the socat proxy (internal/socatproxy) that serves the
// canonical client ports (5442/5443), alongside the Go forwarder now on
// 5444/5445. It is opt-in: nothing here runs unless you `proxy
// install`/`reconcile`. See doc/issues/0004.
func (a *app) proxyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "proxy",
		Short: "socat client proxy on the canonical 5442/5443 (Go forwarder moved to 5444/5445)",
	}
	c.AddCommand(
		a.proxyReconcileCmd(),
		a.proxyInstallCmd(),
		a.proxyUninstallCmd(),
		a.proxyStatusCmd(),
	)
	return c
}

func (a *app) proxyReconcileCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reconcile",
		Short: "Re-point the socat LaunchAgents at the active/staging machine IPs (DB-driven, flock-guarded)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			applied, err := a.reconcileProxy(cmd.Context(), "manual reconcile")
			if err != nil {
				return err
			}
			printApplied(applied)
			return appliedErr(applied)
		},
	}
}

// proxyInstallCmd is the ONE reference command for "install anything proxy": it
// brings up BOTH client paths — the client-facing socat proxy on the canonical
// 5442/5443 AND the Go forwarder on 5444/5445 — so there is no separate
// endpoint.install step to remember. The Go forwarder install is best-effort
// (it self-heals and warns rather than failing, and honors
// PG_ENDPOINT_AUTOINSTALL=0), because socat is the canonical path whose outcome
// must be surfaced; the forwarder is the secondary integration path.
func (a *app) proxyInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Install everything: the socat proxy (canonical 5442/5443) AND the Go forwarder (5444/5445)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			// 1. Go forwarder on 5444/5445. A FULL (re)install, not a gentle
			//    ensure: this boots any pre-swap forwarder off the canonical
			//    5442/5443 (which it would otherwise still hold, blocking socat)
			//    and brings it back on 5444/5445 — freeing the ports BEFORE socat
			//    binds them in step 2. best-effort; honors PG_ENDPOINT_AUTOINSTALL=0.
			a.reinstallForwarder(ctx)
			fmt.Printf("==> Go forwarder: active 127.0.0.1:%d  staging 127.0.0.1:%d\n",
				a.cfg.ForwardActivePort, a.cfg.ForwardStagingPort)

			// 2. socat proxy on the canonical client ports (the client-facing path).
			applied, err := a.reconcileProxy(ctx, "install")
			if err != nil {
				return err
			}
			fmt.Printf("==> socat proxy:  active 127.0.0.1:%d  staging 127.0.0.1:%d  (the canonical client ports)\n",
				a.cfg.ClientActivePort, a.cfg.ClientStagingPort)
			printApplied(applied)
			fmt.Println("    (promote/refresh now keep both in step automatically)")
			return appliedErr(applied)
		},
	}
}

func (a *app) proxyUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove both socat LaunchAgents",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rec, err := a.reconciler()
			if err != nil {
				return err
			}
			if err := rec.Uninstall(cmd.Context(), a.proxyPorts()); err != nil {
				return err
			}
			fmt.Println("==> socat proxy removed.")
			return nil
		},
	}
}

func (a *app) proxyStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "socat LaunchAgent state and last-applied targets",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			rec, err := a.reconciler()
			if err != nil {
				return err
			}
			live := rec.Status(ctx, a.proxyPorts())
			db, err := a.track(ctx)
			if err != nil {
				return err
			}
			applied, err := db.AppliedTargets(ctx)
			if err != nil {
				return err
			}
			for _, role := range []string{"active", "staging"} {
				fmt.Printf("%-8s :%d  %s", role, a.cfg.ClientPort(role), live[role])
				if t, ok := applied[role]; ok {
					fmt.Printf("  (db target: %s)", orNone(t.Target))
				}
				fmt.Println()
			}
			return nil
		},
	}
}

func printApplied(applied []socatproxy.Applied) {
	for _, ap := range applied {
		if ap.Err != nil {
			fmt.Printf("    %-8s :%d  FAILED: %v\n", ap.Role, ap.Port, ap.Err)
			continue
		}
		fmt.Printf("    %-8s :%d  %s -> %s\n", ap.Role, ap.Port, ap.Action, orNone(ap.Target))
	}
}
