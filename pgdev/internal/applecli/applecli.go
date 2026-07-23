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
	"strconv"
	"strings"
	"sync"
	"time"
)

var ipv4re = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)

// execMu serializes every real `container` invocation across ALL CLI instances.
// Apple 1.1's exec/XPC transport is fragile under concurrency — the Slice-0
// smoke test proved that delete/recreate of one machine is safe for a sibling
// ONLY when lifecycle execs are strictly serialized; concurrent execs have taken
// a VM down before (spec §4, memory apple-container-cli-quirks). Each invocation
// holds the lock only for its own duration and releases before any retry sleep,
// so a boot/ready poll on one machine still interleaves with the sibling between
// individual calls — but two `container` processes never run at once.
var execMu sync.Mutex

// CLI drives one Apple container machine by name.
type CLI struct {
	Machine string // MACHINE_NAME, e.g. vpg-a
	// run overrides the `container` invocation in tests (nil = exec for real).
	run func(ctx context.Context, stdin string, args ...string) (string, error)
	// pollInterval overrides the retry cadence of WaitReady/Boot in tests
	// (0 = use each method's real default).
	pollInterval time.Duration
}

func New(machine string) *CLI { return &CLI{Machine: machine} }

// pollEvery is the retry cadence for readiness loops, overridable in tests.
func (c *CLI) pollEvery(def time.Duration) time.Duration {
	if c.pollInterval > 0 {
		return c.pollInterval
	}
	return def
}

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
		case <-time.After(c.pollEvery(2 * time.Second)):
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

// ----- machine lifecycle (host-orchestrated hard reset) --------------------
// A daemon cannot delete/recreate its own Apple machine, so these host-side
// verbs drive the two-machine model's hard-reset tier (spec §2, §5): delete a
// staging machine to reclaim its sparse vdb on macOS, then recreate + reboot it.
// They mirror the Makefile's `delete`/`machine` targets and the smoke test's
// create_boot exactly, including tolerate-and-verify delete semantics.

// CreateOpts are the resources for a freshly created machine.
type CreateOpts struct {
	CPUs   int    // e.g. 4
	Memory string // e.g. "8G"
	Image  string // e.g. local/pg-incus-machine:26.04
}

// Create makes the machine (Apple leaves it stopped; call Boot to start it).
// It is NOT --set-default: with two machines there is no meaningful default.
func (c *CLI) Create(ctx context.Context, o CreateOpts) error {
	_, err := c.exec(ctx, "", "machine", "create",
		"--name", c.Machine,
		"--cpus", strconv.Itoa(o.CPUs),
		"--memory", o.Memory,
		"--home-mount", "rw",
		o.Image)
	return err
}

// Stop stops the machine, tolerating errors (used before Delete).
func (c *CLI) Stop(ctx context.Context) error {
	_, err := c.exec(ctx, "", "machine", "stop", c.Machine)
	return err
}

// Delete stops then deletes the machine and verifies it is actually gone.
// Apple 1.1 frequently returns an XPC timeout when deleting a RUNNING machine
// even though the delete completes and the apiserver then drops the entry — so
// success is judged by absence, not exit code (mirrors the Makefile `delete`
// target). A no-op if the machine does not exist.
func (c *CLI) Delete(ctx context.Context) error {
	if !c.Exists(ctx) {
		return nil
	}
	_, _ = c.exec(ctx, "", "machine", "stop", c.Machine)   // documented workaround; tolerate
	_, _ = c.exec(ctx, "", "machine", "delete", c.Machine) // completes despite XPC timeout; tolerate
	if c.Exists(ctx) {
		return fmt.Errorf("machine %s still present after delete — the Apple apiserver may be wedged (retry, or reset with `container system stop && container system start`)", c.Machine)
	}
	return nil
}

// Boot starts a freshly created (or stopped) machine and blocks until it is
// ready to serve execs. It clears the two Apple early-boot races in order: first
// the stopped→running transition, which must be a non-interactive exec and which
// Apple rejects with "Operation not supported by device" until the machine
// actually accepts it (retry `run -- true` until it succeeds); then the system
// bus coming up (WaitReady). The retry sleeps happen with the exec lock released,
// so a sibling machine's calls still interleave between attempts.
func (c *CLI) Boot(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := c.Run(ctx, "true"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("machine %s never became execable within %s", c.Machine, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(c.pollEvery(3 * time.Second)):
		}
	}
	return c.WaitReady(ctx, time.Until(deadline))
}

// Recreate is the hard-reset primitive: delete the machine (reclaiming its
// sparse disk on macOS), create it fresh, and boot it ready. The caller
// re-provisions the backend on top afterwards.
func (c *CLI) Recreate(ctx context.Context, o CreateOpts, bootTimeout time.Duration) error {
	if err := c.Delete(ctx); err != nil {
		return err
	}
	if err := c.Create(ctx, o); err != nil {
		return err
	}
	return c.Boot(ctx, bootTimeout)
}

func (c *CLI) exec(ctx context.Context, stdin string, args ...string) (string, error) {
	if c.run != nil {
		return c.run(ctx, stdin, args...)
	}
	execMu.Lock()
	defer execMu.Unlock()
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
