package track

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// open is a test helper that opens a fresh DB in a temp dir with the legacy
// mirrors wired to temp files, returning the DB and the mirror paths.
func open(t *testing.T) (*DB, string, func(string) string) {
	t.Helper()
	dir := t.TempDir()
	activeMirror := filepath.Join(dir, "active-machine")
	ipMirror := func(slot string) string { return filepath.Join(dir, "machine-ip-"+slot) }
	db, reset, err := Open(context.Background(), Options{
		Path:         filepath.Join(dir, "pgdev.db"),
		ActiveMirror: activeMirror,
		IPMirror:     ipMirror,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if reset {
		t.Fatalf("fresh open reported a reset")
	}
	t.Cleanup(func() { db.Close() })
	return db, activeMirror, ipMirror
}

func TestActiveDefaultsToA(t *testing.T) {
	db, _, _ := open(t)
	if got := db.ActiveSlot(context.Background()); got != "a" {
		t.Fatalf("fresh active = %q, want a", got)
	}
	if got := db.StagingSlot(context.Background()); got != "b" {
		t.Fatalf("staging = %q, want b", got)
	}
}

func TestSetActiveMirrorsFile(t *testing.T) {
	db, activeMirror, _ := open(t)
	ctx := context.Background()
	if err := db.SetActive(ctx, "b"); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	if got := db.ActiveSlot(ctx); got != "b" {
		t.Fatalf("active = %q, want b", got)
	}
	b, err := os.ReadFile(activeMirror)
	if err != nil {
		t.Fatalf("read mirror: %v", err)
	}
	if got := trim(string(b)); got != "b" {
		t.Fatalf("mirror file = %q, want b", got)
	}
	if err := db.SetActive(ctx, "x"); err == nil {
		t.Fatalf("SetActive(x) should reject non-a/b")
	}
}

func TestMachineIPSetGetMirror(t *testing.T) {
	db, _, ipMirror := open(t)
	ctx := context.Background()
	if got := db.MachineIP(ctx, "a"); got != "" {
		t.Fatalf("fresh ip = %q, want empty", got)
	}
	if err := db.SetMachineIP(ctx, "a", "10.0.0.5"); err != nil {
		t.Fatalf("SetMachineIP: %v", err)
	}
	if got := db.MachineIP(ctx, "a"); got != "10.0.0.5" {
		t.Fatalf("ip = %q, want 10.0.0.5", got)
	}
	b, _ := os.ReadFile(ipMirror("a"))
	if got := trim(string(b)); got != "10.0.0.5" {
		t.Fatalf("ip mirror = %q, want 10.0.0.5", got)
	}
	// An empty IP must not clobber a good cached value, and must not write a mirror.
	if err := db.SetMachineIP(ctx, "a", ""); err != nil {
		t.Fatalf("SetMachineIP empty: %v", err)
	}
	if got := db.MachineIP(ctx, "a"); got != "10.0.0.5" {
		t.Fatalf("ip after empty write = %q, want 10.0.0.5 (unchanged)", got)
	}
}

// TestSeedMachineIPsFromMirrorOnOpen verifies a fresh DB adopts the machine IPs
// from the existing legacy mirror files, so a first `proxy install` has targets
// without waiting for a refresh.
func TestSeedMachineIPsFromMirrorOnOpen(t *testing.T) {
	dir := t.TempDir()
	// Pre-existing deployment state: legacy IP files present, no DB yet.
	if err := os.WriteFile(filepath.Join(dir, "machine-ip-a"), []byte("192.168.64.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "machine-ip-b"), []byte("192.168.64.20\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	db, _, err := Open(context.Background(), Options{
		Path:     filepath.Join(dir, "pgdev.db"),
		IPMirror: func(slot string) string { return filepath.Join(dir, "machine-ip-"+slot) },
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if got := db.MachineIP(ctx, "a"); got != "192.168.64.22" {
		t.Fatalf("seeded ip a = %q, want 192.168.64.22", got)
	}
	if got := db.MachineIP(ctx, "b"); got != "192.168.64.20" {
		t.Fatalf("seeded ip b = %q, want 192.168.64.20", got)
	}
}

// TestForgetMachine verifies a purged machine's IP is cleared from BOTH the DB
// and the legacy mirror, so nothing routes at its dead address afterwards.
func TestForgetMachine(t *testing.T) {
	db, _, ipMirror := open(t)
	ctx := context.Background()
	if err := db.SetMachineIP(ctx, "b", "10.0.0.6"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ipMirror("b")); err != nil {
		t.Fatalf("mirror not written: %v", err)
	}
	if err := db.ForgetMachine(ctx, "b"); err != nil {
		t.Fatalf("ForgetMachine: %v", err)
	}
	if got := db.MachineIP(ctx, "b"); got != "" {
		t.Fatalf("ip after forget = %q, want empty", got)
	}
	if _, err := os.Stat(ipMirror("b")); !os.IsNotExist(err) {
		t.Fatalf("mirror still present after forget (err=%v)", err)
	}
	// The forgotten slot is now unroutable in DesiredTargets (active a, b down).
	got, _ := db.DesiredTargets(ctx, 5442, 5443, 5432)
	for _, tt := range got {
		if tt.Role == "staging" && tt.Target != "" {
			t.Fatalf("staging target = %q, want empty after forget", tt.Target)
		}
	}
}

func TestDesiredTargets(t *testing.T) {
	db, _, _ := open(t)
	ctx := context.Background()
	db.SetMachineIP(ctx, "a", "10.0.0.5")
	db.SetMachineIP(ctx, "b", "10.0.0.6")

	// active=a: active->a's ip:5432 on 5444, staging->b's ip:5432 on 5445.
	got, err := db.DesiredTargets(ctx, 5444, 5445, 5432)
	if err != nil {
		t.Fatalf("DesiredTargets: %v", err)
	}
	want := map[string]string{"active": "10.0.0.5:5432", "staging": "10.0.0.6:5432"}
	for _, tt := range got {
		if want[tt.Role] != tt.Target {
			t.Fatalf("role %s target = %q, want %q", tt.Role, tt.Target, want[tt.Role])
		}
	}

	// Flip active to b: the mapping swaps.
	db.SetActive(ctx, "b")
	got, _ = db.DesiredTargets(ctx, 5444, 5445, 5432)
	want = map[string]string{"active": "10.0.0.6:5432", "staging": "10.0.0.5:5432"}
	for _, tt := range got {
		if want[tt.Role] != tt.Target {
			t.Fatalf("after flip role %s target = %q, want %q", tt.Role, tt.Target, want[tt.Role])
		}
	}
}

func TestDesiredTargetUnroutableWhenIPMissing(t *testing.T) {
	db, _, _ := open(t)
	ctx := context.Background()
	db.SetMachineIP(ctx, "a", "10.0.0.5") // only a has an IP; b is down
	got, _ := db.DesiredTargets(ctx, 5444, 5445, 5432)
	byRole := map[string]Target{}
	for _, tt := range got {
		byRole[tt.Role] = tt
	}
	if byRole["active"].Target != "10.0.0.5:5432" {
		t.Fatalf("active target = %q", byRole["active"].Target)
	}
	if byRole["staging"].Target != "" {
		t.Fatalf("staging target = %q, want empty (b down)", byRole["staging"].Target)
	}
}

func TestAppliedTargetsRoundTrip(t *testing.T) {
	db, _, _ := open(t)
	ctx := context.Background()
	if err := db.RecordApplied(ctx, Target{Role: "active", Port: 5444, Target: "10.0.0.5:5432"}); err != nil {
		t.Fatalf("RecordApplied: %v", err)
	}
	m, err := db.AppliedTargets(ctx)
	if err != nil {
		t.Fatalf("AppliedTargets: %v", err)
	}
	if m["active"].Target != "10.0.0.5:5432" || m["active"].Port != 5444 {
		t.Fatalf("applied active = %+v", m["active"])
	}
}

// TestSchemaResetCarriesActive simulates a schema-version change: the tables are
// dropped and recreated, but the active slot must survive (never silently reset
// to "a").
func TestSchemaResetCarriesActive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pgdev.db")
	activeMirror := filepath.Join(dir, "active-machine")
	opts := Options{Path: path, ActiveMirror: activeMirror, IPMirror: func(s string) string { return filepath.Join(dir, "ip-"+s) }}
	ctx := context.Background()

	db, _, err := Open(ctx, opts)
	if err != nil {
		t.Fatalf("open1: %v", err)
	}
	if err := db.SetActive(ctx, "b"); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	// Force a version mismatch as a future schema bump would.
	if _, err := db.sql.ExecContext(ctx, "UPDATE schema_meta SET version = 99 WHERE id = 1"); err != nil {
		t.Fatalf("bump version: %v", err)
	}
	db.Close()

	db2, reset, err := Open(ctx, opts)
	if err != nil {
		t.Fatalf("open2: %v", err)
	}
	defer db2.Close()
	if !reset {
		t.Fatalf("expected reset=true on version mismatch")
	}
	if got := db2.ActiveSlot(ctx); got != "b" {
		t.Fatalf("active after reset = %q, want b (carried over)", got)
	}
	// IPs are expected to be gone after a reset.
	if got := db2.MachineIP(ctx, "a"); got != "" {
		t.Fatalf("ip survived reset = %q, want empty", got)
	}
}
