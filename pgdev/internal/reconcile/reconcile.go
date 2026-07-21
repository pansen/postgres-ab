// Package reconcile makes live Incus state match a blueprint. It is
// level-triggered and idempotent, safe to run on every status/promote/start.
//
// It replaces four partial reconcilers from scripts/pg-dev-local — _set_forwards,
// _ensure_proxy_device, _repair_backend_ip and cmd_refresh — and, because
// promote is now "flip the pointer, then Reconcile()", it also subsumes promote's
// hand-rolled rollback (§5.4 of issues/0001).
package reconcile

import (
	"context"
	"errors"
	"fmt"

	"pansen.me/pgdev/internal/blueprint"
	"pansen.me/pgdev/internal/logx"
)

// Incus is the control-plane surface the reconciler needs; *backend.Incus
// satisfies it (and, per Option Zero, a future non-Incus backend can too).
type Incus interface {
	State(ctx context.Context, container string) (string, error) // "" if absent
	Exists(ctx context.Context, container string) bool
	RepairIP(ctx context.Context, container string) error
	SetProxyDevice(ctx context.Context, proxy, dev, listen, connect string) error
}

// Result reports what the reconciler asserted, for the /v1/reconcile response.
type Result struct {
	ProxyRunning bool     `json:"proxyRunning"`
	Actions      []string `json:"actions"`
}

// Reconcile repairs backend IP pins, then re-asserts the two proxy devices when
// the proxy is running (a no-op otherwise, exactly like _set_forwards).
func Reconcile(ctx context.Context, ic Incus, bp blueprint.Blueprint, log logx.Func) (Result, error) {
	l := logx.Or(log)
	var res Result
	var errs []error

	for _, b := range bp.Backends {
		if !ic.Exists(ctx, b.Name) {
			continue // not provisioned yet; nothing to repair
		}
		if err := ic.RepairIP(ctx, b.Name); err != nil {
			errs = append(errs, fmt.Errorf("repair %s IP: %w", b.Name, err))
		}
	}

	state, err := ic.State(ctx, bp.Proxy.Name)
	if err != nil {
		errs = append(errs, fmt.Errorf("proxy state: %w", err))
		return res, errors.Join(errs...)
	}
	res.ProxyRunning = state == "RUNNING"
	if !res.ProxyRunning {
		l("proxy %s is %s; leaving forwards untouched", bp.Proxy.Name, orAbsent(state))
		return res, errors.Join(errs...)
	}

	for _, d := range bp.Proxy.Devices {
		if err := ic.SetProxyDevice(ctx, bp.Proxy.Name, d.Name, d.Listen, d.Connect); err != nil {
			errs = append(errs, fmt.Errorf("set proxy device %s: %w", d.Name, err))
			continue
		}
		res.Actions = append(res.Actions, fmt.Sprintf("%s → %s", d.Name, d.Connect))
	}
	return res, errors.Join(errs...)
}

func orAbsent(state string) string {
	if state == "" {
		return "ABSENT"
	}
	return state
}
