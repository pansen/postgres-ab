package forward

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"pansen.me/pgdev/internal/activeslot"
	"pansen.me/pgdev/internal/logx"
)

// Options configures a Server. The path/lookup fields are what let the forwarder
// track the drifting IPs and the active pointer without a live `container` exec.
type Options struct {
	Bind        string // listen address, default 127.0.0.1
	ActivePort  int    // host client port for the active role (5442)
	StagingPort int    // host client port for the staging role (5443)
	BackendPort int    // port each machine serves its backend on (5432)

	ActiveMachinePath string                   // var/active-machine
	MachineIPPath     func(slot string) string // slot ("a"/"b") -> var/machine-ip-<slot>
	StatePath         string                   // var/forward-state.json (heartbeat + live mapping); "" disables

	PollInterval time.Duration // re-read cadence for the pointer + IP files (default 1s)
	DialTimeout  time.Duration // per-connection upstream dial timeout (default 5s)
	Log          logx.Func     // progress/diagnostic sink (nil = discard)
	Verbose      bool          // extra per-poll DBG tracing (PG_FORWARD_DEBUG=1)
}

// liveConn is one relayed client session, tagged with the role and target it was
// established against so a re-point can selectively tear down the now-stale ones.
type liveConn struct {
	role   string
	target string
	client net.Conn
	up     net.Conn
}

// Server owns the two client listeners and a mutex-guarded routing table it
// re-points from the watched state files. Listeners are bound once and never
// rebound; only the dial targets swap.
type Server struct {
	opts Options
	log  logx.Func

	mu      sync.RWMutex
	targets map[string]string      // role -> "ip:port" ("" = unroutable)
	live    map[int64]liveConn     // established sessions, for drop-on-repoint
	nextID  int64                  // monotonic connection id (logging + live-map key)
	dial    map[string]*dialHealth // per-role dial outcome (health summary + log throttle)

	started time.Time // process start, for uptime in the heartbeat
}

// dialHealth tracks the outcome of upstream dials for one role, so the heartbeat
// can report "active: ok, last 3s ago" vs "staging: FAILING 12x EHOSTUNREACH"
// and so repeated identical failures are throttled instead of flooding the log.
type dialHealth struct {
	lastOK    time.Time // last successful dial
	lastErr   time.Time // last failed dial
	lastClass string    // classifyDial() bucket of the last failure
	fails     int       // consecutive failures since the last success
	loggedAt  time.Time // last failure line we actually emitted (throttle anchor)
}

// New builds a Server, filling defaults.
func New(o Options) *Server {
	if o.Bind == "" {
		o.Bind = "127.0.0.1"
	}
	if o.PollInterval <= 0 {
		o.PollInterval = time.Second
	}
	if o.DialTimeout <= 0 {
		o.DialTimeout = 5 * time.Second
	}
	return &Server{
		opts:    o,
		log:     logx.Or(o.Log),
		targets: map[string]string{},
		live:    map[int64]liveConn{},
		dial:    map[string]*dialHealth{},
	}
}

// ----- logging ---------------------------------------------------------------
//
// Every forwarder line carries a millisecond timestamp and a level tag, because
// this log is the primary forensic record when the endpoint mysteriously stops
// working (see the macOS Local Network Privacy trap). "was it ever working, and
// exactly when did it flip?" must be answerable from this file alone. Format
// strings are compile-time literals; values ride in args.

func (s *Server) logl(level, format string, args ...any) {
	stamp := time.Now().Format("2006-01-02 15:04:05.000")
	s.log("%s %-4s "+format, append([]any{stamp, level}, args...)...)
}

func (s *Server) info(format string, args ...any) { s.logl("INFO", format, args...) }
func (s *Server) warn(format string, args ...any) { s.logl("WARN", format, args...) }

// debug lines (per-poll target recomputation) are silent unless Verbose — they
// are the "rather too much" tier, one line per second, opt-in via
// PG_FORWARD_DEBUG=1.
func (s *Server) debug(format string, args ...any) {
	if s.opts.Verbose {
		s.logl("DBG", format, args...)
	}
}

// role pairs a listener with the routing key it serves.
type role struct {
	name string
	port int
}

