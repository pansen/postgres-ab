package backend

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"pansen.me/pgdev/internal/config"
)

// This file holds the Incus control primitives provisioning composes (§5.3 of
// issues/0001). They map 1:1 to `incus` CLI verbs; the orchestration (order,
// idempotency, the golden-image decision) lives in internal/pg and the daemon.
// The old `incus launch … </dev/null` stdin hack is gone: exec.Command gives a
// closed stdin, so `incus launch` never blocks waiting for YAML.

// Launch creates and starts a container from an image (idempotent: a no-op if it
// already exists).
func (i *Incus) Launch(ctx context.Context, name, image string) error {
	if i.Exists(ctx, name) {
		return nil
	}
	i.log("launching %s from %s...", name, image)
	return i.run(ctx, "launch", image, name)
}

// Delete force-deletes a container (idempotent: absent → no-op).
func (i *Incus) Delete(ctx context.Context, name string) error {
	if !i.Exists(ctx, name) {
		return nil
	}
	return i.run(ctx, "delete", name, "--force")
}

// HasDevice reports whether a container has a named device.
func (i *Incus) HasDevice(ctx context.Context, name, dev string) bool {
	return i.run(ctx, "config", "device", "get", name, dev, "type") == nil
}

// AddDiskDevice bind-mounts an idmapped host directory into the container
// (shift=true), used to attach a slot's data dir at /var/lib/postgresql.
func (i *Incus) AddDiskDevice(ctx context.Context, name, dev, source, path string) error {
	if i.HasDevice(ctx, name, dev) {
		return nil
	}
	return i.run(ctx, "config", "device", "add", name, dev, "disk",
		"source="+source, "path="+path, "shift=true")
}

// SetEth0Static pins the container's eth0 to a static address by overriding the
// profile-inherited NIC (falling back to `set` once it is instance-local).
func (i *Incus) SetEth0Static(ctx context.Context, name, ip string) error {
	if err := i.run(ctx, "config", "device", "override", name, "eth0", "ipv4.address="+ip); err != nil {
		return i.run(ctx, "config", "device", "set", name, "eth0", "ipv4.address="+ip)
	}
	return nil
}

// Restart restarts a container.
func (i *Incus) Restart(ctx context.Context, name string) error {
	return i.run(ctx, "restart", name)
}

