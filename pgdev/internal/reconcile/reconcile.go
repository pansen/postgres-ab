// Package reconcile makes live Incus state match a blueprint. It is
// level-triggered and idempotent, safe to run on every status/start.
//
// Two-machine model (spec 0002): with one backend per machine and no proxy
// container or IP pinning, reconcile collapses to a single job — when the
// backend is running, (re)assert the one proxy device that exposes it on the
// machine's eth0. There is nothing to repair when it is stopped.
package reconcile

import (
	"context"
	"errors"
	"fmt"

	"pansen.me/pgdev/internal/blueprint"
	"pansen.me/pgdev/internal/logx"
)

// Incus is the control-plane surface the reconciler needs; *backend.Incus
// satisfies it.
type Incus interface {
	State(ctx context.Context, container string) (string, error) // "" if absent
	SetProxyDevice(ctx context.Context, container, dev, listen, connect string) error
}

// Result reports what the reconciler asserted, for the /v1/reconcile response.
type Result struct {
	BackendRunning bool     `json:"backendRunning"`
	Actions        []string `json:"actions"`
}

// Reconcile asserts the backend's eth0 forward device when the backend is
// running (a no-op otherwise).
func Reconcile(ctx context.Context, ic Incus, bp blueprint.Blueprint, log logx.Func) (Result, error) {
	l := logx.Or(log)
	var res Result

	state, err := ic.State(ctx, bp.Backend.Name)
	if err != nil {
		return res, fmt.Errorf("backend state: %w", err)
	}
	res.BackendRunning = state == "RUNNING"
	if !res.BackendRunning {
		l("backend %s is %s; leaving the forward untouched", bp.Backend.Name, orAbsent(state))
		return res, nil
	}

	f := bp.Forward
	if err := ic.SetProxyDevice(ctx, bp.Backend.Name, f.Device, f.Listen, f.Connect); err != nil {
		return res, errors.Join(fmt.Errorf("set forward device %s: %w", f.Device, err))
	}
	res.Actions = append(res.Actions, fmt.Sprintf("%s → %s", f.Device, f.Connect))
	return res, nil
}

func orAbsent(state string) string {
	if state == "" {
		return "ABSENT"
	}
	return state
}
