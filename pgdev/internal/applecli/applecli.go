// Package applecli is the ONLY place the host shells out to Apple's `container`
// CLI. Apple's container runtime is Swift + XPC with no REST API, so machine
// lifecycle and the machine's eth0 address stay CLI-driven (§2, §5.10 of
// issues/0001). The surface is deliberately tiny: everything else reaches the
// machine over the resident HTTP API instead of `container machine run` execs.
package applecli

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

var ipv4re = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)

// CLI drives one Apple container machine by name.
type CLI struct {
	Machine string // MACHINE_NAME, e.g. vpg
	// run overrides the `container` invocation in tests (nil = exec for real).
	run func(ctx context.Context, stdin string, args ...string) (string, error)
}

func New(machine string) *CLI { return &CLI{Machine: machine} }

// Exists reports whether the machine has been created.
func (c *CLI) Exists(ctx context.Context) bool {
	_, err := c.exec(ctx, "", "machine", "inspect", c.Machine)
	return err == nil
}

// MachineIP reads the machine's current eth0 IPv4 by execing into it. This is a
// fallback discovery path (the daemon also pushes the IP to var/machine-ip);
// it is NOT on the stateful control path. Empty string if the machine is down
// or has no address yet.
func (c *CLI) MachineIP(ctx context.Context) (string, error) {
	out, err := c.exec(ctx, "", "machine", "run", "--name", c.Machine, "--root", "--",
		"ip", "-4", "-o", "addr", "show", "dev", "eth0")
	if err != nil {
		return "", err
	}
	// "2: eth0 inet 192.168.64.5/24 ..." → first IPv4.
	return ipv4re.FindString(out), nil
}

var systemdStates = map[string]bool{"running": true, "degraded": true, "starting": true, "maintenance": true}

// WaitReady blocks until the machine's systemd is up enough to run systemctl.
// This clears two early-boot races at once: right after a freshly created
// machine's first stopped→running transition Apple rejects execs with "Operation
// not supported by device", and even once execs work the system bus isn't
// listening yet (systemctl fails with "Failed to connect to system scope bus").
// `systemctl is-system-running` prints a settled state word to stdout only once
// the bus answers (even "degraded", which is this guest's steady state and exits
// non-zero) — so a printed state word is the reliable readiness signal.
func (c *CLI) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		out, _ := c.Run(ctx, "systemctl", "is-system-running") // ignore exit code; read stdout
		if systemdStates[strings.TrimSpace(out)] {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("machine %s systemd not ready within %s (last: %q)", c.Machine, timeout, strings.TrimSpace(out))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// Run executes argv inside the machine as root and returns combined output. Used
// by `pgdev agent deploy` for the handful of one-shot install/restart execs that
// stand the daemon up — Apple's transport handles single non-interactive execs
// fine; it is the resident control path that must not depend on them.
func (c *CLI) Run(ctx context.Context, argv ...string) (string, error) {
	return c.RunStdin(ctx, "", argv...)
}

// RunStdin is Run with data piped to the command's stdin (e.g. an install
// script). Apple keeps stdin open for the session, so callers that don't need
// it should pass "" and the command should not read stdin.
func (c *CLI) RunStdin(ctx context.Context, stdin string, argv ...string) (string, error) {
	args := append([]string{"machine", "run", "--name", c.Machine, "--root", "--"}, argv...)
	return c.exec(ctx, stdin, args...)
}

func (c *CLI) exec(ctx context.Context, stdin string, args ...string) (string, error) {
	if c.run != nil {
		return c.run(ctx, stdin, args...)
	}
	cmd := exec.CommandContext(ctx, "container", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		if errb.Len() > 0 {
			return out.String(), fmt.Errorf("container %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
		}
		return out.String(), fmt.Errorf("container %s: %w", strings.Join(args, " "), err)
	}
	return out.String(), nil
}
