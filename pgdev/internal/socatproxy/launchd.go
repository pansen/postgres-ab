// Package socatproxy is the EXPERIMENTAL socat-based host client proxy that runs
// ALONGSIDE the resident Go forwarder (internal/forward) during an integration
// phase (see doc/issues/0004). It exists because the Go forwarder's own binary
// trips macOS Local Network Privacy — needing a codesign ceremony and still
// throwing occasional prompts — whereas Homebrew's socat does not. So we trial
// routing through socat under launchd on a SECOND pair of ports (5444 active /
// 5445 staging), leaving 5442/5443 untouched.
//
// socat cannot re-point itself the way the Go forwarder does: each promote / IP
// drift must rewrite and reload a launchd job. That reload is exactly the
// process-lifecycle race that got socat retired in spec 0003 — an orphaned
// listener still holding the port makes the reloaded socat fail to bind, and the
// stale mapping silently persists (pg_restore → wrong DB). This package tames it
// NOT with the SQLite transaction (that only serializes reconcilers) but with an
// explicit port-free GATE before bind and a post-bind VERIFY that the live socat
// argv actually carries the intended target. Reconcile (reconcile.go) drives it.
package socatproxy

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// job is one role's socat LaunchAgent: label, plist path, and the parameters
// baked into the socat argv. One job per role because a socat process forwards
// exactly one listener.
type job struct {
	label          string // me.pansen.<prefix>-socat-<role>
	plist          string // ~/Library/LaunchAgents/<label>.plist
	role           string // active | staging
	bind           string // 127.0.0.1
	port           int    // 5444 | 5445
	backend        string // current dial target "ip:port" ("" = no listener wanted)
	socat          string // absolute socat path
	logPath        string // StandardErrorPath (socat -d -d writes here)
	uid            string // decimal uid for gui/<uid>
	connectTimeout int    // socat connect-timeout seconds
	log            *slog.Logger
}

func (j *job) domain() string       { return "gui/" + j.uid }
func (j *job) domainTarget() string { return "gui/" + j.uid + "/" + j.label }
func (j *job) addr() string         { return net.JoinHostPort(j.bind, strconv.Itoa(j.port)) }

// listenArg / dialArg build the two socat addresses. The options are the
// architect-mandated set: reuseaddr+fork for a normal listener, keepalive/nodelay
// both ways, and — critically — connect-timeout on the dial so a drifted/stale
// target refuses fast instead of hanging psql for the ~75s TCP default.
func (j *job) listenArg() string {
	return fmt.Sprintf("TCP-LISTEN:%d,bind=%s,reuseaddr,fork,keepalive,nodelay", j.port, j.bind)
}
func (j *job) dialArg() string {
	return fmt.Sprintf("TCP:%s,connect-timeout=%d,keepalive,nodelay", j.backend, j.connectTimeout)
}

// program is the full socat argv baked into the plist. `-d -d` makes socat log
// connection lifecycle to stderr (→ the log file) so the proxy is as forensically
// chatty as the Go forwarder it shadows.
func (j *job) program() []string {
	return []string{j.socat, "-d", "-d", j.listenArg(), j.dialArg()}
}

// installed reports whether the plist is on disk.
func (j *job) installed() bool {
	_, err := os.Stat(j.plist)
	return err == nil
}

// currentTarget parses the dial target out of the on-disk plist ("" if the plist
// is absent/unparsable). This — not the DB — is what reconcile compares desired
// state against, so a crash between apply and DB-record self-heals: the plist is
// the durable truth of what launchd runs.
func (j *job) currentTarget() string {
	b, err := os.ReadFile(j.plist)
	if err != nil {
		return ""
	}
	for _, s := range plistStrings(b) {
		if strings.HasPrefix(s, "TCP:") {
			rest := strings.TrimPrefix(s, "TCP:")
			if i := strings.IndexByte(rest, ','); i >= 0 {
				rest = rest[:i]
			}
			return rest
		}
	}
	return ""
}

// loaded reports whether launchd currently has the job (best-effort; a timed
// `launchctl print`).
func (j *job) loaded(ctx context.Context) bool {
	return j.launchctl(ctx, 8*time.Second, "print", j.domainTarget()) == nil
}

