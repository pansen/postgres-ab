package forward

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// writeFile is a tiny helper for the var/ state files the Server watches.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newTestServer wires a Server over a temp dir's state files, returning it plus
// the dir so tests can rewrite the pointer/IP files under it.
func newTestServer(t *testing.T, o Options) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	o.ActiveMachinePath = filepath.Join(dir, "active-machine")
	o.MachineIPPath = func(slot string) string { return filepath.Join(dir, "machine-ip-"+slot) }
	return New(o), dir
}

// TestReloadSwapsOnPointerFlip drives the file-watch computation end to end: a
// rewrite of the pointer file must swap both role targets on the next reload,
// with the listeners never involved.
func TestReloadSwapsOnPointerFlip(t *testing.T) {
	s, dir := newTestServer(t, Options{BackendPort: 5432})
	writeFile(t, filepath.Join(dir, "machine-ip-a"), "10.0.0.1\n")
	writeFile(t, filepath.Join(dir, "machine-ip-b"), "10.0.0.2\n")
	writeFile(t, s.opts.ActiveMachinePath, "a\n")

	s.reload()
	if got := s.target("active"); got != "10.0.0.1:5432" {
		t.Fatalf("active = %q, want 10.0.0.1:5432", got)
	}
	if got := s.target("staging"); got != "10.0.0.2:5432" {
		t.Fatalf("staging = %q, want 10.0.0.2:5432", got)
	}

	writeFile(t, s.opts.ActiveMachinePath, "b\n")
	s.reload()
	if got := s.target("active"); got != "10.0.0.2:5432" {
		t.Fatalf("after flip active = %q, want 10.0.0.2:5432", got)
	}
	if got := s.target("staging"); got != "10.0.0.1:5432" {
		t.Fatalf("after flip staging = %q, want 10.0.0.1:5432", got)
	}
}

// TestReloadDropsStaleSessionsOnRepoint verifies the drop-on-repoint safety
// property: an established session on a role whose target changes (a promote)
// is torn down so the client can't keep talking to the demoted database, while
// a session on an unchanged role is left alone.
func TestReloadDropsStaleSessionsOnRepoint(t *testing.T) {
	s, dir := newTestServer(t, Options{BackendPort: 5432})
	writeFile(t, filepath.Join(dir, "machine-ip-a"), "10.0.0.1\n")
	writeFile(t, filepath.Join(dir, "machine-ip-b"), "10.0.0.2\n")
	writeFile(t, s.opts.ActiveMachinePath, "a\n")
	s.reload()

	// Register a fake session on each role at the current target.
	activeCli, activeUp := net.Pipe()
	stagingCli, stagingUp := net.Pipe()
	s.register(s.nextConnID(), liveConn{role: "active", target: s.target("active"), client: activeCli, up: activeUp})
	s.register(s.nextConnID(), liveConn{role: "staging", target: s.target("staging"), client: stagingCli, up: stagingUp})

	// Promote: flip the pointer. Both role targets change (a<->b swap), so BOTH
	// sessions are stale and must be dropped.
	writeFile(t, s.opts.ActiveMachinePath, "b\n")
	s.reload()

	if !isClosed(activeCli) {
		t.Error("active session was not dropped after promote")
	}
	if !isClosed(stagingCli) {
		t.Error("staging session was not dropped after promote")
	}
	if n := s.liveCount(); n != 0 {
		t.Errorf("live sessions after repoint = %d, want 0", n)
	}
}

// TestReloadKeepsUnaffectedSession leaves a role's target unchanged (a staging
// IP refresh that doesn't alter the active mapping) and asserts the active
// session survives.
func TestReloadKeepsUnaffectedSession(t *testing.T) {
	s, dir := newTestServer(t, Options{BackendPort: 5432})
	writeFile(t, filepath.Join(dir, "machine-ip-a"), "10.0.0.1\n")
	writeFile(t, filepath.Join(dir, "machine-ip-b"), "10.0.0.2\n")
	writeFile(t, s.opts.ActiveMachinePath, "a\n")
	s.reload()

	cli, up := net.Pipe()
	s.register(s.nextConnID(), liveConn{role: "active", target: s.target("active"), client: cli, up: up})

	// Only staging's IP changes; active target is untouched.
	writeFile(t, filepath.Join(dir, "machine-ip-b"), "10.0.0.99\n")
	s.reload()

	if isClosed(cli) {
		t.Error("active session was dropped despite its target being unchanged")
	}
}

func (s *Server) liveCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.live)
}

// isClosed reports whether a net.Pipe end has been closed, via a short read.
func isClosed(c net.Conn) bool {
	c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, err := c.Read(make([]byte, 1))
	return err != nil && err != os.ErrDeadlineExceeded && !isTimeout(err)
}

func isTimeout(err error) bool {
	te, ok := err.(interface{ Timeout() bool })
	return ok && te.Timeout()
}

// freePort returns a currently-free localhost TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// echoServer accepts one-shot connections and echoes bytes back until EOF.
func echoServer(t *testing.T) (addrPort int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestServeProxiesActiveAndClosesUnroutableStaging brings the real Server up on
// loopback and checks: (1) the active port relays to the backend; (2) the
// staging port, whose machine has no cached IP, accepts then immediately closes.
func TestServeProxiesActiveAndClosesUnroutableStaging(t *testing.T) {
	backend := echoServer(t)
	activePort, stagingPort := freePort(t), freePort(t)

	s, dir := newTestServer(t, Options{
		Bind:         "127.0.0.1",
		ActivePort:   activePort,
		StagingPort:  stagingPort,
		BackendPort:  backend,
		PollInterval: 20 * time.Millisecond,
	})
	writeFile(t, filepath.Join(dir, "machine-ip-a"), "127.0.0.1\n") // active reachable
	// machine-ip-b intentionally absent -> staging target empty
	writeFile(t, s.opts.ActiveMachinePath, "a\n")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// The active port relays to the echo backend.
	activeConn := dialWithRetry(t, activePort)
	defer activeConn.Close()
	if _, err := activeConn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	activeConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(activeConn, buf); err != nil {
		t.Fatalf("active read: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("active echo = %q, want ping", buf)
	}

	// The staging port has no target: the conn is accepted then closed (read
	// returns EOF with no data).
	stagingConn := dialWithRetry(t, stagingPort)
	defer stagingConn.Close()
	stagingConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, err := stagingConn.Read(make([]byte, 1)); err == nil && n > 0 {
		t.Fatalf("staging returned data (%d bytes); want immediate close", n)
	}
}

func dialWithRetry(t *testing.T, port int) net.Conn {
	t.Helper()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("dial %s: %v", addr, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
