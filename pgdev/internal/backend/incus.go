package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"pansen.me/pgdev/internal/config"
	"pansen.me/pgdev/internal/logx"
)

var ipv4re = regexp.MustCompile(`\d+\.\d+\.\d+\.\d+`)

// Incus drives backends by shelling out to the `incus` CLI over the machine's
// local Incus socket. Slice 1 keeps the shell transport (it matches the current
// script exactly); a later slice replaces this file with the typed Incus Go
// client behind the same Backend interface.
type Incus struct {
	Cfg        config.Config
	Unit       string        // PostgreSQL systemd unit
	ReadyTries int           // pg_isready attempts
	ReadyEvery time.Duration // delay between attempts
	Log        logx.Func     // progress logging (nil = silent)

	// runFn overrides the `incus` invocation in tests (nil = shell out for real).
	runFn func(ctx context.Context, args ...string) error
}

func (i *Incus) log(format string, args ...any) { logx.Or(i.Log)(format, args...) }

// NewIncus returns an Incus backend with defaults matching the shell.
func NewIncus(cfg config.Config) *Incus {
	return &Incus{Cfg: cfg, Unit: config.PGUnit, ReadyTries: 30, ReadyEvery: time.Second}
}

func (i *Incus) ContainerRunning(ctx context.Context, c string) (bool, error) {
	out, err := i.output(ctx, "list", c, "--format", "csv", "-c", "s")
	if err != nil {
		return false, err
	}
	return strings.EqualFold(strings.TrimSpace(out), "RUNNING"), nil
}

func (i *Incus) PGActive(ctx context.Context, c string) (bool, error) {
	err := i.run(ctx, "exec", c, "--", "systemctl", "is-active", "--quiet", i.Unit)
	if err == nil {
		return true, nil
	}
	// `systemctl is-active` exits non-zero (3) when the unit is simply stopped —
	// that is the "not active" answer, not a transport failure. run() wraps the
	// error, so unwrap with errors.As to reach the *exec.ExitError.
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return false, nil
	}
	return false, err
}

func (i *Incus) StopPG(ctx context.Context, c string) error {
	return i.run(ctx, "exec", c, "--", "systemctl", "stop", i.Unit)
}

func (i *Incus) EnsurePGRunning(ctx context.Context, c string) error {
	running, err := i.ContainerRunning(ctx, c)
	if err != nil {
		return err
	}
	if !running {
		return fmt.Errorf("container %s is not running; cannot ensure PostgreSQL", c)
	}
	// PostgreSQL auto-starts on container boot, so the primary job is to WAIT for
	// readiness rather than to start it (an explicit start would race the boot
	// unit). Nudge only when the unit is genuinely down, tolerating a transient
	// start failure (e.g. the pgdata bind-mount not yet accessible) and retrying.
	i.log("waiting for PostgreSQL on %s to accept connections...", c)
	var last, logged string
	for attempt := 0; attempt < i.ReadyTries; attempt++ {
		if i.pgReady(ctx, c) {
			if attempt > 0 {
				i.log("PostgreSQL on %s is ready.", c)
			}
			return nil
		}
		last = i.pgUnitState(ctx, c)
		switch last {
		case "failed":
			if last != logged {
				i.log("PostgreSQL unit on %s is failed; resetting and retrying its start...", c)
			}
			_ = i.run(ctx, "exec", c, "--", "systemctl", "reset-failed", i.Unit)
			_ = i.run(ctx, "exec", c, "--", "systemctl", "start", i.Unit)
		case "inactive":
			if last != logged {
				i.log("PostgreSQL unit on %s is inactive; starting it...", c)
			}
			_ = i.run(ctx, "exec", c, "--", "systemctl", "start", i.Unit)
			// active/activating/deactivating: PG is coming up on its own — just wait.
		default:
			if attempt > 0 && attempt%5 == 0 {
				i.log("still waiting for PostgreSQL on %s (unit: %s, %ds elapsed)...", c, last, attempt)
			}
		}
		logged = last
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(i.ReadyEvery):
		}
	}
	return fmt.Errorf("PostgreSQL on %s not ready after %d attempts (unit state: %s)", c, i.ReadyTries, last)
}

func (i *Incus) StopContainer(ctx context.Context, c string) error {
	state, err := i.state(ctx, c)
	if err != nil {
		return err
	}
	if state == "" {
		return fmt.Errorf("container %s does not exist", c)
	}
	if state != "STOPPED" {
		if err := i.run(ctx, "stop", c); err != nil {
			return err
		}
	}
	if state, _ = i.state(ctx, c); state != "STOPPED" {
		return fmt.Errorf("container %s is %s, not STOPPED; refusing to touch its data", c, state)
	}
	return nil
}

func (i *Incus) StopContainerForce(ctx context.Context, c string) error {
	state, err := i.state(ctx, c)
	if err != nil {
		return err
	}
	if state == "" || state == "STOPPED" {
		return nil
	}
	return i.run(ctx, "stop", c, "--force")
}

