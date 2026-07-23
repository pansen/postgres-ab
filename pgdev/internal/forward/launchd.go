package forward

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

var plistTemplate = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>{{.Label}}</string>
    <key>ProgramArguments</key>
    <array>
{{range .Program}}        <string>{{.}}</string>
{{end}}    </array>
    <key>WorkingDirectory</key>
    <string>{{.RepoRoot}}</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PG_REPO_ROOT</key>
        <string>{{.RepoRoot}}</string>
    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>ProcessType</key>
    <string>Background</string>
    <key>StandardOutPath</key>
    <string>{{.LogPath}}</string>
    <key>StandardErrorPath</key>
    <string>{{.LogPath}}</string>
</dict>
</plist>
`))

func (l *Launchd) writePlist() error {
	if err := os.MkdirAll(filepath.Dir(l.Plist), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	// text/template escapes nothing for XML; the values here are our own binary
	// path, a fixed label and a log path under the repo — no untrusted input.
	if err := plistTemplate.Execute(&buf, l); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.LogPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(l.Plist, buf.Bytes(), 0o644)
}