// Serve binds both listeners, starts the poll-watcher, and forwards until ctx is
// cancelled. It returns after the listeners are closed and their accept loops
// have stopped. A bind failure on either port is fatal (returned) — unlike the
// old socat path there is nothing to orphan, so a clean error is the whole point.
func (s *Server) Serve(ctx context.Context) error {
	s.started = time.Now()
	s.logBoot()
	s.reload() // seed the table before accepting, so the first conn already routes

	roles := []role{{"active", s.opts.ActivePort}, {"staging", s.opts.StagingPort}}
	lns := make([]net.Listener, 0, len(roles))
	lc := net.ListenConfig{}
	for _, r := range roles {
		addr := net.JoinHostPort(s.opts.Bind, strconv.Itoa(r.port))
		ln, err := lc.Listen(ctx, "tcp", addr)
		if err != nil {
			s.warn("FATAL: cannot bind %s (%s): %v — is another forwarder/socat holding the port?", addr, r.name, err)
			for _, done := range lns {
				done.Close()
			}
			return fmt.Errorf("listen %s (%s): %w", addr, r.name, err)
		}
		s.info("listening on %s (%s)", addr, r.name)
		lns = append(lns, ln)
	}

	var wg sync.WaitGroup
	for i, r := range roles {
		wg.Add(1)
		go func(ln net.Listener, r role) {
			defer wg.Done()
			s.acceptLoop(ctx, ln, r.name)
		}(lns[i], r)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		s.watch(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		s.heartbeat(ctx)
	}()

	<-ctx.Done()
	s.info("shutdown: signal received, closing %d listener(s) and %d live session(s) (up %s)",
		len(lns), s.liveN(), time.Since(s.started).Round(time.Second))
	for _, ln := range lns {
		ln.Close() // unblocks the accept loops
	}
	s.closeAllLive() // drop established sessions so shutdown is prompt (ExitTimeOut)
	wg.Wait()
	return nil
}

// logBoot dumps the full effective configuration and the initial resolved
// mapping (with the IP-file provenance) at startup — the "what did this process
// see when it came up" record every incident starts from.
func (s *Server) logBoot() {
	s.info("boot: pgdev forward serve pid=%d bind=%s active=:%d staging=:%d backend=:%d poll=%s dial-timeout=%s verbose=%t",
		os.Getpid(), s.opts.Bind, s.opts.ActivePort, s.opts.StagingPort, s.opts.BackendPort,
		s.opts.PollInterval, s.opts.DialTimeout, s.opts.Verbose)
	s.info("boot: watching pointer=%s ip-a=%s ip-b=%s state=%s",
		s.opts.ActiveMachinePath, s.opts.MachineIPPath("a"), s.opts.MachineIPPath("b"), orNone(s.opts.StatePath))
	slot := activeslot.Pointer{Path: s.opts.ActiveMachinePath}.Get()
	ipA, ipB := readIP(s.opts.MachineIPPath("a")), readIP(s.opts.MachineIPPath("b"))
	active, staging := Targets(slot, ipA, ipB, s.opts.BackendPort)
	s.info("boot: initial active-slot=%s ip-a=%s ip-b=%s -> active=%s staging=%s",
		orNone(slot), orNone(ipA), orNone(ipB), orNone(active), orNone(staging))
	if !isLoopbackBind(s.opts.Bind) {
		s.warn("boot: bind=%s is NOT loopback — the dev-credentialed backend is exposed on every interface", s.opts.Bind)
	}
}

// heartbeat emits one summary line on a slow ticker: uptime, live session count,
// and each role's current target plus its dial health. This is the line that
// answers "was it working, and when did it stop" long after the event — so it
// runs even when idle (deliberately chatty; that is the point).
func (s *Server) heartbeat(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.mu.RLock()
			active, staging := s.targets["active"], s.targets["staging"]
			live := len(s.live)
			as, ss := s.dialSummaryLocked("active"), s.dialSummaryLocked("staging")
			s.mu.RUnlock()
			s.info("beat: uptime=%s live=%d | active=%s [%s] | staging=%s [%s]",
				time.Since(s.started).Round(time.Second), live, orNone(active), as, orNone(staging), ss)
		}
	}
}

// dialSummaryLocked renders one role's dial health for the heartbeat. Caller
// holds s.mu (read is fine; the recorders take the write lock).
func (s *Server) dialSummaryLocked(role string) string {
	h := s.dial[role]
	if h == nil || (h.lastOK.IsZero() && h.fails == 0) {
		return "no dials yet"
	}
	if h.fails > 0 {
		return fmt.Sprintf("FAILING %dx %s, last %s ago", h.fails, h.lastClass, time.Since(h.lastErr).Round(time.Second))
	}
	return fmt.Sprintf("ok, last %s ago", time.Since(h.lastOK).Round(time.Second))
}

// watch re-reads the pointer + IP files on a ticker and swaps the routing table.
// Poll (not fsnotify) is deliberately the simplest thing that is robust on macOS
// — the files change rarely and a ~1s lag on promote is within the "promote may
// drop sessions; reconnect" contract.
func (s *Server) watch(ctx context.Context) {
	t := time.NewTicker(s.opts.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reload()
		}
	}
}