func (i *Incus) StartContainerAndWait(ctx context.Context, c string) error {
	running, err := i.ContainerRunning(ctx, c)
	if err != nil {
		return err
	}
	if !running {
		i.log("starting container %s...", c)
		if err := i.run(ctx, "start", c); err != nil {
			return err
		}
		// After a fresh boot the container's systemd bus is not up for a moment;
		// issuing systemctl too early fails with "Failed to connect to bus".
		if err := i.waitSystemd(ctx, c); err != nil {
			return err
		}
	}
	err = i.EnsurePGRunning(ctx, c)
	if err == nil {
		return nil
	}
	// Some container boots come up with the pgdata bind-mount not yet accessible,
	// so PostgreSQL can never start on that boot; a full container restart
	// reliably clears it (observed on this setup). Retry the whole bring-up once.
	i.log("PostgreSQL did not come up on %s (%v); restarting the container once and retrying...", c, err)
	if rerr := i.run(ctx, "restart", c); rerr != nil {
		return errors.Join(err, rerr)
	}
	if werr := i.waitSystemd(ctx, c); werr != nil {
		return werr
	}
	return i.EnsurePGRunning(ctx, c)
}

// waitSystemd blocks until the container's system manager is reachable (any
// settled state), so subsequent systemctl calls don't race the bus coming up.
func (i *Incus) waitSystemd(ctx context.Context, c string) error {
	for attempt := 0; attempt < i.ReadyTries; attempt++ {
		if attempt == 3 {
			i.log("waiting for systemd in %s to become reachable...", c)
		}
		// is-system-running exits non-zero for "degraded" etc. but still prints a
		// state word; only a bus-connect failure prints none. Parse the text.
		out, _ := exec.CommandContext(ctx, "incus", "exec", c, "--", "systemctl", "is-system-running").CombinedOutput()
		switch strings.TrimSpace(string(out)) {
		case "running", "degraded", "starting", "maintenance":
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(i.ReadyEvery):
		}
	}
	return fmt.Errorf("systemd in %s did not become reachable", c)
}

func (i *Incus) RepairIP(ctx context.Context, c string) error {
	ip, err := i.backendIP(ctx, c)
	if err != nil {
		return err
	}
	if ip == "" {
		return nil // no pin configured/derivable; leave networking as-is
	}

	// Pin the address only if it's missing or wrong. `override` creates the
	// instance-local device over the profile-inherited NIC; once it's already
	// local (e.g. pinned at provisioning), `override` fails and `set` is the
	// right verb. Idempotent: a correct pin touches nothing.
	have := ""
	if out, e := i.output(ctx, "config", "device", "get", c, "eth0", "ipv4.address"); e == nil {
		have = strings.TrimSpace(out)
	}
	if have != ip {
		i.log("pinning %s eth0 -> %s (was %q)...", c, ip, have)
		if err := i.run(ctx, "config", "device", "override", c, "eth0", "ipv4.address="+ip); err != nil {
			if err := i.run(ctx, "config", "device", "set", c, "eth0", "ipv4.address="+ip); err != nil {
				return err
			}
		}
	}

	// A running backend on the wrong address needs a restart to take the new
	// static lease; a stopped one picks it up on next start (the restore path).
	running, err := i.ContainerRunning(ctx, c)
	if err != nil {
		return err
	}
	if running {
		if actual, _ := i.containerIP(ctx, c); actual != ip {
			i.log("restarting %s to apply %s...", c, ip)
			if err := i.run(ctx, "restart", c); err != nil {
				return err
			}
			for attempt := 0; attempt < i.ReadyTries; attempt++ {
				if a, _ := i.containerIP(ctx, c); a == ip {
					break
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(i.ReadyEvery):
				}
			}
		}
	}
	return nil
}

// containerIP returns the container's first IPv4 address, or "" if none.
func (i *Incus) containerIP(ctx context.Context, c string) (string, error) {
	out, err := i.output(ctx, "list", c, "--format", "csv", "-c", "4")
	if err != nil {
		return "", err
	}
	return ipv4re.FindString(out), nil
}

// backendIP resolves a container's pinned nested-bridge address: an explicit
// PG_BACKEND_{A,B}_IP override, else <incusbr0 prefix>.11/.12 (mirrors the
// shell's _pick_backend_a_ip / _incusbr0_prefix).
func (i *Incus) backendIP(ctx context.Context, c string) (string, error) {
	slot, err := i.Cfg.SlotForContainer(c)
	if err != nil {
		return "", err
	}
	if ip := i.Cfg.BackendIP(slot); ip != "" {
		return ip, nil
	}
	prefix, err := i.NetworkIPv4Prefix(ctx)
	if err != nil {
		return "", err
	}
	host := "11"
	if slot == "b" {
		host = "12"
	}
	return prefix + "." + host, nil
}

