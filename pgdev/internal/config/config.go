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

	MachineName string // vpg — the legacy single Apple container machine

	// ----- two-machine model (spec 0002) -----
	// MachinePrefix yields the per-slot machine names vpg-a / vpg-b, each an
	// Apple machine hosting exactly one backend. Slot is which of those THIS
	// process serves: set (a/b) in a daemon's pgdevd.env, empty on the host.
	MachinePrefix string // vpg
	Slot          string // "" host-side; "a"/"b" inside a machine's pgdevd
	// BackendPort is the single port each machine exposes its one backend on,
	// over its own eth0 (no in-machine proxy). The host forwarder maps the
	// client ports to <active-machine-ip>:BackendPort / <staging>:BackendPort.
	BackendPort int // 5432
	// Per-machine resources for host-orchestrated create/hard-reset. Two
	// machines share the Mac, so the default memory is smaller than the old 12G.
	MachineCPUs   int    // 4
	MachineMemory string // 8G
	MachineImage  string // local/pg-incus-machine:26.04

	// Machine-side proxy ports (the Incus proxy devices' listeners). LEGACY —
	// retired with the in-machine pg-proxy once routing moves host-side.
	ActivePort, StagingPort int // 5432 / 5433
	// Host loopback ports clients actually connect to.
	ClientActivePort, ClientStagingPort int // 5442 / 5443
	// ProxyHostname is the host printed in psql/.pgpass lines (PG_PROXY_HOSTNAME).
	// Defaults to host.docker.internal so the endpoint is reachable both from the
	// Mac and from sibling containers/k3d; 127.0.0.1 also works host-only.
	ProxyHostname string
	// ForwardBind is the address the host forwarder (internal/forward) binds its
	// client listeners to (PG_FORWARD_BIND). Default 127.0.0.1 (loopback only, as
	// socat did); set to 0.0.0.0 to reach the endpoints from sibling
	// containers/k3d that can't hit the Mac's loopback — this exposes the
	// dev-credentialed backend to every interface, so widen deliberately.
	ForwardBind string
	// ForwardVerbose enables the forwarder's per-poll DBG trace (PG_FORWARD_DEBUG=1)
	// on top of its always-on lifecycle logging. Read from .env like the rest so
	// the LaunchAgent (which loads config via PG_REPO_ROOT/.env, not the process
	// env) actually honors it.
	ForwardVerbose bool

	BackendPrefix string // pg-dev
	ProxyName     string // pg-proxy — LEGACY (single-machine in-machine proxy)
	// BackendAIP/BackendBIP are the pinned eth0 addresses; empty means "derive
	// from incusbr0" (<prefix>.11 / .12), resolved live by the daemon.
	BackendAIP, BackendBIP string

	DataRoot     string // /var/lib/pg-dev-local
	DataDiskSize string // 140G
	DataImage    string // /var/lib/pg-dev-local.xfs (loop-backed store)

	BaseImage   string // images:ubuntu/24.04/cloud — provisioning base
	GoldenImage string // pg-dev-base — published PG-installed image (Slice 3)

	// Agent transport (pgdev ↔ pgdevd).
	AgentPort      int    // 5440 — HTTP/JSON on the machine's eth0
	AgentTokenPath string // <repo>/var/agent-token (0600) — HOST-side secret store
	// AgentToken is the token VALUE (PG_AGENT_TOKEN). The daemon reads ONLY this,
	// delivered in its machine-local env file, never the home-mounted token file:
	// that virtiofs cache can serve a stale empty read to the long-running daemon
	// right after boot (spec 0002 deploy note). Host-side, the token comes from
	// AgentTokenPath via EnsureToken.
	AgentToken string
	MachineIP  string // override for the discovered machine eth0 IP

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
		MachinePrefix:     get("MACHINE_PREFIX", get("MACHINE_NAME", "vpg")),
		Slot:              get("PG_SLOT", ""),
		BackendPort:       atoi(get("PG_BACKEND_PORT", "5432")),
		MachineCPUs:       atoi(get("MACHINE_CPUS", "4")),
		MachineMemory:     get("MACHINE_MEMORY", "8G"),
		MachineImage:      get("MACHINE_IMAGE", "local/pg-incus-machine:26.04"),
		ActivePort:        atoi(get("PG_ACTIVE_PORT", "5432")),
		StagingPort:       atoi(get("PG_STAGING_PORT", "5433")),
		ClientActivePort:  atoi(get("PG_CLIENT_ACTIVE_PORT", "5442")),
		ClientStagingPort: atoi(get("PG_CLIENT_STAGING_PORT", "5443")),
		ProxyHostname:     get("PG_PROXY_HOSTNAME", "host.docker.internal"),
		ForwardBind:       get("PG_FORWARD_BIND", "127.0.0.1"),
		ForwardVerbose:    get("PG_FORWARD_DEBUG", "") == "1",
		BackendPrefix:     get("PG_BACKEND_PREFIX", DefaultBackendPrefix),
		ProxyName:         get("PG_PROXY_NAME", DefaultProxyName),
		BackendAIP:        get("PG_BACKEND_A_IP", ""),
		BackendBIP:        get("PG_BACKEND_B_IP", ""),
		DataRoot:          get("PG_DATA_ROOT", DefaultDataRoot),
		DataDiskSize:      get("PG_DATA_DISK_SIZE", "140G"),
		DataImage:         get("PG_DATA_IMAGE", DefaultDataRoot+".xfs"),
		BaseImage:         get("PG_BASE_IMAGE", "images:ubuntu/24.04/cloud"),
		GoldenImage:       get("PG_GOLDEN_IMAGE", "pg-dev-base"),
		AgentPort:         atoi(get("PG_AGENT_PORT", "5440")),
		AgentToken:        get("PG_AGENT_TOKEN", ""),
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

// ----- two-machine helpers (spec 0002) -------------------------------------

// MachineNameForSlot maps a slot ("a"/"b") to its Apple machine name, e.g.
// vpg-a / vpg-b. Each machine hosts exactly one backend.
func (c Config) MachineNameForSlot(slot string) string { return c.MachinePrefix + "-" + slot }

// ActiveMachinePath is the host-side pointer to which machine is active (behind
// the :5442 client port). Holds "a" or "b"; the other machine is staging.
func (c Config) ActiveMachinePath() string {
	return filepath.Join(c.RepoRoot, "var", "active-machine")
}

// MachineIPPath is the host-side cache of a machine's drifting eth0 IP, one file
// per machine (var/machine-ip-a, var/machine-ip-b), since the two leases drift
// independently and the forwarder must track both.
func (c Config) MachineIPPath(slot string) string {
	return filepath.Join(c.RepoRoot, "var", "machine-ip-"+slot)
}

// ForwardStatePath is where a running `pgdev forward serve` writes its live
// mapping + heartbeat (internal/forward.State), read by promote/status.
func (c Config) ForwardStatePath() string {
	return filepath.Join(c.RepoRoot, "var", "forward-state.json")
}

// ForwardLogPath is the LaunchAgent's combined stdout/stderr log.
func (c Config) ForwardLogPath() string {
	return filepath.Join(c.RepoRoot, "var", c.MachinePrefix+"-forward.log")
}

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
