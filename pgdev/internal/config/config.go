// Package config holds the small set of constants and environment overrides the
// in-machine binary needs. It mirrors the values baked into scripts/pg-dev-local
// (BACKEND_PREFIX, DATA_ROOT, the PostgreSQL systemd unit) so both agree on paths
// and container names during the strangler migration.
package config

import (
	"fmt"
	"os"
)

const (
	// DefaultDataRoot is the XFS reflink store mount inside the machine.
	DefaultDataRoot = "/var/lib/pg-dev-local"
	// DefaultBackendPrefix yields container names pg-dev-a / pg-dev-b.
	DefaultBackendPrefix = "pg-dev"
	// PGUnit is the PostgreSQL 17 systemd unit inside each backend container.
	PGUnit = "postgresql@17-main"
)

// Config is resolved once at startup. Only DataRoot and BackendPrefix are
// configurable today; both have safe defaults matching the shell.
type Config struct {
	DataRoot      string
	BackendPrefix string
	// BackendIP maps a slot ("a"/"b") to its pinned nested-bridge address. Empty
	// when not overridden, in which case the backend derives it from incusbr0.
	BackendIP map[string]string
}

// FromEnv reads the same environment the shell exports, applying defaults.
func FromEnv() Config {
	c := Config{
		DataRoot:      envOr("PG_DATA_ROOT", DefaultDataRoot),
		BackendPrefix: envOr("PG_BACKEND_PREFIX", DefaultBackendPrefix),
		BackendIP:     map[string]string{},
	}
	if v := os.Getenv("PG_BACKEND_A_IP"); v != "" {
		c.BackendIP["a"] = v
	}
	if v := os.Getenv("PG_BACKEND_B_IP"); v != "" {
		c.BackendIP["b"] = v
	}
	return c
}

// Container returns the backend container name for a slot.
func (c Config) Container(slot string) string {
	return c.BackendPrefix + "-" + slot
}

// SlotForContainer is the inverse of Container.
func (c Config) SlotForContainer(container string) (string, error) {
	switch container {
	case c.Container("a"):
		return "a", nil
	case c.Container("b"):
		return "b", nil
	default:
		return "", fmt.Errorf("unknown backend container %q", container)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
