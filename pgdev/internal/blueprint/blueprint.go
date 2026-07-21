// Package blueprint expresses the desired topology as data. Compute() returns
// the full intended state as a pure function of (config, active slot, resolved
// backend IPs); the reconciler (internal/reconcile) makes reality match it.
//
// The load-bearing collapse (§5.2 of issues/0001): the proxy devices' connect
// targets are a pure function of the active slot, so promote becomes
// "SetActive(other) then Reconcile()" — no hand-threaded rollback, no drift.
package blueprint

import (
	"fmt"

	"pansen.me/pgdev/internal/config"
)

// Backend is one PostgreSQL slot container.
type Backend struct {
	Name string // pg-dev-a
	Slot string // a | b
	IP   string // pinned eth0 (.11 / .12)
}

// ProxyDevice is one Incus proxy device on the proxy container. bind=host puts
// the listener on the Apple machine's eth0 (reachable from macOS); the connect
// target is the backend's pinned incusbr0 address.
type ProxyDevice struct {
	Name    string // "main" | "staging"
	Listen  string // tcp:0.0.0.0:<clientPort-on-machine>
	Connect string // tcp:<backendIP>:5432
}

// Proxy is the bare container that owns the two proxy devices.
type Proxy struct {
	Name    string        // pg-proxy
	Devices []ProxyDevice // main → active, staging → staging
}

// Blueprint is the complete intended topology for a given active slot.
type Blueprint struct {
	Active   string // active slot
	Backends [2]Backend
	Proxy    Proxy
}

// Device names on the proxy container (kept identical to the shell's constants).
const (
	MainDevice    = "main"
	StagingDevice = "staging"
	// backendPGPort is the PostgreSQL port inside every backend container.
	backendPGPort = 5432
)

// Compute returns the intended topology. aIP/bIP are the resolved pinned
// addresses for slots a/b (the caller resolves any incusbr0-derived defaults so
// this stays pure and testable).
func Compute(cfg config.Config, active, aIP, bIP string) Blueprint {
	staging := "b"
	if active == "b" {
		staging = "a"
	}
	ip := map[string]string{"a": aIP, "b": bIP}

	return Blueprint{
		Active: active,
		Backends: [2]Backend{
			{Name: cfg.Container("a"), Slot: "a", IP: aIP},
			{Name: cfg.Container("b"), Slot: "b", IP: bIP},
		},
		Proxy: Proxy{
			Name: cfg.ProxyName,
			Devices: []ProxyDevice{
				{
					Name:    MainDevice,
					Listen:  fmt.Sprintf("tcp:0.0.0.0:%d", cfg.ActivePort),
					Connect: fmt.Sprintf("tcp:%s:%d", ip[active], backendPGPort),
				},
				{
					Name:    StagingDevice,
					Listen:  fmt.Sprintf("tcp:0.0.0.0:%d", cfg.StagingPort),
					Connect: fmt.Sprintf("tcp:%s:%d", ip[staging], backendPGPort),
				},
			},
		},
	}
}