// reload recomputes targets from the current on-disk state, swaps them in, and
// tears down any established session whose role target changed (drop-on-repoint):
// a client left on the demoted machine after a promote would otherwise keep
// reading/writing the wrong database — the exact hazard this component exists to
// remove. New connections use the new target; dropped clients simply reconnect.
func (s *Server) reload() {
	slot := activeslot.Pointer{Path: s.opts.ActiveMachinePath}.Get()
	ipA := readIP(s.opts.MachineIPPath("a"))
	ipB := readIP(s.opts.MachineIPPath("b"))
	active, staging := Targets(slot, ipA, ipB, s.opts.BackendPort)

	s.mu.Lock()
	changed := active != s.targets["active"] || staging != s.targets["staging"]
	s.targets["active"], s.targets["staging"] = active, staging
	var drop []liveConn
	for id, lc := range s.live {
		want := active
		if lc.role == "staging" {
			want = staging
		}
		if lc.target != want {
			drop = append(drop, lc)
			delete(s.live, id)
		}
	}
	s.mu.Unlock()

	for _, lc := range drop {
		lc.client.Close()
		lc.up.Close()
	}
	if changed {
		s.info("route: active -> %s, staging -> %s (slot=%s ip-a=%s ip-b=%s; dropped %d stale session(s))",
			orNone(active), orNone(staging), orNone(slot), orNone(ipA), orNone(ipB), len(drop))
	} else {
		s.debug("poll: active=%s staging=%s (slot=%s ip-a=%s ip-b=%s)",
			orNone(active), orNone(staging), orNone(slot), orNone(ipA), orNone(ipB))
	}
	s.writeState(slot, active, staging)
}

// liveN returns the current live-session count (own lock).
func (s *Server) liveN() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.live)
}

func isLoopbackBind(bind string) bool {
	switch bind {
	case "", "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

func (s *Server) target(role string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.targets[role]
}

// acceptLoop accepts on ln until ctx is done or the listener is closed; each
// accepted conn is handled on its own goroutine so a slow dial never stalls the
// listener. A transient Accept error (e.g. EMFILE, a momentary resource pinch)
// must NOT abandon the role for the process's lifetime — that would defeat the
// whole point of a resident forwarder — so it backs off and retries (the
// net/http.Server pattern), returning only on context cancellation.
func (s *Server) acceptLoop(ctx context.Context, ln net.Listener, role string) {
	var backoff time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return // shutdown: the listener was closed on purpose
			default:
			}
			if backoff == 0 {
				backoff = 5 * time.Millisecond
			} else {
				backoff *= 2
			}
			if backoff > time.Second {
				backoff = time.Second
			}
			s.warn("accept on %s: %v; retrying in %s", role, err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			continue
		}
		backoff = 0
		go s.handle(conn, role)
	}
}

// handle relays one client connection to the current target for its role. The
// target is snapshotted at accept time and the session registered so a later
// re-point can drop it (see reload). Every phase (accept, dial, close) is logged
// with the connection id so a single session can be followed end to end.
func (s *Server) handle(client net.Conn, role string) {
	id := s.nextConnID()
	remote := client.RemoteAddr()
	defer client.Close()

	target := s.target(role)
	if target == "" {
		s.warn("conn#%d %s: no target — machine down or IP file empty; closing client %s", id, role, remote)
		return
	}
	s.debug("conn#%d %s: accept from %s, dialing %s", id, role, remote, target)

	t0 := time.Now()
	upstream, err := net.DialTimeout("tcp", target, s.opts.DialTimeout)
	if err != nil {
		s.recordDialFail(id, role, target, err, time.Since(t0))
		return
	}
	s.recordDialOK(id, role, target, time.Since(t0))
	defer upstream.Close()
	keepAlive(client)
	keepAlive(upstream)

	s.register(id, liveConn{role: role, target: target, client: client, up: upstream})
	defer s.unregister(id)

	up, down := pipe(client, upstream)
	s.info("conn#%d %s: closed after %s (client→up %s, up→client %s) target=%s",
		id, role, time.Since(t0).Round(time.Millisecond), humanBytes(up), humanBytes(down), target)
}

// nextConnID hands out a monotonic id used both for log correlation and as the
// live-session map key.
func (s *Server) nextConnID() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	return s.nextID
}

func (s *Server) register(id int64, lc liveConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live[id] = lc
}

func (s *Server) unregister(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.live, id)
}

func (s *Server) closeAllLive() {
	s.mu.Lock()
	drop := make([]liveConn, 0, len(s.live))
	for id, lc := range s.live {
		drop = append(drop, lc)
		delete(s.live, id)
	}
	s.mu.Unlock()
	for _, lc := range drop {
		lc.client.Close()
		lc.up.Close()
	}
}

