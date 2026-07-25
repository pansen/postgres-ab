package socatproxy

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Target is one role's desired upstream, mirrored from track.Target so this
// package need not import track (keeps the dependency one-way: the caller wires
// track's output into Reconcile).
type Target struct {
	Role   string // active | staging
	Port   int    // 5444 | 5445
	Target string // "ip:port" or "" (unroutable)
}

// Reconciler owns the two socat LaunchAgents and the cross-process lock that
// makes a reconcile atomic against a concurrent one.
type Reconciler struct {
	Prefix         string                   // vpg -> me.pansen.vpg-socat-<role>
	Bind           string                   // 127.0.0.1
	Socat          string                   // absolute socat path
	LockPath       string                   // var/reconcile.flock (flock target)
	LogPath        func(role string) string // per-role socat log path
	UID            string                   // decimal uid; "" = current user
	ConnectTimeout int                      // socat dial connect-timeout seconds (default 5)
	Log            *slog.Logger             // structured sink; nil = slog.Default()
}

func (r *Reconciler) logger() *slog.Logger {
	l := r.Log
	if l == nil {
		l = slog.Default()
	}
	return l.With("component", "socatproxy")
}

func (r *Reconciler) uid() string {
	if r.UID != "" {
		return r.UID
	}
	return strconv.Itoa(os.Getuid())
}

func (r *Reconciler) connectTimeout() int {
	if r.ConnectTimeout > 0 {
		return r.ConnectTimeout
	}
	return 5
}

// job builds the LaunchAgent handle for one role/target.
func (r *Reconciler) job(role string, port int, target string) *job {
	label := "me.pansen." + r.Prefix + "-socat-" + role
	home, _ := os.UserHomeDir()
	logPath := ""
	if r.LogPath != nil {
		logPath = r.LogPath(role)
	}
	return &job{
		label:          label,
		plist:          filepath.Join(home, "Library", "LaunchAgents", label+".plist"),
		role:           role,
		bind:           r.Bind,
		port:           port,
		backend:        target,
		socat:          r.Socat,
		logPath:        logPath,
		uid:            r.uid(),
		connectTimeout: r.connectTimeout(),
		log:            r.logger(),
	}
}

// Applied reports, per role, what one reconcile actually did — for honest
// caller output and for recording into the DB observability table.
type Applied struct {
	Role   string
	Port   int
	Target string // "" = torn down (unroutable)
	Action string // "unchanged" | "reloaded" | "stopped" | "installed"
	Err    error  // non-nil if this role failed to converge
}

// Reconcile drives both socat jobs to their desired targets, under the flock so
// two concurrent `pgdev` reconciles cannot interleave. CRITICAL: `fetch` (the
// atomic DB snapshot of active+IPs) is called AFTER the lock is held, not before
// — otherwise a reconcile that snapshotted pre-promote state could win the lock
// last and apply a stale mapping over a newer one (the wrong-DB hazard). The
// flock + fetch-under-lock together give the "one clear outcome". record (may be
// nil) stamps the observability table AFTER each launchd change verified.
func (r *Reconciler) Reconcile(ctx context.Context, fetch func(context.Context) ([]Target, error), record func(Target) error) ([]Applied, error) {
	log := r.logger()
	unlock, err := r.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	targets, err := fetch(ctx)
	if err != nil {
		return nil, fmt.Errorf("socatproxy: fetch desired targets under lock: %w", err)
	}
	log.Debug("reconcile lock acquired", "lock", r.LockPath, "targets", len(targets))

	out := make([]Applied, 0, len(targets))
	for _, t := range targets {
		a := r.reconcileOne(ctx, t)
		out = append(out, a)
		if a.Err != nil {
			log.Error("role failed to converge", "role", t.Role, "target", t.Target, "err", a.Err)
			continue
		}
		if record != nil {
			if err := record(Target{Role: t.Role, Port: t.Port, Target: t.Target}); err != nil {
				log.Warn("recording applied target failed (observability only)", "role", t.Role, "err", err)
			}
		}
	}
	log.Info("reconcile complete", "roles", len(out))
	return out, nil
}

