// Package blueprint expresses the desired in-machine topology as data.
// Compute() returns the intended state as a pure function of (config, slot); the
// reconciler (internal/reconcile) makes reality match it.
//
// Two-machine model (spec 0002): each machine hosts exactly ONE backend and no
// separate proxy container. The backend is exposed on the machine's own eth0 by
// a single Incus proxy device attached to the backend container itself
// (listen on the machine host, connect to PostgreSQL on the container's
// loopback) — so there is no incusbr0 static-IP pinning to reason about: the
// connect target never drifts. promote is a host-side concern and does not
// appear here.
package blueprint

import (
	"fmt"

	"pansen.me/pgdev/internal/config"
)

// ForwardDevice is the name of the proxy device that exposes the backend on the
// machine's eth0. Kept stable so reconcile re-points the same device.
const ForwardDevice = "pgforward"

// Backend is this machine's single PostgreSQL container.
type Backend struct {
	Name string // pg-dev-a
	Slot string // a | b
}

// Forward is the proxy device on the backend container. bind=host puts the
// listener on the Apple machine's eth0 (reachable from macOS); connect targets
// PostgreSQL on the container's loopback.
type Forward struct {
	Device  string // pgforward
	Listen  string // tcp:0.0.0.0:<backendPort>
	Connect string // tcp:127.0.0.1:<backendPort>
}

// Blueprint is the complete intended topology for this machine's one backend.
type Blueprint struct {
	Backend Backend
	Forward Forward
}

// Compute returns the intended topology for the given slot. Pure and testable:
// no live IP resolution is needed because the forward connects to loopback.
func Compute(cfg config.Config, slot string) Blueprint {
	return Blueprint{
		Backend: Backend{Name: cfg.Container(slot), Slot: slot},
		Forward: Forward{
			Device:  ForwardDevice,
			Listen:  fmt.Sprintf("tcp:0.0.0.0:%d", cfg.BackendPort),
			Connect: fmt.Sprintf("tcp:127.0.0.1:%d", cfg.BackendPort),
		},
	}
}
