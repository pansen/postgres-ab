package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"pansen.me/pgdev/internal/config"
)

// Incus drives backends by shelling out to the `incus` CLI over the machine's
// local Incus socket. Slice 1 keeps the shell transport (it matches the current
// script exactly); a later slice replaces this file with the typed Incus Go
// client behind the same Backend interface.
type Incus struct {
	Cfg        config.Config
	Unit       string        // PostgreSQL systemd unit
	ReadyTries int           // pg_isready attempts
	ReadyEvery time.Duration // delay between attempts

	// runFn overrides the `incus` invocation in tests (nil = shell out for real).
	runFn func(ctx context.Context, args ...string) error
}

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
	active, err := i.PGActive(ctx, c)
	if err != nil {
		return err
	}
	if !active {
		if err := i.run(ctx, "exec", c, "--", "systemctl", "start", i.Unit); err != nil {
			return err
		}
	}
	return i.waitReady(ctx, c)
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
		if err := i.run(ctx, "start", c); err != nil {
			return err
		}
	}
	// A container boot does not necessarily start PostgreSQL (its systemd unit is
	// not reliably enabled), so bring PG up explicitly and wait for readiness
	// instead of assuming the boot did it.
	return i.EnsurePGRunning(ctx, c)
}

func (i *Incus) RepairIP(ctx context.Context, c string) error {
	ip, err := i.backendIP(ctx, c)
	if err != nil {
		return err
	}
	if ip == "" {
		return nil // no pin configured/derivable; leave networking as-is
	}
	return i.run(ctx, "config", "device", "override", c, "eth0", "ipv4.address="+ip)
}

// backendIP resolves a container's pinned nested-bridge address: an explicit
// PG_BACKEND_{A,B}_IP override, else <incusbr0 prefix>.11/.12 (mirrors the
// shell's _pick_backend_a_ip / _incusbr0_prefix).
func (i *Incus) backendIP(ctx context.Context, c string) (string, error) {
	slot, err := i.Cfg.SlotForContainer(c)
	if err != nil {
		return "", err
	}
	if ip := i.Cfg.BackendIP[slot]; ip != "" {
		return ip, nil
	}
	out, err := i.output(ctx, "network", "get", "incusbr0", "ipv4.address")
	if err != nil {
		return "", err
	}
	addr := strings.TrimSpace(out) // e.g. "10.114.221.1/24"
	if slash := strings.IndexByte(addr, '/'); slash >= 0 {
		addr = addr[:slash]
	}
	dot := strings.LastIndexByte(addr, '.')
	if dot < 0 {
		return "", fmt.Errorf("cannot parse incusbr0 address %q", addr)
	}
	host := "11"
	if slot == "b" {
		host = "12"
	}
	return addr[:dot+1] + host, nil
}

// ----- helpers -------------------------------------------------------------

func (i *Incus) state(ctx context.Context, c string) (string, error) {
	out, err := i.output(ctx, "list", c, "--format", "csv", "-c", "s")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (i *Incus) waitReady(ctx context.Context, c string) error {
	for attempt := 0; attempt < i.ReadyTries; attempt++ {
		if err := i.run(ctx, "exec", c, "--", "pg_isready", "-q"); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(i.ReadyEvery):
		}
	}
	return fmt.Errorf("PostgreSQL on %s not ready after %d attempts", c, i.ReadyTries)
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