// reload applies j.backend to launchd with the full safe sequence: bootout, wait
// for exit, GATE on the port being free (kill an orphan if launchd leaked one),
// write the plist, bootstrap, then VERIFY the live socat is listening AND its
// argv carries the intended target. Any verify timeout fails loudly rather than
// assuming success — that is the whole point.
func (j *job) reload(ctx context.Context) error {
	j.log.Info("reloading socat job", "label", j.label, "addr", j.addr(), "target", j.backend)

	if err := j.boototAndWait(ctx); err != nil {
		return err
	}
	if err := j.gatePortFree(ctx); err != nil {
		return err
	}
	if err := j.writePlist(); err != nil {
		return fmt.Errorf("socatproxy: write plist %s: %w", j.label, err)
	}
	if err := j.bootstrap(ctx); err != nil {
		return err
	}
	return j.verifyRunning(ctx)
}

// stop tears the job down entirely (used when a role has no routable target —
// the machine is down): bootout, wait, gate the port free, remove the plist.
// Leaving no listener is the correct "unroutable" state; a client then gets a
// clean connection-refused rather than a hang against a dead backend.
func (j *job) stop(ctx context.Context) error {
	j.log.Info("stopping socat job (no routable target)", "label", j.label, "addr", j.addr())
	if err := j.boototAndWait(ctx); err != nil {
		return err
	}
	if err := j.gatePortFree(ctx); err != nil {
		return err
	}
	if err := os.Remove(j.plist); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("socatproxy: remove plist %s: %w", j.plist, err)
	}
	return nil
}

// boototAndWait boots the job out and polls until launchd reports it gone, so a
// subsequent bootstrap can't race a still-loaded label and so the forked socat
// children have actually exited before we probe the port. A job that will NOT
// unload is a HARD error, not a warning: the job has KeepAlive, so killing its
// port holder in the gate would just have launchd resurrect it — reporting
// success there would silently pin the old mapping. Better to fail the reconcile
// loudly and let the operator see it.
func (j *job) boototAndWait(ctx context.Context) error {
	_ = j.launchctl(ctx, 10*time.Second, "bootout", j.domainTarget()) // tolerate "not loaded"
	deadline := time.Now().Add(8 * time.Second)
	for {
		if !j.loaded(ctx) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("socatproxy: %s: launchd still reports the job loaded 8s after bootout — refusing to proceed (a KeepAlive job that won't unload would resurrect the old listener and pin a stale mapping)", j.label)
		}
		if err := sleep(ctx, 200*time.Millisecond); err != nil {
			return err
		}
	}
}

// gatePortFree is the orphan-listener defense: it confirms bind:port is actually
// bindable before we lay down a new listener. If launchd leaked a socat child
// that still holds the port, it kills that child (by port, verified LISTEN) and
// re-probes — logging an ERROR because a leak means launchd teardown misbehaved.
// A port that never frees is a hard failure: better to abort the reconcile than
// bootstrap a socat that will crash-loop on EADDRINUSE and silently keep the old
// mapping (the spec-0003 failure).
func (j *job) gatePortFree(ctx context.Context) error {
	if j.probeFree() {
		return nil
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if j.probeFree() {
			return nil
		}
		if err := sleep(ctx, 200*time.Millisecond); err != nil {
			return err
		}
	}
	// Still held — reap the orphan and try once more.
	if pids := listenerPIDs(ctx, j.port); len(pids) > 0 {
		j.log.Error("port still held after bootout — reaping orphaned listener(s) (launchd teardown leaked)",
			"addr", j.addr(), "pids", pids)
		for _, pid := range pids {
			_ = killPID(pid)
		}
		if err := sleep(ctx, 300*time.Millisecond); err != nil {
			return err
		}
	}
	if j.probeFree() {
		return nil
	}
	return fmt.Errorf("socatproxy: %s: port %s never freed — refusing to bootstrap onto a held port", j.label, j.addr())
}

