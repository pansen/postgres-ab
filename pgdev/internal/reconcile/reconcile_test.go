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
	setDevices []string // "dev=connect"
	setErr     error
	stateErr   error
}

func (f *fakeIncus) State(_ context.Context, c string) (string, error) {
	return f.states[c], f.stateErr
}
func (f *fakeIncus) SetProxyDevice(_ context.Context, _, dev, _, connect string) error {
	f.setDevices = append(f.setDevices, dev+"="+connect)
	return f.setErr
}

func bp() blueprint.Blueprint {
	cfg := config.Config{BackendPrefix: "pg-dev", BackendPort: 5432}
	return blueprint.Compute(cfg, "a")
}

func TestReconcileSetsForwardWhenBackendRunning(t *testing.T) {
	f := &fakeIncus{states: map[string]string{"pg-dev-a": "RUNNING"}}
	res, err := Reconcile(context.Background(), f, bp(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.BackendRunning {
		t.Fatal("BackendRunning = false")
	}
	if len(f.setDevices) != 1 || f.setDevices[0] != "pgforward=tcp:127.0.0.1:5432" {
		t.Fatalf("setDevices = %v, want the one forward", f.setDevices)
	}
}

// A no-op when the backend isn't running — there is nothing to forward to.
func TestReconcileSkipsForwardWhenBackendDown(t *testing.T) {
	f := &fakeIncus{states: map[string]string{"pg-dev-a": "STOPPED"}}
	res, err := Reconcile(context.Background(), f, bp(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.BackendRunning {
		t.Fatal("BackendRunning = true for a stopped backend")
	}
	if len(f.setDevices) != 0 {
		t.Fatalf("set %v forward devices on a stopped backend", f.setDevices)
	}
}

func TestReconcileReportsSetError(t *testing.T) {
	f := &fakeIncus{states: map[string]string{"pg-dev-a": "RUNNING"}, setErr: errors.New("boom")}
	if _, err := Reconcile(context.Background(), f, bp(), nil); err == nil {
		t.Fatal("expected error from SetProxyDevice failure")
	}
}

func TestReconcileReportsStateError(t *testing.T) {
	f := &fakeIncus{stateErr: errors.New("no incus")}
	if _, err := Reconcile(context.Background(), f, bp(), nil); err == nil {
		t.Fatal("expected error when backend state can't be read")
	}
}