// reconcileOne converges a single role. Desired state is compared against the
// on-disk plist (the durable truth of what launchd runs), NOT the DB, so a crash
// between apply and record self-heals on the next pass.
func (r *Reconciler) reconcileOne(ctx context.Context, t Target) Applied {
	j := r.job(t.Role, t.Port, t.Target)
	res := Applied{Role: t.Role, Port: t.Port, Target: t.Target}

	if t.Target == "" {
		// Unroutable: ensure no listener exists.
		if !j.installed() && !j.loaded(ctx) {
			res.Action = "unchanged"
			return res
		}
		if err := j.stop(ctx); err != nil {
			res.Err = err
			return res
		}
		res.Action = "stopped"
		return res
	}

	// "Healthy" requires not just the plist matching, but a LIVE socat whose argv
	// actually carries the target — the same check verifyRunning makes. Without
	// it a job that relaunched on a stale argv (e.g. a failed reload that left the
	// old KeepAlive child running) would read as "unchanged" forever and pin the
	// stale mapping. The plist match alone is not enough.
	current := j.currentTarget()
	healthy := false
	if j.installed() && current == t.Target {
		if pids := listenerPIDs(ctx, t.Port); len(pids) > 0 && strings.Contains(processArgs(ctx, pids[0]), t.Target) {
			healthy = true
		}
	}
	if healthy {
		j.log.Debug("role already healthy", "role", t.Role, "target", t.Target)
		res.Action = "unchanged"
		return res
	}

	action := "reloaded"
	if !j.installed() {
		action = "installed"
	}
	if err := j.reload(ctx); err != nil {
		res.Err = err
		return res
	}
	res.Action = action
	return res
}

// AnyInstalled reports whether either socat LaunchAgent plist is on disk — i.e.
// whether the operator has opted into the experiment. promote/refresh use it to
// stay completely hands-off (never touching launchd) until `proxy install`.
func (r *Reconciler) AnyInstalled() bool {
	for _, role := range []string{"active", "staging"} {
		if r.job(role, 0, "").installed() {
			return true
		}
	}
	return false
}

// Status returns a per-role human status line for `pgdev proxy status`.
func (r *Reconciler) Status(ctx context.Context, ports map[string]int) map[string]string {
	out := map[string]string{}
	for _, role := range []string{"active", "staging"} {
		j := r.job(role, ports[role], "")
		out[role] = j.status(ctx)
	}
	return out
}

// Uninstall tears both jobs down (for `pgdev proxy uninstall`).
func (r *Reconciler) Uninstall(ctx context.Context, ports map[string]int) error {
	unlock, err := r.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	var firstErr error
	for _, role := range []string{"active", "staging"} {
		j := r.job(role, ports[role], "")
		if err := j.stop(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ----- cross-process reconcile lock (flock) ----------------------------------

// lock takes an exclusive advisory flock on LockPath, blocking (with a ctx-bounded
// spin) until it is free. flock is the reconcile mutex — deliberately OUTSIDE any
// SQLite transaction so a wedged launchctl can never hold a DB write lock and
// brick every other `pgdev` command. The lock dies with the process, so there is
// no stale-lockfile recovery to get wrong.
func (r *Reconciler) lock(ctx context.Context) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(r.LockPath), 0o755); err != nil {
		return nil, fmt.Errorf("socatproxy: mkdir for lock: %w", err)
	}
	f, err := os.OpenFile(r.LockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("socatproxy: open lock %s: %w", r.LockPath, err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if err != syscall.EWOULDBLOCK {
			f.Close()
			return nil, fmt.Errorf("socatproxy: flock %s: %w", r.LockPath, err)
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("socatproxy: timed out waiting for reconcile lock %s (another reconcile is running)", r.LockPath)
		}
		if err := sleep(ctx, 200*time.Millisecond); err != nil {
			f.Close()
			return nil, err
		}
	}
}