// probeFree returns true if we can bind bind:port right now (immediately closed).
// A successful Listen is the cheapest, most truthful "is the port free" probe.
func (j *job) probeFree() bool {
	ln, err := net.Listen("tcp", j.addr())
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// bootstrap loads the plist, retrying the transient EIO launchd throws for a beat
// after a bootout (same fragility the Go forwarder's launchd code guards against).
func (j *job) bootstrap(ctx context.Context) error {
	var last error
	for i := 0; i < 4; i++ {
		if err := j.launchctl(ctx, 15*time.Second, "bootstrap", j.domain(), j.plist); err == nil {
			return nil
		} else {
			last = err
		}
		if err := sleep(ctx, 500*time.Millisecond); err != nil {
			return err
		}
	}
	return fmt.Errorf("socatproxy: bootstrap %s: %w", j.label, last)
}

// verifyRunning polls until the port is LISTENing again AND the process holding
// it is a socat whose argv contains the intended target. Because socat's parent
// never dials (the fork child connects at accept time), this argv check is the
// ONLY detector of a stale mapping — so it is mandatory, not optional.
func (j *job) verifyRunning(ctx context.Context) error {
	deadline := time.Now().Add(6 * time.Second)
	for {
		pids := listenerPIDs(ctx, j.port)
		if len(pids) > 0 {
			cmd := processArgs(ctx, pids[0])
			if strings.Contains(cmd, j.backend) {
				j.log.Info("socat job verified live", "label", j.label, "addr", j.addr(), "target", j.backend, "pid", pids[0])
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("socatproxy: %s: listener on %s does not carry target %s (running: %q) — stale mapping",
					j.label, j.addr(), j.backend, cmd)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("socatproxy: %s: no listener on %s after bootstrap — socat failed to start (check %s)",
				j.label, j.addr(), j.logPath)
		}
		if err := sleep(ctx, 200*time.Millisecond); err != nil {
			return err
		}
	}
}

// status is a short human line for `pgdev proxy status`.
func (j *job) status(ctx context.Context) string {
	if !j.installed() {
		return "not installed"
	}
	pids := listenerPIDs(ctx, j.port)
	if len(pids) == 0 {
		return "installed, NOT listening"
	}
	return fmt.Sprintf("listening pid=%d -> %s", pids[0], j.currentTarget())
}

// ----- launchctl / process helpers ------------------------------------------

func (j *job) launchctl(ctx context.Context, timeout time.Duration, args ...string) error {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, "launchctl", args...).CombinedOutput()
	if err != nil {
		j.log.Debug("launchctl", "args", args, "err", err, "out", strings.TrimSpace(string(out)))
	}
	return err
}

// listenerPIDs returns the PIDs LISTENing on a TCP port (lsof; macOS). Best-effort.
func listenerPIDs(ctx context.Context, port int) []int {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "lsof", "-nP", "-t", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN").Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, f := range strings.Fields(string(out)) {
		if p, err := strconv.Atoi(f); err == nil {
			pids = append(pids, p)
		}
	}
	return pids
}

// processArgs returns a process's full command line (ps; macOS). Used to verify
// the live socat carries the intended target.
func processArgs(ctx context.Context, pid int) string {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func killPID(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// ----- plist rendering -------------------------------------------------------

// plistStrings extracts every <string>…</string> value from a plist, used to
// recover the socat argv (and thus the current target) from disk.
func plistStrings(b []byte) []string {
	var out []string
	dec := xml.NewDecoder(bytes.NewReader(b))
	inString := false
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			inString = t.Name.Local == "string"
		case xml.EndElement:
			inString = false
		case xml.CharData:
			if inString {
				out = append(out, string(t))
			}
		}
	}
	return out
}

func xmlEscape(s string) (string, error) {
	var b bytes.Buffer
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return "", err
	}
	return b.String(), nil
}

var plistTemplate = template.Must(template.New("socat-plist").Funcs(template.FuncMap{"xml": xmlEscape}).Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>{{xml .Label}}</string>
    <key>ProgramArguments</key>
    <array>
{{range .Program}}        <string>{{xml .}}</string>
{{end}}    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>ThrottleInterval</key>
    <integer>1</integer>
    <key>ProcessType</key>
    <string>Background</string>
    <key>StandardErrorPath</key>
    <string>{{xml .LogPath}}</string>
    <key>StandardOutPath</key>
    <string>{{xml .LogPath}}</string>
</dict>
</plist>
`))

func (j *job) writePlist() error {
	if err := os.MkdirAll(filepath.Dir(j.plist), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(j.logPath), 0o755); err != nil {
		return err
	}
	data := struct {
		Label   string
		Program []string
		LogPath string
	}{j.label, j.program(), j.logPath}
	var buf bytes.Buffer
	if err := plistTemplate.Execute(&buf, data); err != nil {
		return err
	}
	return os.WriteFile(j.plist, buf.Bytes(), 0o644)
}