// recordDialOK marks a successful dial and, if the role had been failing, logs a
// recovery line naming how many attempts it took — the "it started working
// again at HH:MM:SS" bookend to a failure streak.
func (s *Server) recordDialOK(id int64, role, target string, took time.Duration) {
	s.mu.Lock()
	h := s.dialHealthLocked(role)
	recovered := h.fails
	h.lastOK, h.fails, h.lastClass = time.Now(), 0, ""
	s.mu.Unlock()

	if recovered > 0 {
		s.info("conn#%d %s: dial %s OK in %s — RECOVERED after %d consecutive failure(s)",
			id, role, target, took.Round(time.Millisecond), recovered)
		return
	}
	s.debug("conn#%d %s: dial %s OK in %s", id, role, target, took.Round(time.Millisecond))
}

// recordDialFail classifies the error, updates the role's health, and logs it.
// The first failure of a streak (or a change of error class) is always logged
// loudly WITH a remediation hint; identical repeats are throttled to one line
// per 10s (with a running count) so a hammering client can't flood the file but
// the failure is never invisible.
func (s *Server) recordDialFail(id int64, role, target string, err error, took time.Duration) {
	class, hint := classifyDial(err)

	s.mu.Lock()
	h := s.dialHealthLocked(role)
	h.lastErr = time.Now()
	h.fails++
	firstOfStreak := h.fails == 1 || h.lastClass != class
	h.lastClass = class
	quiet := !firstOfStreak && time.Since(h.loggedAt) < 10*time.Second
	if !quiet {
		h.loggedAt = time.Now()
	}
	fails := h.fails
	s.mu.Unlock()

	if quiet {
		return
	}
	s.warn("conn#%d %s: dial %s FAILED after %s: %v [%s] (%d in a row)",
		id, role, target, took.Round(time.Millisecond), err, class, fails)
	if hint != "" && firstOfStreak {
		s.warn("conn#%d %s: hint: %s", id, role, hint)
	}
}

func (s *Server) dialHealthLocked(role string) *dialHealth {
	h := s.dial[role]
	if h == nil {
		h = &dialHealth{}
		s.dial[role] = h
	}
	return h
}

// classifyDial buckets an upstream dial error and returns a one-line remediation
// hint. The EHOSTUNREACH case is the star: it is the macOS Local Network Privacy
// signature (this process is denied the VM subnet even though a terminal dial to
// the same IP works, because CLI tools inherit Terminal's grant).
func classifyDial(err error) (class, hint string) {
	switch {
	case errors.Is(err, syscall.EHOSTUNREACH):
		return "EHOSTUNREACH", "macOS Local Network Privacy is almost certainly blocking THIS process from the VM subnet " +
			"(a plain `nc`/psql from a terminal can still reach it — CLI tools inherit Terminal's grant). " +
			"Grant Local Network to pgdev in System Settings → Privacy & Security, then `make endpoint.restart`."
	case errors.Is(err, syscall.ECONNREFUSED):
		return "ECONNREFUSED", "the VM is reachable but nothing is listening on the backend port — Postgres/the container is likely down on that machine (`make status`)."
	case errors.Is(err, syscall.ENETUNREACH):
		return "ENETUNREACH", "the host has no route to the VM subnet — bridge100/vmnet is down or the machine never booted (`make start`)."
	case isDialTimeout(err):
		return "timeout", "the target IP did not answer within the dial timeout — a stale IP after recreate, or a wedged VM (`make status` to re-check the IP)."
	default:
		return "other", ""
	}
}

func isDialTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// pipe copies bytes both ways and returns only once BOTH directions have ended,
// reporting the bytes moved each way (client→upstream, upstream→client) for the
// close log. Each half half-closes the peer's write side (CloseWrite) on EOF so
// a one-way shutdown propagates as a FIN; waiting for both (not the first)
// avoids truncating a reply still streaming the other way (e.g. COPY OUT).
func pipe(client, upstream net.Conn) (clientToUp, upToClient int64) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn, n *int64) {
		defer wg.Done()
		c, _ := io.Copy(dst, src)
		*n = c
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
	}
	go cp(upstream, client, &clientToUp)
	go cp(client, upstream, &upToClient)
	wg.Wait()
	return clientToUp, upToClient
}

// humanBytes renders a byte count compactly for the per-connection close line.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// keepAlive turns on TCP keepalive so a vanished machine (IP gone, no RST)
// eventually surfaces as a read error instead of leaking the goroutine pair.
func keepAlive(c net.Conn) {
	if t, ok := c.(*net.TCPConn); ok {
		_ = t.SetKeepAlive(true)
		_ = t.SetKeepAlivePeriod(30 * time.Second)
	}
}

// readIP returns the trimmed contents of an IP cache file, or "" if it is
// missing/empty (the machine is down or hasn't reported yet).
func readIP(path string) string {
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
