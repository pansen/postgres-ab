package forward

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// Launchd manages the per-user LaunchAgent that keeps `pgdev forward serve`
// running across logins. Unlike the retired socat agent, this one NEVER needs
// restarting to re-point — the resident process swaps targets itself — so
// promote/refresh no longer touch launchd at all. The agent exists purely for
// persistence (RunAtLoad) and crash-restart (KeepAlive).
type Launchd struct {
	Label     string   // me.pansen.<prefix>-forward
	Plist     string   // ~/Library/LaunchAgents/<label>.plist
	Program   []string // argv, e.g. [<pgdev-abs-path>, forward, serve]
	LogPath   string   // combined stdout/stderr log
	RepoRoot  string   // WorkingDirectory + PG_REPO_ROOT for the agent
	UID       string   // decimal uid for the gui/<uid> domain; "" = current user
	ReapPorts []int    // client ports whose stale (socat) holders to kill on install
}

// NewLaunchd derives the standard agent layout for a machine prefix. program is
// the argv the agent runs (the absolute pgdev path plus "forward serve").
// repoRoot is baked into the plist as WorkingDirectory AND PG_REPO_ROOT: launchd
// starts agents with cwd "/", so config.Load()'s os.Getwd() fallback would
// otherwise resolve var/ under "/" and the forwarder would watch the wrong files.
func NewLaunchd(prefix string, program []string, logPath, repoRoot string) *Launchd {
	label := "me.pansen." + prefix + "-forward"
	home, _ := os.UserHomeDir()
	return &Launchd{
		Label:    label,
		Plist:    filepath.Join(home, "Library", "LaunchAgents", label+".plist"),
		Program:  program,
		LogPath:  logPath,
		RepoRoot: repoRoot,
		UID:      strconv.Itoa(os.Getuid()),
	}
}

// Installed reports whether the plist is on disk.
func (l *Launchd) Installed() bool {
	_, err := os.Stat(l.Plist)
	return err == nil
}

func (l *Launchd) domainTarget() string { return "gui/" + l.UID + "/" + l.Label }
func (l *Launchd) domain() string       { return "gui/" + l.UID }

// Install writes the plist and (re)loads the agent, in an order that survives
// the migration off the retired socat agent (which shares this exact label):
//
//  1. write the plist;
//  2. bootout any prior instance AND WAIT until it is actually gone — the old
//     agent has KeepAlive, so it would respawn socat the instant we reaped
//     ports if it were still loaded;
//  3. only THEN reap stale holders of the client ports — orphaned socat
//     children survive bootout but nothing respawns them now, so the new serve
//     can bind instead of crash-looping on EADDRINUSE;
//  4. bootstrap (RunAtLoad starts serve immediately), retrying the transient EIO
//     launchd sometimes returns right after a bootout.
//
// No kickstart: the fresh bootout+bootstrap already starts a new instance, and a
// blocking `kickstart -k` against a crash-looping service is exactly what used
// to hang install. Idempotent.
func (l *Launchd) Install(ctx context.Context) error {
	if err := l.writePlist(); err != nil {
		return err
	}
	l.bootoutWait(ctx)
	if len(l.ReapPorts) > 0 {
		ReapListeners(l.ReapPorts...)
	}
	return l.bootstrap(ctx)
}