// WaitIPv4 blocks until the container reports an IPv4 on eth0 — any address, or
// the specific expected one when given (ports _wait_for_ipv4).
func (i *Incus) WaitIPv4(ctx context.Context, name, expected string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		ip, _ := i.containerIP(ctx, name)
		if expected == "" && ip != "" {
			return nil
		}
		if expected != "" && ip == expected {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not get IPv4 %s within %s (have %q)", name, orAny(expected), timeout, ip)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// ImageExists reports whether a local image alias is present.
func (i *Incus) ImageExists(ctx context.Context, alias string) bool {
	return i.run(ctx, "image", "info", alias) == nil
}

// Publish snapshots a STOPPED container's rootfs into a local image alias (the
// golden pg-dev-base). Bind-mounted data (/var/lib/postgresql on the XFS slot)
// is NOT part of the rootfs, so only PG binaries + /etc config are captured.
func (i *Incus) Publish(ctx context.Context, name, alias string) error {
	// Replace any stale alias so a re-publish is idempotent.
	_ = i.run(ctx, "image", "delete", alias)
	i.log("publishing %s as image %s...", name, alias)
	return i.run(ctx, "publish", name, "--alias", alias)
}

// ExecScript runs a bash script inside a container over `incus exec` (a nested
// call on the machine's own socket — none of Apple's exec quirks apply here) and
// returns its combined output.
func (i *Incus) ExecScript(ctx context.Context, name, script string) (string, error) {
	cmd := exec.CommandContext(ctx, "incus", "exec", name, "--", "bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("exec in %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// EnsureTopology configures the Incus daemon's storage pool, bridge network and
// default profile devices once, without disturbing an already-initialized
// daemon (ports apple-machine-init:188-204). Called by pgdevd bootstrap.
func (i *Incus) EnsureTopology(ctx context.Context, cfg config.Config) error {
	if !i.storageExists(ctx, "default") {
		if err := i.run(ctx, "storage", "create", "default", "dir"); err != nil {
			return err
		}
	}
	if !i.networkExists(ctx, "incusbr0") {
		if err := i.run(ctx, "network", "create", "incusbr0",
			"ipv4.address=auto", "ipv4.nat=true", "ipv6.address=auto", "ipv6.nat=true"); err != nil {
			return err
		}
	}
	if i.run(ctx, "profile", "device", "get", "default", "root", "path") != nil {
		if err := i.run(ctx, "profile", "device", "add", "default", "root", "disk", "path=/", "pool=default"); err != nil {
			return err
		}
	}
	if i.run(ctx, "profile", "device", "get", "default", "eth0", "network") != nil {
		if err := i.run(ctx, "profile", "device", "add", "default", "eth0", "nic", "network=incusbr0", "name=eth0"); err != nil {
			return err
		}
	}
	return nil
}

// RepairNetworks re-creates managed bridges that Incus knows about but whose
// interface is missing from the kernel. An incusd that started against a
// read-only /proc/sys cannot write net.ipv6.conf.<bridge>.disable_ipv6 and so
// leaves its bridge defined-but-UNAVAILABLE; every instance on it then fails to
// start. Incus has no per-network "start" verb — only a daemon restart re-runs
// the setup — so this restarts incusd, but only when a bridge is actually
// missing (i.e. once, on the boot that first remounted /proc/sys read-write).
//
// The restart is issued --no-block: pgdevd is ordered After=incus.service, so a
// blocking restart from its ExecStartPre could deadlock against its own start
// job. The post-condition waited on is therefore the interface appearing, not
// the systemd job finishing.
func (i *Incus) RepairNetworks(ctx context.Context, timeout time.Duration) error {
	missing, err := i.missingBridges(ctx)
	if err != nil {
		return err
	}
	if len(missing) == 0 {
		return nil
	}
	i.log("Incus bridge(s) %s are defined but absent; restarting incusd to re-create them...",
		strings.Join(missing, ", "))
	if out, err := exec.CommandContext(ctx, "systemctl", "restart", "--no-block", "incus.service").CombinedOutput(); err != nil {
		return fmt.Errorf("restart incus.service: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// Wait for the restart to actually land. A query error here means incusd is
	// mid-restart (the shutting-down daemon answers "database is closed"), which
	// is a "keep waiting", NOT a "nothing is missing" — reading it as the latter
	// would return success and hand the next caller a daemon that is still going
	// down.
	deadline := time.Now().Add(timeout)
	for {
		switch left, err := i.missingBridges(ctx); {
		case err != nil: // incusd not answering yet
		case len(left) == 0:
			i.log("Incus bridge(s) %s are back.", strings.Join(missing, ", "))
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Incus bridge(s) %s did not come back within %s after restarting incusd",
				strings.Join(missing, ", "), timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// missingBridges lists the managed bridge networks with no kernel interface. The
// error is the caller's signal that the daemon could not be asked at all, which
// is never the same answer as "none are missing".
func (i *Incus) missingBridges(ctx context.Context) ([]string, error) {
	out, err := i.output(ctx, "network", "list", "--format", "csv", "-c", "ntm")
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, name := range managedBridges(out) {
		if _, err := os.Stat("/sys/class/net/" + name); err != nil {
			missing = append(missing, name)
		}
	}
	return missing, nil
}

// managedBridges picks the Incus-managed bridges out of
// `incus network list --format csv -c ntm` (name,type,managed). Unmanaged
// entries are the machine's own interfaces (eth0, lo) — Incus never creates
// those, so their presence is not its business.
func managedBridges(csv string) []string {
	var out []string
	for _, line := range strings.Split(csv, "\n") {
		f := strings.Split(strings.TrimSpace(line), ",")
		if len(f) < 3 || f[1] != "bridge" || !strings.EqualFold(f[2], "YES") {
			continue
		}
		out = append(out, f[0])
	}
	return out
}

// WaitReady blocks until the local Incus daemon answers (ports `incus admin
// waitready`), so bootstrap doesn't race incusd's first-start initialization.
func (i *Incus) WaitReady(ctx context.Context, timeout time.Duration) error {
	secs := int(timeout.Seconds())
	if secs < 1 {
		secs = 1
	}
	return i.run(ctx, "admin", "waitready", "--timeout", fmt.Sprintf("%d", secs))
}

func (i *Incus) storageExists(ctx context.Context, name string) bool {
	out, err := i.output(ctx, "storage", "list", "--format", "csv", "-c", "n")
	return err == nil && hasLine(out, name)
}

func (i *Incus) networkExists(ctx context.Context, name string) bool {
	out, err := i.output(ctx, "network", "list", "--format", "csv", "-c", "n")
	return err == nil && hasLine(out, name)
}

func hasLine(out, want string) bool {
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == want {
			return true
		}
	}
	return false
}

func orAny(s string) string {
	if s == "" {
		return "(any)"
	}
	return s
}
