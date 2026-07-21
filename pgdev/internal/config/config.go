// Package config holds the single resolved configuration both binaries share.
// It mirrors the values baked into scripts/pg-dev-local and .env (§3 of
// issues/0001) so the host CLI (pgdev) and the in-machine daemon (pgdevd) agree
// on paths, container names, ports and the agent transport.
//
// Load() reads the process environment first (the Makefile does `-include .env;
// export`, so `make` already puts every var in the environment) and falls back
// to parsing <RepoRoot>/.env directly, so the binaries also work when invoked by
// hand. Required-for-host credentials are validated lazily by the caller that
// needs them, not here, so the daemon can start credential-free.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	// DefaultDataRoot is the XFS reflink store mount inside the machine.
	DefaultDataRoot = "/var/lib/pg-dev-local"
	// DefaultBackendPrefix yields container names pg-dev-a / pg-dev-b.
	DefaultBackendPrefix = "pg-dev"
	// DefaultProxyName is the bare container that owns the two proxy devices.
	DefaultProxyName = "pg-proxy"
	// PGUnit is the PostgreSQL 17 systemd unit inside each backend container.
	PGUnit = "postgresql@17-main"
)

// Config is resolved once at startup by Load. Every field has a safe default
// matching the shell; only the PG credentials are required, and only for the
// host-side rendering that uses them.
type Config struct {
	// PostgreSQL credentials — used host-side to render .pgpass/psql lines. The
	// daemon never needs them (status returns raw facts; the host formats).
	PGUser, PGDB, PGPassword string

	MachineName string // vpg — the Apple container machine

	// Machine-side proxy ports (the Incus proxy devices' listeners).
	ActivePort, StagingPort int // 5432 / 5433
	// Host loopback ports clients actually connect to.
	ClientActivePort, ClientStagingPort int    // 5442 / 5443
	ClientHost                          string // 127.0.0.1

	BackendPrefix string // pg-dev
	ProxyName     string // pg-proxy
	// BackendAIP/BackendBIP are the pinned eth0 addresses; empty means "derive
	// from incusbr0" (<prefix>.11 / .12), resolved live by the daemon.
	BackendAIP, BackendBIP string

	DataRoot     string // /var/lib/pg-dev-local
	DataDiskSize string // 140G

	// Agent transport (pgdev ↔ pgdevd).
	AgentPort      int    // 5440 — HTTP/JSON on the machine's eth0
	AgentTokenPath string // <repo>/var/agent-token (0600 bearer token)
	MachineIP      string // override for the discovered machine eth0 IP

	RepoRoot string // repo root, visible in-machine at the same path (home-mount)

	// Ownership for host-visible files the daemon writes (var/active-slot); the
	// machine runs as root, so it chowns back to the invoking macOS user.
	HostUID, HostGID string
}

// Load resolves configuration from the environment and <RepoRoot>/.env.
func Load() Config {
	repo := envOr("PG_REPO_ROOT", "")
	if repo == "" {
		if wd, err := os.Getwd(); err == nil {
			repo = wd
		}
	}
	env := mergedEnv(repo)
	get := func(k, def string) string {
		if v, ok := env[k]; ok && v != "" {
			return v
		}
		return def
	}

	c := Config{
		PGUser:            get("PG_USER", ""),
		PGDB:              get("PG_DB", ""),
		PGPassword:        get("PG_PASSWORD", ""),
		MachineName:       get("MACHINE_NAME", "vpg"),
		ActivePort:        atoi(get("PG_ACTIVE_PORT", "5432")),
		StagingPort:       atoi(get("PG_STAGING_PORT", "5433")),
		ClientActivePort:  atoi(get("PG_CLIENT_ACTIVE_PORT", "5442")),
		ClientStagingPort: atoi(get("PG_CLIENT_STAGING_PORT", "5443")),
		ClientHost:        get("PG_CLIENT_HOST", "127.0.0.1"),
		BackendPrefix:     get("PG_BACKEND_PREFIX", DefaultBackendPrefix),
		ProxyName:         get("PG_PROXY_NAME", DefaultProxyName),
		BackendAIP:        get("PG_BACKEND_A_IP", ""),
		BackendBIP:        get("PG_BACKEND_B_IP", ""),
		DataRoot:          get("PG_DATA_ROOT", DefaultDataRoot),
		DataDiskSize:      get("PG_DATA_DISK_SIZE", "140G"),
		AgentPort:         atoi(get("PG_AGENT_PORT", "5440")),
		MachineIP:         get("PG_MACHINE_IP", ""),
		RepoRoot:          repo,
		HostUID:           get("HOST_UID", ""),
		HostGID:           get("HOST_GID", ""),
	}
	c.AgentTokenPath = get("PG_AGENT_TOKEN_PATH", filepath.Join(repo, "var", "agent-token"))
	return c
}

// RequireCreds fails unless the PostgreSQL credentials are set (host-side only).
func (c Config) RequireCreds() error {
	var missing []string
	if c.PGUser == "" {
		missing = append(missing, "PG_USER")
	}
	if c.PGDB == "" {
		missing = append(missing, "PG_DB")
	}
	if c.PGPassword == "" {
		missing = append(missing, "PG_PASSWORD")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s must be set in .env", strings.Join(missing, ", "))
	}
	return nil
}

// Container returns the backend container name for a slot.
func (c Config) Container(slot string) string { return c.BackendPrefix + "-" + slot }

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

// BackendIP maps a slot to its configured override IP ("" = derive from incusbr0).
func (c Config) BackendIP(slot string) string {
	if slot == "b" {
		return c.BackendBIP
	}
	return c.BackendAIP
}

// ActiveSlotPath is the host-visible pointer file (default active slot: "a").
func (c Config) ActiveSlotPath() string { return filepath.Join(c.RepoRoot, "var", "active-slot") }

// ClientPort returns the host client port for a role ("active"/"staging").
func (c Config) ClientPort(role string) int {
	if role == "staging" {
		return c.ClientStagingPort
	}
	return c.ClientActivePort
}

// ----- helpers -------------------------------------------------------------

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

// mergedEnv layers the real environment over a best-effort parse of
// <repo>/.env, with the environment winning (as `-include .env; export` does).
func mergedEnv(repo string) map[string]string {
	m := map[string]string{}
	if repo != "" {
		if f, err := os.Open(filepath.Join(repo, ".env")); err == nil {
			defer f.Close()
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				line = strings.TrimPrefix(line, "export ")
				k, v, ok := strings.Cut(line, "=")
				if !ok {
					continue
				}
				m[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
			}
		}
	}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}