// NetworkIPv4Prefix returns the /24 prefix of incusbr0 (e.g. "10.114.221" from
// "10.114.221.1/24"), mirroring the shell's _incusbr0_prefix. It is the source
// for the derived backend addresses when PG_BACKEND_{A,B}_IP is unset.
func (i *Incus) NetworkIPv4Prefix(ctx context.Context) (string, error) {
	out, err := i.output(ctx, "network", "get", "incusbr0", "ipv4.address")
	if err != nil {
		return "", err
	}
	addr := strings.TrimSpace(out) // e.g. "10.114.221.1/24"
	if addr == "" || addr == "none" {
		return "", fmt.Errorf("cannot read incusbr0 ipv4.address; set PG_BACKEND_{A,B}_IP in .env")
	}
	if slash := strings.IndexByte(addr, '/'); slash >= 0 {
		addr = addr[:slash]
	}
	dot := strings.LastIndexByte(addr, '.')
	if dot < 0 {
		return "", fmt.Errorf("cannot parse incusbr0 address %q", addr)
	}
	return addr[:dot], nil
}

// ----- status (used by the daemon's /v1/status) ----------------------------

// Info returns a container's state and its global-scope IPv4/IPv6 addresses in
// one `incus list` call, mirroring _status_row. state is "" when the container
// does not exist.
func (i *Incus) Info(ctx context.Context, c string) (state string, ips []string, err error) {
	out, err := i.output(ctx, "list", c, "--format", "json")
	if err != nil {
		return "", nil, err
	}
	var insts []struct {
		Status string `json:"status"`
		State  struct {
			Network map[string]struct {
				Addresses []struct {
					Address string `json:"address"`
					Scope   string `json:"scope"`
				} `json:"addresses"`
			} `json:"network"`
		} `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &insts); err != nil {
		return "", nil, fmt.Errorf("parse incus list json: %w", err)
	}
	if len(insts) == 0 {
		return "", nil, nil
	}
	state = strings.ToUpper(insts[0].Status)
	for iface, n := range insts[0].State.Network {
		if iface == "lo" {
			continue
		}
		for _, a := range n.Addresses {
			if a.Scope == "global" {
				ips = append(ips, a.Address)
			}
		}
	}
	sort.Strings(ips)
	return state, ips, nil
}

// Version returns the `incus version` text (client + server), best-effort.
func (i *Incus) Version(ctx context.Context) string {
	out, _ := i.output(ctx, "version")
	return strings.TrimSpace(out)
}

// ----- control plane (used by internal/reconcile) --------------------------

// Exists reports whether a container is provisioned (`incus info` succeeds).
func (i *Incus) Exists(ctx context.Context, c string) bool {
	return i.run(ctx, "info", c) == nil
}

// State returns a container's lifecycle state (RUNNING/STOPPED/…), or "" if it
// does not exist. Exported wrapper over the internal helper for reconcile.
func (i *Incus) State(ctx context.Context, c string) (string, error) {
	return i.state(ctx, c)
}

// SetProxyDevice adds-or-re-points one proxy device on the proxy container,
// porting the shell's _ensure_proxy_device. bind=host puts the listener on the
// Apple machine's eth0. A device left over from the Colima layout (bind=instance)
// is replaced in place. Idempotent: re-pointing to the same target is harmless.
func (i *Incus) SetProxyDevice(ctx context.Context, proxy, dev, listen, connect string) error {
	bind := ""
	if out, err := i.output(ctx, "config", "device", "get", proxy, dev, "bind"); err == nil {
		bind = strings.TrimSpace(out)
	}
	if bind == "host" {
		return i.run(ctx, "config", "device", "set", proxy, dev,
			"listen="+listen, "connect="+connect)
	}
	_ = i.run(ctx, "config", "device", "remove", proxy, dev) // ignore "not found"
	i.log("adding proxy %s:%s → %s...", proxy, dev, connect)
	return i.run(ctx, "config", "device", "add", proxy, dev, "proxy",
		"listen="+listen, "connect="+connect, "bind=host")
}

// ----- helpers -------------------------------------------------------------

func (i *Incus) state(ctx context.Context, c string) (string, error) {
	out, err := i.output(ctx, "list", c, "--format", "csv", "-c", "s")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (i *Incus) pgReady(ctx context.Context, c string) bool {
	return i.run(ctx, "exec", c, "--", "pg_isready", "-q") == nil
}

// pgUnitState returns the PostgreSQL unit's activation-state word (active,
// inactive, failed, activating, …), or "" if the bus can't be reached.
func (i *Incus) pgUnitState(ctx context.Context, c string) string {
	out, _ := exec.CommandContext(ctx, "incus", "exec", c, "--", "systemctl", "is-active", i.Unit).CombinedOutput()
	return strings.TrimSpace(string(out))
}

func (i *Incus) run(ctx context.Context, args ...string) error {
	if i.runFn != nil {
		return i.runFn(ctx, args...)
	}
	cmd := exec.CommandContext(ctx, "incus", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if stderr.Len() > 0 {
			return fmt.Errorf("incus %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return fmt.Errorf("incus %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func (i *Incus) output(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "incus", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("incus %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}
