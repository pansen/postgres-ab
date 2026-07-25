package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/lmittmann/tint"
	"github.com/spf13/cobra"

	"pansen.me/pgdev/internal/logx"
	"pansen.me/pgdev/internal/socatproxy"
	"pansen.me/pgdev/internal/track"
)

// newLogger builds the process's structured logger. The default sink is
// lmittmann/tint — a drop-in slog.Handler that renders slog's own key=value
// shape with the level and keys colored. EVERY progress line goes through it
// (the old bare "==> " Printf lines included), so a run reads as one colored,
// timestamped stream on stderr, while stdout keeps only the reports you copy
// from: status tables, endpoints, .pgpass/psql lines, snapshot timelines — and
// the destructive confirm banners, which must render verbatim next to their
// prompt. verbose flips the level to Debug, which the tracking + socat paths use
// to be deliberately chatty (we want a fat forensic trail).
//
// Two knobs, resolved from .env like the rest of the config:
//
//   - PG_LOG_COLOR=auto|always|never — auto (the default) colors only a real
//     terminal and honors NO_COLOR/TERM (logx.UseColor; tint does no detection
//     of its own). always forces escapes through a pipe (`… |& less -R`).
//   - PG_LOG_FORMAT=text|json — json swaps in the stdlib JSON handler, for a run
//     that is captured and machine-parsed rather than watched.
//
// On a terminal the stamp is a bare clock; anywhere else it carries the date
// too, so a redirected promote/reconcile trail stays self-dating.
func newLogger(verbose bool, colorMode, format string) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	if strings.EqualFold(strings.TrimSpace(format), "json") {
		return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	}
	color := logx.UseColor(colorMode, os.Stderr)
	timeFormat := "2006-01-02 15:04:05.000"
	if color {
		timeFormat = "15:04:05.000"
	}
	// Only tint the level when we are actually coloring: with NoColor a tinted
	// value takes a different render path in tint for no gain.
	var replace func([]string, slog.Attr) slog.Attr
	if color {
		replace = greyDebugLevel
	}
	return slog.New(tint.NewTextHandler(os.Stderr, &tint.Options{
		Level:       level,
		TimeFormat:  timeFormat,
		NoColor:     !color,
		ReplaceAttr: replace,
	}))
}

// ansiGrey is tint's palette index for bright black (rendered as \033[90m) —
// the same grey it already uses for timestamps and keys.
const ansiGrey = 8

// greyDebugLevel paints the DBG label grey. tint colors INF green, WRN yellow
// and ERR red but leaves DBG in the default foreground, so with PG_PROXY_DEBUG
// on (the default) the chatty debug stream reads just as loud as the lines that
// matter. Grey lets it recede without losing it.
func greyDebugLevel(groups []string, a slog.Attr) slog.Attr {
	if len(groups) != 0 || a.Key != slog.LevelKey {
		return a
	}
	if lvl, ok := a.Value.Any().(slog.Level); ok && lvl < slog.LevelInfo {
		return tint.Attr(ansiGrey, a)
	}
	return a
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
		Bind:     a.cfg.ClientBind,
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

// renderProxy prints the client proxy's live state — one line per role: the
// LaunchAgent's launchd state and the target the DB last recorded as applied.
// Shared by `proxy status` and the main `status` command so both agree. Never
// fails: with no socat (or nothing installed) it says so and moves on, because
// status must keep rendering.
func (a *app) renderProxy(ctx context.Context) {
	rec, err := a.reconciler()
	if err != nil {
		fmt.Printf("client proxy: %v\n", err)
		return
	}
	live := rec.Status(ctx, a.proxyPorts())
	applied := map[string]track.Target{}
	if db, err := a.track(ctx); err == nil {
		if t, err := db.AppliedTargets(ctx); err == nil {
			applied = t
		}
	}
	for _, role := range []string{"active", "staging"} {
		fmt.Printf("proxy %-8s :%d  %s", role, a.cfg.ClientPort(role), live[role])
		if t, ok := applied[role]; ok {
			fmt.Printf("  (db target: %s)", orNone(t.Target))
		}
		fmt.Println()
	}
}

// ----- `pgdev proxy` command -------------------------------------------------

// proxyCmd groups the socat proxy (internal/socatproxy) that serves the client
// ports (5442/5443) — the only host-side client path since the Go forwarder was
// removed. Installing it is explicit (`proxy install`); promote/refresh only
// reconcile an ALREADY-installed proxy. See doc/issues/0004.
func (a *app) proxyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "proxy",
		Short: "socat client proxy on the client ports (5442/5443)",
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
			a.logApplied(applied)
			return appliedErr(applied)
		},
	}
}

// proxyInstallCmd brings up the client path: the socat LaunchAgents on the
// client ports. Installing is what opts promote/refresh into reconciling, so
// this is the one command to run after a fresh checkout or a `proxy uninstall`.
func (a *app) proxyInstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Install the socat client proxy on the client ports (5442/5443)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if !isLoopback(a.cfg.ClientBind) {
				a.log.Warn("this bind exposes the dev-credentialed PostgreSQL backend on every interface (LAN/Wi-Fi)",
					"bind", a.cfg.ClientBind)
			}
			applied, err := a.reconcileProxy(ctx, "install")
			if err != nil {
				return err
			}
			a.log.Info("socat proxy installed on the client ports — promote/refresh now keep it in step automatically",
				"active", fmt.Sprintf("%s:%d", a.cfg.ClientBind, a.cfg.ClientActivePort),
				"staging", fmt.Sprintf("%s:%d", a.cfg.ClientBind, a.cfg.ClientStagingPort))
			a.logApplied(applied)
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
			a.log.Info("socat proxy removed")
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
			a.renderProxy(cmd.Context())
			return nil
		},
	}
}

// logApplied reports the per-role outcome of a reconcile. A role that did not
// converge is an ERROR, not a printed line the eye slides over — the reconcile
// as a whole then fails via appliedErr.
func (a *app) logApplied(applied []socatproxy.Applied) {
	for _, ap := range applied {
		if ap.Err != nil {
			a.log.Error("proxy role did not converge", "role", ap.Role, "port", ap.Port, "err", ap.Err)
			continue
		}
		a.log.Info("proxy role applied", "role", ap.Role, "port", ap.Port, "action", ap.Action, "target", orNone(ap.Target))
	}
}

// isLoopback reports whether the client listeners stay on the loopback — the
// safe default. Anything else publishes the dev-credentialed backend to the
// network, so `proxy install` says so out loud.
func isLoopback(bind string) bool {
	return bind == "" || bind == "127.0.0.1" || bind == "::1" || bind == "localhost"
}
