package socatproxy

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testJob(t *testing.T, target string) *job {
	t.Helper()
	dir := t.TempDir()
	return &job{
		label:          "me.pansen.test-socat-active",
		plist:          filepath.Join(dir, "job.plist"),
		role:           "active",
		bind:           "127.0.0.1",
		port:           5444,
		backend:        target,
		socat:          "/opt/homebrew/bin/socat",
		logPath:        filepath.Join(dir, "socat.log"),
		uid:            "501",
		connectTimeout: 5,
		log:            slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	}
}

func TestProgramArgs(t *testing.T) {
	j := testJob(t, "10.0.0.5:5432")
	prog := j.program()
	joined := strings.Join(prog, " ")
	for _, want := range []string{
		"/opt/homebrew/bin/socat",
		"TCP-LISTEN:5444,bind=127.0.0.1,reuseaddr,fork,keepalive,nodelay",
		"TCP:10.0.0.5:5432,connect-timeout=5,keepalive,nodelay",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("program args %q missing %q", joined, want)
		}
	}
}

// TestPlistRoundTrip writes the plist and recovers the target out of it — the
// exact path reconcile uses to decide desired-vs-current against on-disk state.
func TestPlistRoundTrip(t *testing.T) {
	j := testJob(t, "10.0.0.5:5432")
	if err := j.writePlist(); err != nil {
		t.Fatalf("writePlist: %v", err)
	}
	if !j.installed() {
		t.Fatalf("installed() false after writePlist")
	}
	if got := j.currentTarget(); got != "10.0.0.5:5432" {
		t.Fatalf("currentTarget = %q, want 10.0.0.5:5432", got)
	}
}

func TestCurrentTargetAbsent(t *testing.T) {
	j := testJob(t, "10.0.0.5:5432")
	if got := j.currentTarget(); got != "" {
		t.Fatalf("currentTarget with no plist = %q, want empty", got)
	}
}

// TestProbeFree checks the port-free gate reflects reality: free when nothing
// listens, not-free when we hold the port.
func TestProbeFree(t *testing.T) {
	j := testJob(t, "10.0.0.5:5432")
	// Pick an ephemeral free port to avoid clashing with a real 5444.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	j.port = port

	if j.probeFree() {
		t.Fatalf("probeFree = true while a listener is held on %d", port)
	}
	ln.Close()
	// Give the OS a moment to release, then it should read free.
	deadline := time.Now().Add(2 * time.Second)
	for !j.probeFree() {
		if time.Now().After(deadline) {
			t.Fatalf("probeFree still false after closing listener on %d", port)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestReconcilerAnyInstalled(t *testing.T) {
	// Point HOME at a temp dir so job plist paths resolve there.
	home := t.TempDir()
	t.Setenv("HOME", home)
	r := &Reconciler{Prefix: "test", Bind: "127.0.0.1", Socat: "/opt/homebrew/bin/socat",
		LockPath: filepath.Join(home, "reconcile.flock")}
	if r.AnyInstalled() {
		t.Fatalf("AnyInstalled true with no plists")
	}
	// Create one plist where job() expects it.
	la := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(la, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(la, "me.pansen.test-socat-active.plist"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write plist: %v", err)
	}
	if !r.AnyInstalled() {
		t.Fatalf("AnyInstalled false after creating a plist")
	}
}

// TestLockSerializes checks the flock mutex actually excludes a second holder.
func TestLockSerializes(t *testing.T) {
	dir := t.TempDir()
	r := &Reconciler{LockPath: filepath.Join(dir, "reconcile.flock")}
	unlock, err := r.lock(context.Background())
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	// A second lock attempt must time out (bounded quickly for the test).
	r2 := &Reconciler{LockPath: r.LockPath}
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	if _, err := r2.lock(ctx); err == nil {
		t.Fatalf("second lock succeeded while first held")
	}
	unlock()
	// After release, a lock should succeed.
	unlock2, err := r2.lock(context.Background())
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	unlock2()
}

// TestReconcileFetchUnderLock verifies targets are fetched AFTER the lock (a
// fetch error aborts cleanly) and that the record callback never fires when
// fetch fails — i.e. the stale-snapshot race fix is wired up.
func TestReconcileFetchUnderLock(t *testing.T) {
	dir := t.TempDir()
	r := &Reconciler{Prefix: "test", Bind: "127.0.0.1", Socat: "/opt/homebrew/bin/socat",
		LockPath: filepath.Join(dir, "reconcile.flock"),
		Log:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))}

	fetched := false
	recorded := false
	_, err := r.Reconcile(context.Background(),
		func(context.Context) ([]Target, error) {
			fetched = true
			return nil, errFetch
		},
		func(Target) error { recorded = true; return nil })
	if err == nil {
		t.Fatalf("Reconcile should surface a fetch error")
	}
	if !fetched {
		t.Fatalf("fetch was not called")
	}
	if recorded {
		t.Fatalf("record must not fire when fetch fails")
	}
}

var errFetch = errTest("boom")

type errTest string

func (e errTest) Error() string { return string(e) }

func TestListenArgUsesPort(t *testing.T) {
	j := testJob(t, "1.2.3.4:5432")
	j.port = 5445
	if !strings.Contains(j.listenArg(), "TCP-LISTEN:5445") {
		t.Fatalf("listenArg = %q", j.listenArg())
	}
}