// bootoutWait boots the agent out and polls until launchd reports it unloaded
// (or a short deadline passes). bootout on a running `serve` sends SIGTERM and
// returns once it exits — serve handles SIGTERM promptly — so this is normally a
// single pass; the poll just guards against launchd lag before we bootstrap.
func (l *Launchd) bootoutWait(ctx context.Context) {
	for i := 0; i < 20; i++ {
		_ = runTimed(ctx, 10*time.Second, "bootout", l.domainTarget())
		if !l.loaded(ctx) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// bootstrap loads the plist, retrying the transient EIO/"already bootstrapped"
// errors launchd emits for a beat after a bootout. Each attempt is time-bounded
// so a wedged launchctl can't hang install indefinitely.
func (l *Launchd) bootstrap(ctx context.Context) error {
	var last error
	for i := 0; i < 4; i++ {
		out, err := combinedTimed(ctx, 15*time.Second, "bootstrap", l.domain(), l.Plist)
		if err == nil {
			return nil
		}
		last = fmt.Errorf("launchctl bootstrap %s: %w: %s", l.Label, err, strings.TrimSpace(out))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return last
}

func (l *Launchd) loaded(ctx context.Context) bool {
	return runTimed(ctx, 10*time.Second, "print", l.domainTarget()) == nil
}

// Restart kicks the running agent in place so a freshly-granted permission is
// re-evaluated. macOS TCC (notably Local Network Privacy on Tahoe) caches its
// allow/deny decision at process start, so a grant applied while `serve` was
// already running does not take effect until the process restarts — exactly the
// trap where the forwarder keeps returning EHOSTUNREACH after the user has
// ticked the Local Network box. `kickstart -k` SIGKILLs the current instance and
// relaunches it from the loaded job, without rewriting the plist. Time-bounded
// so a wedged launchctl can't hang the caller.
func (l *Launchd) Restart(ctx context.Context) error {
	out, err := combinedTimed(ctx, 15*time.Second, "kickstart", "-k", l.domainTarget())
	if err != nil {
		return fmt.Errorf("launchctl kickstart %s: %w: %s (is the agent installed? run 'pgdev forward install')",
			l.Label, err, strings.TrimSpace(out))
	}
	return nil
}

// EnsureResult reports what Ensure observed and did, so the caller can print an
// honest one-liner (silent on the healthy common path, loud when it healed).
type EnsureResult struct {
	Action string // "healthy", "installed", or "reinstalled"
	Reason string // why it (re)installed; empty when Action == "healthy"
}

// Ensure validates the installed LaunchAgent against this Launchd's desired
// configuration and self-heals a missing, unloaded, stale, or dead agent by
// (re)installing it. It is the guard `make start` runs so the moved/renamed-repo
// trap — a plist left pointing at a deleted binary, which merely being "on disk"
// (Installed) does not catch — can no longer silently persist and leave the
// client ports dead behind an opaque EX_CONFIG. Healthy is the cheap common
// path: a single `launchctl print` and a compare, no writes, no reload.
func (l *Launchd) Ensure(ctx context.Context) (EnsureResult, error) {
	heal := func(action, reason string) (EnsureResult, error) {
		if err := l.Install(ctx); err != nil {
			return EnsureResult{}, err
		}
		return EnsureResult{Action: action, Reason: reason}, nil
	}
	if !l.Installed() {
		return heal("installed", "no LaunchAgent was installed")
	}
	info, ok := l.inspect(ctx)
	if !ok {
		return heal("reinstalled", "the plist was on disk but launchd had not loaded it")
	}
	if got, stale := info.stale(l.Program); stale {
		return heal("reinstalled", fmt.Sprintf("it pointed at a stale program (%s)", got))
	}
	if !info.running {
		return heal("reinstalled", "the agent was loaded but not running (crashed?)")
	}
	return EnsureResult{Action: "healthy"}, nil
}

// printInfo is the slice of `launchctl print` output Ensure reasons over.
type printInfo struct {
	program string   // the scalar `program =` line
	args    []string // the `arguments = { ... }` block (ProgramArguments)
	running bool     // a live pid / `state = running`
}

// stale reports the observed program (for the message) and whether it differs
// from want (the desired argv). It prefers the full arguments array and falls
// back to the scalar program path; with neither readable it reports NOT stale,
// so a parse miss never triggers a needless reinstall.
func (p printInfo) stale(want []string) (string, bool) {
	if len(p.args) > 0 {
		return strings.Join(p.args, " "), !slices.Equal(p.args, want)
	}
	if p.program != "" {
		return p.program, p.program != want[0]
	}
	return "", false
}

func (l *Launchd) inspect(ctx context.Context) (printInfo, bool) {
	out, err := combinedTimed(ctx, 10*time.Second, "print", l.domainTarget())
	if err != nil {
		return printInfo{}, false
	}
	return parsePrint(out), true
}

// parsePrint extracts the program path, ProgramArguments, and running state from
// `launchctl print gui/<uid>/<label>` output — launchctl's human-readable dump,
// with `program = <path>`, an `arguments = { ... }` block, and `state = running`
// / `pid = N` when the job is up.
func parsePrint(out string) printInfo {
	var p printInfo
	inArgs := false
	for _, ln := range strings.Split(out, "\n") {
		t := strings.TrimSpace(ln)
		switch {
		case inArgs:
			if t == "}" {
				inArgs = false
			} else if t != "" {
				p.args = append(p.args, t)
			}
		case strings.HasPrefix(t, "arguments = {"):
			inArgs = true
		case strings.HasPrefix(t, "program ="):
			p.program = strings.TrimSpace(strings.TrimPrefix(t, "program ="))
		case strings.HasPrefix(t, "pid ="), strings.HasPrefix(t, "state = running"):
			p.running = true
		}
	}
	return p
}

// Uninstall stops the agent and removes its plist. Tolerant of an
// already-removed agent.
func (l *Launchd) Uninstall(ctx context.Context) error {
	_ = runTimed(ctx, 10*time.Second, "bootout", l.domainTarget())
	if err := os.Remove(l.Plist); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// runTimed runs a launchctl subcommand with its own timeout so a wedged
// launchctl (a known launchd fragility) can never hang the caller.
func runTimed(ctx context.Context, timeout time.Duration, args ...string) error {
	_, err := combinedTimed(ctx, timeout, args...)
	return err
}

func combinedTimed(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, "launchctl", args...).CombinedOutput()
	return string(out), err
}

// Status returns the launchctl print excerpt (state/pid) for the agent, or a
// short "not loaded" note.
func (l *Launchd) Status(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "launchctl", "print", l.domainTarget()).CombinedOutput()
	if err != nil {
		return "launchd: not loaded"
	}
	var lines []string
	for _, ln := range strings.Split(string(out), "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "state =") || strings.HasPrefix(t, "pid =") {
			lines = append(lines, t)
		}
	}
	if len(lines) == 0 {
		return "launchd: loaded"
	}
	return "launchd: " + strings.Join(lines, ", ")
}

// xmlEscape renders a value as XML character data. Paths and labels are "ours",
// but a path can legally contain '&' or '<', which would otherwise produce an
// invalid plist and fail bootstrap in a baffling way — so every interpolated
// <string> goes through this.
func xmlEscape(s string) (string, error) {
	var b bytes.Buffer
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return "", err
	}
	return b.String(), nil
}

var plistTemplate = template.Must(template.New("plist").Funcs(template.FuncMap{"xml": xmlEscape}).Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>{{xml .Label}}</string>
    <key>ProgramArguments</key>
    <array>
{{range .Program}}        <string>{{xml .}}</string>
{{end}}    </array>
    <key>WorkingDirectory</key>
    <string>{{xml .RepoRoot}}</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PG_REPO_ROOT</key>
        <string>{{xml .RepoRoot}}</string>
    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>ProcessType</key>
    <string>Background</string>
    <key>StandardOutPath</key>
    <string>{{xml .LogPath}}</string>
    <key>StandardErrorPath</key>
    <string>{{xml .LogPath}}</string>
</dict>
</plist>
`))

func (l *Launchd) writePlist() error {
	if err := os.MkdirAll(filepath.Dir(l.Plist), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := plistTemplate.Execute(&buf, l); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.LogPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(l.Plist, buf.Bytes(), 0o644)
}
