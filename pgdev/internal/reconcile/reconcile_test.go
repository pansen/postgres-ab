package reconcile

import (
	"context"
	"errors"
	"testing"

	"pansen.me/pgdev/internal/blueprint"
	"pansen.me/pgdev/internal/config"
)

// fakeIncus records calls and returns scripted states/errors.
type fakeIncus struct {
	states     map[string]string
	exists     map[string]bool
	repaired   []string
	setDevices []string // "dev=connect"
	repairErr  error
	setErr     error
	stateErr   error
}

func (f *fakeIncus) State(_ context.Context, c string) (string, error) {
	return f.states[c], f.stateErr
}
func (f *fakeIncus) Exists(_ context.Context, c string) bool { return f.exists[c] }
func (f *fakeIncus) RepairIP(_ context.Context, c string) error {
	f.repaired = append(f.repaired, c)
	return f.repairErr
}
func (f *fakeIncus) SetProxyDevice(_ context.Context, _, dev, _, connect string) error {
	f.setDevices = append(f.setDevices, dev+"="+connect)
	return f.setErr
}

func bp() blueprint.Blueprint {
	cfg := config.Config{BackendPrefix: "pg-dev", ProxyName: "pg-proxy", ActivePort: 5432, StagingPort: 5433}
	return blueprint.Compute(cfg, "a", "10.0.0.11", "10.0.0.12")
}

func TestReconcileSetsForwardsWhenProxyRunning(t *testing.T) {
	f := &fakeIncus{
		states: map[string]string{"pg-proxy": "RUNNING"},
		exists: map[string]bool{"pg-dev-a": true, "pg-dev-b": true},
	}
	res, err := Reconcile(context.Background(), f, bp(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.ProxyRunning {
		t.Fatal("ProxyRunning = false")
	}
	if len(f.repaired) != 2 {
		t.Fatalf("repaired = %v, want both backends", f.repaired)
	}
	if len(f.setDevices) != 2 {
		t.Fatalf("setDevices = %v, want main+staging", f.setDevices)
	}
}

// A no-op when the proxy isn't running — exactly like the shell's _set_forwards.
func TestReconcileSkipsForwardsWhenProxyDown(t *testing.T) {
	f := &fakeIncus{
		states: map[string]string{"pg-proxy": "STOPPED"},
		exists: map[string]bool{"pg-dev-a": true, "pg-dev-b": true},
	}
	res, err := Reconcile(context.Background(), f, bp(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.ProxyRunning {
		t.Fatal("ProxyRunning = true for a stopped proxy")
	}
	if len(f.setDevices) != 0 {
		t.Fatalf("set %v proxy devices on a stopped proxy", f.setDevices)
	}
	if len(f.repaired) != 2 {
		t.Fatalf("repaired = %v; IP repair should still run", f.repaired)
	}
}

// Unprovisioned backends are skipped (no RepairIP), mirroring _repair_backend_ip's
// `incus info … || return 0`.
func TestReconcileSkipsUnprovisionedBackends(t *testing.T) {
	f := &fakeIncus{
		states: map[string]string{"pg-proxy": ""},
		exists: map[string]bool{"pg-dev-a": true}, // b not provisioned
	}
	if _, err := Reconcile(context.Background(), f, bp(), nil); err != nil {
		t.Fatal(err)
	}
	if len(f.repaired) != 1 || f.repaired[0] != "pg-dev-a" {
		t.Fatalf("repaired = %v, want only pg-dev-a", f.repaired)
	}
}

func TestReconcileAggregatesErrors(t *testing.T) {
	f := &fakeIncus{
		states:    map[string]string{"pg-proxy": "RUNNING"},
		exists:    map[string]bool{"pg-dev-a": true, "pg-dev-b": true},
		setErr:    errors.New("boom"),
		repairErr: errors.New("bad ip"),
	}
	if _, err := Reconcile(context.Background(), f, bp(), nil); err == nil {
		t.Fatal("expected aggregated error")
	}
}
