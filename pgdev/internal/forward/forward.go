package forward

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
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

	mu          sync.RWMutex
	targets     map[string]string  // role -> "ip:port" ("" = unroutable)
	live        map[int64]liveConn // established sessions, for drop-on-repoint
	nextID      int64
	lastDialLog map[string]time.Time // per-role dial-failure log throttle
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
		opts:        o,
		log:         logx.Or(o.Log),
		targets:     map[string]string{},
		live:        map[int64]liveConn{},
		lastDialLog: map[string]time.Time{},
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
	s.reload() // seed the table before accepting, so the first conn already routes

	roles := []role{{"active", s.opts.ActivePort}, {"staging", s.opts.StagingPort}}
	lns := make([]net.Listener, 0, len(roles))
	lc := net.ListenConfig{}
	for _, r := range roles {
		addr := net.JoinHostPort(s.opts.Bind, strconv.Itoa(r.port))
		ln, err := lc.Listen(ctx, "tcp", addr)
		if err != nil {
			for _, done := range lns {
				done.Close()
			}
			return fmt.Errorf("listen %s (%s): %w", addr, r.name, err)
		}
		s.log("listening on %s (%s)", addr, r.name)
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

	<-ctx.Done()
	for _, ln := range lns {
		ln.Close() // unblocks the accept loops
	}
	s.closeAllLive() // drop established sessions so shutdown is prompt (ExitTimeOut)
	wg.Wait()
	return nil
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
		s.log("routing updated: active -> %s, staging -> %s (dropped %d stale session(s))",
			orNone(active), orNone(staging), len(drop))
	}
	s.writeState(slot, active, staging)
}

func (s *Server) target(role string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.targets[role]
}

// acceptLoop accepts on ln until ctx is done or the listener is closed; each
// accepted conn is handled on its own goroutine so a slow dial never stalls the
// listener.
func (s *Server) acceptLoop(ctx context.Context, ln net.Listener, role string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return // shutdown: expected error from Close
			default:
				s.log("accept on %s: %v", role, err)
				return
			}
		}
		go s.handle(conn, role)
	}
}

// handle relays one client connection to the current target for its role. The
// target is snapshotted at accept time and the session registered so a later
// re-point can drop it (see reload).
func (s *Server) handle(client net.Conn, role string) {
	defer client.Close()
	target := s.target(role)
	if target == "" {
		s.log("%s: no target (machine down?); closing %s", role, client.RemoteAddr())
		return
	}
	upstream, err := net.DialTimeout("tcp", target, s.opts.DialTimeout)
	if err != nil {
		s.logDialFailure(role, target, err)
		return
	}
	defer upstream.Close()
	keepAlive(client)
	keepAlive(upstream)

	id := s.register(liveConn{role: role, target: target, client: client, up: upstream})
	defer s.unregister(id)
	pipe(client, upstream)
}

func (s *Server) register(lc liveConn) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := s.nextID
	s.live[id] = lc
	return id
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

// logDialFailure rate-limits per-role dial errors: a down machine leaves a stale
// IP file, so a retrying client would otherwise flood the log every reconnect.
func (s *Server) logDialFailure(role, target string, err error) {
	s.mu.Lock()
	now := time.Now()
	quiet := now.Sub(s.lastDialLog[role]) < 30*time.Second
	if !quiet {
		s.lastDialLog[role] = now
	}
	s.mu.Unlock()
	if !quiet {
		s.log("%s: dial %s: %v", role, target, err)
	}
}

// pipe copies bytes both ways and returns only once BOTH directions have ended.
// Each half half-closes the peer's write side (CloseWrite) on EOF so a one-way
// shutdown propagates as a FIN; waiting for both (not the first) avoids
// truncating a reply still streaming the other way (e.g. COPY OUT).
func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
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
